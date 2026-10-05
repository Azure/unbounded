// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

type transferDiscard struct{ *httptest.ResponseRecorder }

func (transferDiscard) ReadFrom(r io.Reader) (int64, error) { return io.Copy(io.Discard, r) }

func TestBodyReadTimeoutReleasesAdmission(t *testing.T) {
	for _, fast := range []bool{false, true} {
		name := "copy"
		if fast {
			name = "HTTP transfer"
		}

		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponseHead(w, 0, 8192, 8192, `"v"`)
				w.(http.Flusher).Flush()
				<-done
			}))

			defer close(done)

			c := testClient(t, path, 1)
			c.config.BodyReadTimeout = 30 * time.Millisecond

			v, err := c.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			if fast {
				_, err = v.WriteToHTTP(transferDiscard{httptest.NewRecorder()})
			} else {
				_, err = io.Copy(io.Discard, v)
			}

			if err == nil || c.Stats().ActiveBulk != 0 {
				t.Fatal("stalled body retained admission", err, c.Stats())
			}
		})
	}
}

func TestBodyReadTimeoutDoesNotBoundCallerThinkTime(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, 2, 2, `"v"`)
		_, _ = w.Write([]byte("ok"))
	}))
	c := testClient(t, path, 1)
	c.config.BodyReadTimeout = 20 * time.Millisecond

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	buf := make([]byte, 1)
	if _, err := v.Read(buf); err != nil {
		t.Fatal(err)
	}

	time.Sleep(40 * time.Millisecond)

	if _, err := v.Read(buf); err != nil || string(buf) != "k" {
		t.Fatal(string(buf), err)
	}
}

func TestStreamingReadAhead(t *testing.T) {
	// Deliver headers, payload, and Complete in a single read-ahead buffer.
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		var all bytes.Buffer
		all.WriteString(subscriptionHead(3, 0, 3))
		_ = fakeSubscriptionFrame(&all, 1, 0, 0, 3)
		all.WriteString("abc")
		_ = fakeSubscriptionFrame(&all, 2, 1, 3, 0)
		_, _ = conn.Write(all.Bytes())

		orderedRelease(t, reader, 0, 3)
	})

	v, err := c.GetStreaming(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if v.stream.conn.Reader.Buffered() != 45 {
		t.Fatal("fixture did not read ahead")
	}

	dst := httptest.NewRecorder()
	if n, err := v.WriteToHTTP(dst); n != 3 || err != nil || dst.Body.String() != "abc" {
		t.Fatal(n, err, dst.Body.String())
	}

	if c.Stats().BytesRead != 3 {
		t.Fatal(c.Stats())
	}
}

func TestStreamingShortWritesAndBodyDeadline(t *testing.T) {
	for _, mode := range []string{"short", "negative", "excess", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))

				_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
				if mode != "deadline" {
					_, _ = io.WriteString(conn, "abc")
				}

				_, _ = io.Copy(io.Discard, reader)
			})
			c.config.BodyReadTimeout = 30 * time.Millisecond

			v, err := c.GetStreaming(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			n, err := v.WriteToHTTP(streamingWriter{httptest.NewRecorder(), func(p []byte) (int, error) {
				switch mode {
				case "negative":
					return -1, nil
				case "excess":
					return len(p) + 1, nil
				}

				return 0, nil
			}})
			if n != 0 {
				t.Fatal(n)
			}

			if mode == "deadline" {
				assertKind(t, err, ErrorIO)

				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatal("missing socket timeout", err)
				}
			} else if !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("admission leaked")
			}
		})
	}
}

func orderedWait(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ordered receiver did not reach barrier")
	}
}

func orderedRelease(t *testing.T, reader io.Reader, number uint64, length uint32) bool {
	t.Helper()

	var frame [12]byte
	if _, err := io.ReadFull(reader, frame[:]); err != nil {
		t.Error(err)
		return false
	}

	if binary.BigEndian.Uint64(frame[:8]) != number || binary.BigEndian.Uint32(frame[8:]) != length {
		t.Errorf("release = %x, want page %d length %d", frame, number, length)
		return false
	}

	return true
}

func orderedClean(t *testing.T, v *Value) {
	t.Helper()
	closeBody(v)
	orderedWait(t, v.ordered.done)

	if len(v.ordered.slots) != 0 || len(v.ordered.ready) != 0 || len(v.stream.buffers) != 0 || v.ordered.lease != nil {
		t.Fatal("ordered storage retained after cleanup")
	}

	v.stream.mu.Lock()
	defer v.stream.mu.Unlock()

	if len(v.stream.outstanding) != 0 || v.stream.bytesHeld != 0 || v.client.Stats().ActiveBulk != 0 {
		t.Fatal("ordered credits or admission retained")
	}
}

func TestGetReadAheadOverlapAndBufferLifetime(t *testing.T) {
	const (
		first = uint64(PageSize) - 2
		end   = 2*uint64(PageSize) + 3
	)

	secondReceived := make(chan struct{})
	allowFinal := make(chan struct{})
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
		if headHeaders(head).Get("Racer-Ordered") != "1" {
			t.Error("Get did not request ordering")
		}

		_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
		_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
		_, _ = io.WriteString(conn, "ab")

		_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), uint32(PageSize))
		if _, err := io.CopyN(conn, repeatedByte('m'), int64(PageSize)); err != nil {
			return
		}

		close(secondReceived)

		if !orderedRelease(t, reader, 0, 2) {
			return
		}

		select {
		case <-allowFinal:
		case <-t.Context().Done():
			return
		}

		_ = fakeSubscriptionFrame(conn, 1, 2, 2*uint64(PageSize), 3)
		_, _ = io.WriteString(conn, "xyz")
		_ = fakeSubscriptionFrame(conn, 2, 3, end-first, 0)
	})

	v, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), Length: ByteLength(end - first), PageCredits: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	var b [1]byte
	if n, err := v.Read(b[:]); n != 1 || err != nil || b[0] != 'a' {
		t.Fatal(n, err, b)
	}

	firstBuffer := &v.ordered.lease.Data[0]

	orderedWait(t, secondReceived) // net.Pipe cannot complete this write without receiving.

	if string(v.ordered.lease.Data) != "ab" || len(v.ordered.slots) != 2 || cap(v.ordered.slots) != 2 {
		t.Fatal("held page changed or read-ahead exceeded two buffers")
	}

	if n, err := v.Read(b[:]); n != 1 || err != nil || b[0] != 'b' {
		t.Fatal(n, err, b)
	}

	if n, err := v.Read(b[:]); n != 1 || err != nil || b[0] != 'm' {
		t.Fatal(n, err, b)
	}

	if &v.ordered.lease.Data[0] == firstBuffer {
		t.Fatal("second receive reused a still-leased buffer")
	}

	close(allowFinal)
	orderedWait(t, v.ordered.done)

	if c.Stats().ActiveBulk != 1 || c.Stats().Dials != 1 {
		t.Fatal("wire completion returned admission prematurely", c.Stats())
	}

	if n, err := io.CopyN(io.Discard, v, int64(PageSize)-1); n != int64(PageSize)-1 || err != nil {
		t.Fatal(n, err)
	}

	if n, err := v.Read(b[:]); n != 1 || err != nil || b[0] != 'x' {
		t.Fatal(n, err, b)
	}

	if &v.ordered.lease.Data[0] != firstBuffer || cap(v.ordered.lease.Data) != 3 || string(v.ordered.lease.Data) != "xyz" {
		t.Fatal("final slice did not safely reuse released storage")
	}

	rest, err := io.ReadAll(v)
	if err != nil || string(rest) != "yz" {
		t.Fatal(string(rest), err)
	}

	orderedClean(t, v)
}

func TestGetReadAheadSingleCredit(t *testing.T) {
	for _, options := range []ReadOptions{{PageCredits: 1}, {PageCredits: 64, ByteCredits: PageSize}} {
		const (
			first = uint64(PageSize) - 2
			end   = uint64(PageSize) + 3
		)

		c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
			_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
			_, _ = io.WriteString(conn, "ab")

			if !orderedRelease(t, reader, 0, 2) {
				return
			}

			_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
			_, _ = io.WriteString(conn, "xyz")
			_ = fakeSubscriptionFrame(conn, 2, 2, 5, 0)
		})
		options.Offset = ByteOffset(first)

		v, err := c.Get(t.Context(), Request{}, options)
		if err != nil {
			t.Fatal(err)
		}

		var b [1]byte
		if _, err := v.Read(b[:]); err != nil {
			t.Fatal(err)
		}

		if cap(v.ordered.slots) != 1 || len(v.ordered.slots) != 1 {
			t.Fatal("single credit did not bound resident buffers")
		}

		old := &v.ordered.lease.Data[0]
		if _, err := v.Read(b[:]); err != nil || b[0] != 'b' {
			t.Fatal(err, b)
		}

		if _, err := v.Read(b[:]); err != nil || b[0] != 'x' || &v.ordered.lease.Data[0] != old {
			t.Fatal("single buffer did not resume/reuse", err, b)
		}

		orderedClean(t, v)
	}
}

func TestGetReadAheadLateFailure(t *testing.T) {
	for _, mode := range []string{"short payload", "missing complete", "bad complete"} {
		for _, copyTo := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "read", true: "copy"}[copyTo], func(t *testing.T) {
				const (
					first = uint64(PageSize) - 2
					end   = uint64(PageSize) + 3
				)

				c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
					_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
					_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
					_, _ = io.WriteString(conn, "ab")

					_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
					if mode == "short payload" {
						_, _ = io.WriteString(conn, "x")
						return
					}

					_, _ = io.WriteString(conn, "xyz")
					if mode == "bad complete" {
						_ = fakeSubscriptionFrame(conn, 2, 2, 4, 0)
					}
				})

				v, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first)})
				if err != nil {
					t.Fatal(err)
				}
				defer closeBody(v)

				orderedWait(t, v.ordered.done)

				var dst bytes.Buffer

				if copyTo {
					var n int64

					n, err = v.WriteTo(&dst)
					if n != 2 {
						t.Fatal("wrong partial count", n)
					}
				} else {
					var data []byte

					data, err = io.ReadAll(v)
					dst.Write(data)
				}

				if dst.String() != "ab" || err == nil || err == io.EOF {
					t.Fatal("late failure lost prefix or exposed final page", dst.String(), err)
				}

				if mode == "bad complete" {
					assertKind(t, err, ErrorProtocol)
				} else if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}

				if _, again := v.Read(make([]byte, 1)); again != err {
					t.Fatal("error was not terminal", again)
				}

				orderedClean(t, v)
			})
		}
	}
}

func TestGetReadAheadCleanup(t *testing.T) {
	for _, state := range []string{"credit wait", "payload read", "complete queued"} {
		for _, action := range []string{"close", "cancel", "client"} {
			t.Run(state+"/"+action, func(t *testing.T) {
				const (
					first = uint64(PageSize) - 2
					end   = uint64(PageSize) + 3
				)

				blocked := make(chan struct{})
				c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
					_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
					_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)

					_, _ = io.WriteString(conn, "ab")
					if state != "credit wait" {
						_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
						if state == "complete queued" {
							_, _ = io.WriteString(conn, "xyz")
							_ = fakeSubscriptionFrame(conn, 2, 2, 5, 0)
						}
					}

					close(blocked)

					var frame [12]byte
					if _, err := io.ReadFull(reader, frame[:]); err == nil {
						t.Error("cleanup prematurely returned held credit")
					}
				})

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				o := ReadOptions{Offset: ByteOffset(first)}
				if state == "credit wait" {
					o.PageCredits = 1
				}

				v, err := c.Get(ctx, Request{}, o)
				if err != nil {
					t.Fatal(err)
				}

				if _, err := v.Read(make([]byte, 1)); err != nil {
					t.Fatal(err)
				}

				orderedWait(t, blocked)

				switch action {
				case "cancel":
					cancel()
					orderedWait(t, v.finished)
				case "client":
					closeBody(c)
				default:
					closeBody(v)
				}

				orderedClean(t, v)

				_, err = v.Read(make([]byte, 1))
				if action == "cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				} else {
					assertKind(t, err, ErrorClosed)
				}
			})
		}
	}
}

func TestGetReadAheadBlockedWriterCleanupAndRequestIsolation(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
		_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
		_, _ = io.WriteString(conn, "abc")
		_ = fakeSubscriptionFrame(conn, 2, 1, 3, 0)
	})

	v, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	var first [1]byte
	if _, err := v.Read(first[:]); err != nil {
		t.Fatal(err)
	}

	old := &v.ordered.lease.Data[0]
	entered := make(chan struct{})

	resume := make(chan struct{})
	defer close(resume)

	result := make(chan error, 1)

	go func() {
		_, err := v.WriteTo(writeFunc(func(p []byte) (int, error) {
			close(entered)
			<-resume

			if string(p) != "bc" {
				t.Error("blocked writer scratch changed")
			}

			return len(p), nil
		}))
		result <- err
	}()

	orderedWait(t, entered)
	orderedClean(t, v) // Must not wait for the application writer.

	next, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(next)

	if _, err := next.Read(first[:]); err != nil || first[0] != 'a' {
		t.Fatal(err, first)
	}

	if &next.ordered.lease.Data[0] == old {
		t.Fatal("payload storage crossed request boundaries")
	}

	orderedClean(t, next)
	// Release the writer separately so the deferred channel close stays unique.
	resume <- struct{}{}

	select {
	case err := <-result:
		assertKind(t, err, ErrorClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled writer did not finish after resumption")
	}
}

type orderedReleaseFailureConn struct{ net.Conn }

func (orderedReleaseFailureConn) SetWriteDeadline(time.Time) error {
	return errors.New("release deadline failure")
}

func TestGetReadAheadReleaseFailureCleanup(t *testing.T) {
	const (
		first = uint64(PageSize) - 2
		end   = uint64(PageSize) + 3
	)

	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
		_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
		_, _ = io.WriteString(conn, "ab")
		_, _ = io.Copy(io.Discard, reader)
	})
	poolConfig := c.bulk.Config()
	dial := poolConfig.Dial
	poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		return orderedReleaseFailureConn{conn}, err
	}
	c.configurePools(poolConfig)

	v, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), PageCredits: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	data, err := io.ReadAll(v)
	if string(data) != "ab" || err == nil || err == io.EOF {
		t.Fatal(string(data), err)
	}

	orderedClean(t, v)
}

type orderedCloseSignal struct {
	io.Closer
	closed chan struct{}
}

func (b orderedCloseSignal) Close() error {
	err := b.Closer.Close()
	close(b.closed)

	return err
}

func TestGetTerminalReadWaitsForReceiverCleanup(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
		_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
		_, _ = io.WriteString(conn, "abc")
		_ = fakeSubscriptionFrame(conn, 2, 1, 3, 0)
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	orderedWait(t, v.ordered.done)

	closed := make(chan struct{})

	v.mu.Lock()
	v.body = orderedCloseSignal{v.body, closed}
	v.mu.Unlock()
	// Hold cleanup at its lease lock while cancellation publishes the terminal
	// error and closes the connection. Read must join cleanup, not just err().
	v.ordered.mu.Lock()
	cancel()

	orderedWait(t, closed)

	result := make(chan error, 1)

	go func() {
		_, err := v.Read(make([]byte, 1))
		result <- err
	}()
	v.ordered.mu.Unlock()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal read did not join cleanup")
	}

	if c.Stats().ActiveBulk != 0 || len(v.stream.buffers) != 0 {
		t.Fatal("terminal read returned before cleanup")
	}
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

type copyDestination struct {
	bytes.Buffer
	readFrom bool
	maxWrite int
	sizes    []int
}

func (w *copyDestination) ReadFrom(io.Reader) (int64, error) {
	w.readFrom = true
	return 0, errors.New("unexpected ReaderFrom")
}

func (w *copyDestination) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	w.sizes = append(w.sizes, len(p))

	return w.Buffer.Write(p)
}

// Exercise copying through the supported subscription transport, including page
// verification, ordered delivery, and admission cleanup.
func copyTestValue(t *testing.T, source io.ReadCloser, length int64) *Value {
	t.Helper()
	t.Cleanup(func() { closeBody(source) })
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, length, length, `"v"`)
		_, _ = io.Copy(w, source)
	}))
	c := testClient(t, path, 1)

	v, err := c.Get(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(v) })

	return v
}

func TestWriteToUsesBoundedBufferWithoutReaderFrom(t *testing.T) {
	data := strings.Repeat("xyz", copyBufferSize+13)
	v := copyTestValue(t, io.NopCloser(strings.NewReader(data)), int64(len(data)))
	w := &copyDestination{}

	n, err := io.Copy(w, v)
	if err != nil || n != int64(len(data)) || w.String() != data || w.readFrom || w.maxWrite != copyBufferSize {
		t.Fatal("copy chunking or ReaderFrom dispatch", n, err, w.readFrom, w.maxWrite)
	}

	if n, err := v.WriteTo(w); n != 0 || err != nil {
		t.Fatal("repeated EOF", n, err)
	}

	if len(v.client.copySlots) != 0 || len(v.client.copyBuffers) != 1 {
		t.Fatal("copy buffer not returned")
	}
}

func TestWriteToDeliveryBatches(t *testing.T) {
	// Use a literal target, independent of the implementation's scratch size.
	const batch = 256 * 1024

	for _, tt := range []struct {
		name  string
		size  int
		sizes []int
	}{
		{"short", batch - 1, []int{batch - 1}},
		{"exact", batch, []int{batch}},
		{"tail", 2*batch + 17, []int{batch, batch, 17}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := bytes.Repeat([]byte("xyz"), (tt.size+2)/3)[:tt.size]
			v := copyTestValue(t, io.NopCloser(bytes.NewReader(data)), int64(len(data)))
			w := &copyDestination{}

			n, err := io.Copy(w, v)
			if err != nil || n != int64(len(data)) || !bytes.Equal(w.Bytes(), data) || w.readFrom || !slices.Equal(w.sizes, tt.sizes) {
				t.Fatalf("copy bytes=%d err=%v ReaderFrom=%v batches=%v want=%v", n, err, w.readFrom, w.sizes, tt.sizes)
			}
		})
	}
}

func TestWriteToWriterFailures(t *testing.T) {
	sentinel := errors.New("writer failed")
	for _, tt := range []struct {
		name    string
		n       int
		err     error
		wantN   int64
		wantErr error
	}{
		{"zero", 0, nil, 0, io.ErrShortWrite},
		{"short", 2, nil, 2, io.ErrShortWrite},
		{"error", 0, sentinel, 0, sentinel},
		{"partial error", 2, sentinel, 2, sentinel},
		{"full error", 4, sentinel, 4, sentinel},
		{"negative", -1, nil, 0, io.ErrShortWrite},
		{"excess", 5, nil, 0, io.ErrShortWrite},
		{"negative error", -1, sentinel, 0, sentinel},
		{"excess error", 5, sentinel, 0, sentinel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := copyTestValue(t, io.NopCloser(strings.NewReader("data")), 4)
			calls := 0

			n, err := v.WriteTo(writeFunc(func(p []byte) (int, error) {
				calls++

				if string(p) != "data" {
					t.Fatal("unexpected writer data")
				}

				return tt.n, tt.err
			}))
			if n != tt.wantN || !errors.Is(err, tt.wantErr) || calls != 1 {
				t.Fatal(n, err, calls)
			}

			if len(v.client.copySlots) != 0 {
				t.Fatal("writer failure retained copy buffer")
			}

			closeBody(v)

			if len(v.client.slots) != 0 {
				t.Fatal("writer failure retained admission after Close")
			}
		})
	}
}

func TestWriteToPartialReadErrorsAndNoProgress(t *testing.T) {
	// Incomplete pages and a missing Complete frame never expose unverified
	// bytes. Source errors cross the wire as truncation, not Go error identities.
	for _, payload := range []string{"dat", "data"} {
		c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, subscriptionHead(4, 0, 4))
			_ = fakeSubscriptionFrame(conn, 1, 0, 0, 4)
			_, _ = io.WriteString(conn, payload)
		})

		v, err := c.Get(t.Context(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		var dst bytes.Buffer

		n, err := v.WriteTo(&dst)
		if n != 0 || dst.Len() != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(n, err, dst.String())
		}

		if _, err := v.Read(nil); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("source error not terminal", err)
		}
	}

	// No progress on a subscription is bounded by its context, rather than a
	// synthetic reader repeatedly returning (0, nil).
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(1, 0, 1))
		_, _ = reader.ReadByte()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.Get(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	if n, err := v.WriteTo(io.Discard); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
}

func TestWriteToCancellationFromWriter(t *testing.T) {
	for _, action := range []string{"context", "value", "client"} {
		t.Run(action, func(t *testing.T) {
			v := copyTestValue(t, io.NopCloser(io.LimitReader(repeatedByte('x'), 2*copyBufferSize)), 2*copyBufferSize)

			n, err := v.WriteTo(writeFunc(func(p []byte) (int, error) {
				switch action {
				case "context":
					v.cancel()
				case "value":
					closeBody(v)
				case "client":
					closeBody(v.client)
				}

				return len(p), nil
			}))
			if n != copyBufferSize {
				t.Fatal("read after cancellation", n)
			}

			if action == "context" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				assertKind(t, err, ErrorClosed)
			}
		})
	}
}

func TestWriteToScratchBoundWithCanceledBlockedWriter(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { streamResponse(w, 0, 1, 1, `"v"`) }))
	c := testClient(t, path, 1)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)

	done := make(chan error, 1)

	go func() {
		_, err := v.WriteTo(writeFunc(func(p []byte) (int, error) { close(entered); <-release; return len(p), nil }))
		done <- err
	}()

	<-entered
	closeBody(v)

	if len(c.slots) != 0 || len(c.copySlots) != 1 {
		t.Fatal("cancellation did not separate value and scratch lifetimes")
	}

	next, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(next)

	if n, err := next.WriteTo(io.Discard); n != 0 {
		t.Fatal(n, err)
	} else {
		assertKind(t, err, ErrorUnavailable)
	}
	// Ordinary Read remains usable without SDK copy scratch.
	if data, err := io.ReadAll(next); err != nil || string(data) != "x" {
		t.Fatal(string(data), err)
	}

	closeBody(c)

	if len(c.copyBuffers) != 0 {
		t.Fatal("closed client retained idle scratch")
	}
	// Unblock without closing twice in deferred cleanup.
	release <- struct{}{}

	select {
	case err := <-done:
		assertKind(t, err, ErrorClosed)
	case <-time.After(time.Second):
		t.Fatal("copy did not return after writer unblocked")
	}

	if len(c.copySlots) != 0 || len(c.copyBuffers) != 0 {
		t.Fatal("copy returned scratch to closed client")
	}
}
