// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamingFinalGateAndIncompletePages(t *testing.T) {
	for _, mode := range []string{"success", "short payload", "missing complete", "bad complete"} {
		t.Run(mode, func(t *testing.T) {
			released := make(chan struct{})

			resume := make(chan struct{})
			defer close(resume)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
				if headHeaders(head).Get("Racer-Ordered") != "1" {
					t.Error("streaming did not force ordering")
				}

				_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))

				_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
				if mode == "short payload" {
					_, _ = io.WriteString(conn, "a")
					return
				}

				_, _ = io.WriteString(conn, "abc")

				if !orderedRelease(t, reader, 0, 3) {
					return
				}

				close(released)
				<-resume

				if mode == "missing complete" {
					return
				}

				length := uint64(3)
				if mode == "bad complete" {
					length++
				}

				_ = fakeSubscriptionFrame(conn, 2, 1, length, 0)
			})

			v, err := c.GetStreaming(t.Context(), Request{}, ReadOptions{PageCredits: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			if v.ordered != nil || v.stream.buffers != nil {
				t.Fatal("streaming allocated page receiver")
			}

			if _, err := v.Read(nil); err == nil {
				t.Fatal("streaming Read accepted")
			} else {
				assertKind(t, err, ErrorInvalidArgument)
			}

			if _, err := v.WriteTo(io.Discard); err == nil {
				t.Fatal("streaming WriteTo accepted")
			}

			dst := httptest.NewRecorder()

			type result struct {
				n   int64
				err error
			}

			done := make(chan result, 1)

			go func() { n, err := v.WriteToHTTP(dst); done <- result{n, err} }()

			if mode != "short payload" {
				orderedWait(t, released)
				// The release is emitted only after both prefix writes have returned.
				if dst.Body.String() != "ab" {
					t.Fatal("final byte escaped before Complete", dst.Body.String())
				}

				resume <- struct{}{}
			}

			r := <-done

			want := "ab"
			if mode == "success" {
				want = "abc"
			}

			if mode == "short payload" {
				want = "a"
			}

			if dst.Body.String() != want || r.n != int64(len(want)) || (r.err == nil) != (mode == "success") {
				t.Fatal(dst.Body.String(), r)
			}

			if mode == "bad complete" {
				assertKind(t, r.err, ErrorProtocol)
			}

			if mode == "short payload" || mode == "missing complete" {
				if !errors.Is(r.err, io.ErrUnexpectedEOF) {
					t.Fatal(r.err)
				}
			}

			wantRead := uint64(3)
			if mode == "short payload" {
				wantRead = 1
			}

			if c.Stats().BytesRead != wantRead || c.Stats().ActiveBulk != 0 {
				t.Fatal(c.Stats())
			}
		})
	}
}

func TestStreamingEmptyAndMalformedFrames(t *testing.T) {
	for _, mode := range []string{"empty", "one byte", "empty bad complete", "kind", "number", "offset", "length", "early complete"} {
		t.Run(mode, func(t *testing.T) {
			size := uint64(3)
			if mode == "empty" || mode == "empty bad complete" {
				size = 0
			}

			if mode == "one byte" {
				size = 1
			}

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(size, 0, size))
				if size == 0 {
					n := uint64(0)
					if mode == "empty bad complete" {
						n++
					}

					_ = fakeSubscriptionFrame(conn, 2, n, 0, 0)

					return
				}

				kind, number, offset, length := byte(1), uint64(0), uint64(0), uint32(size)

				switch mode {
				case "kind":
					kind = 3
				case "number":
					number++
				case "offset":
					offset++
				case "length":
					length++
				case "early complete":
					kind = 2
				}

				_ = fakeSubscriptionFrame(conn, kind, number, offset, length)
				if mode == "one byte" {
					_, _ = io.WriteString(conn, "x")
					if orderedRelease(t, reader, 0, 1) {
						_ = fakeSubscriptionFrame(conn, 2, 1, 1, 0)
					}
				}
			})

			v, err := c.GetStreaming(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			dst := httptest.NewRecorder()

			n, err := v.WriteToHTTP(dst)
			if mode == "empty" || mode == "one byte" {
				if n != int64(size) || err != nil {
					t.Fatal(n, err)
				}
			} else {
				assertKind(t, err, ErrorProtocol)

				if n != 0 || dst.Body.Len() != 0 {
					t.Fatal("invalid payload exposed")
				}
			}
		})
	}
}

type streamingResponse struct {
	http.ResponseWriter
	fast *atomic.Int64
}

func (w streamingResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w streamingResponse) ReadFrom(r io.Reader) (int64, error) {
	lr, ok := r.(*io.LimitedReader)
	if !ok || lr.N <= 0 || lr.N > copyBufferSize {
		return 0, errors.New("unbounded transfer")
	}

	if _, ok := lr.R.(*net.UnixConn); !ok {
		return 0, errors.New("wrapped source")
	}

	w.fast.Add(lr.N)

	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

func TestStreamingHTTPFastPathRangesAndFallback(t *testing.T) {
	const size = int64(PageSize) + copyBufferSize + 17

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, last := fixtureRange(t, r, size)
		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{offset: int64(first)}, int64(last-first)+1)
	}))
	c := testClient(t, path, 2)

	var fast, connections atomic.Int64

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)

		v, err := c.GetStreaming(r.Context(), Request{}, ReadOptions{Offset: ByteOffset(offset), PageCredits: 1})
		if err != nil {
			t.Error(err)
			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))

		if offset != 0 {
			w.WriteHeader(http.StatusPartialContent)
		}

		n, err := v.WriteToHTTP(streamingResponse{w, &fast})
		if n != size-offset || err != nil {
			t.Error(n, err)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}

	server.Start()
	defer server.Close()

	for _, offset := range []int64{0, int64(PageSize) - 3, size - 1} {
		r, err := server.Client().Get(server.URL + "?offset=" + strconv.FormatInt(offset, 10))
		if err != nil {
			t.Fatal(err)
		}

		n, err := io.Copy(&offsetSink{offset: offset}, r.Body)
		closeBody(r.Body)

		if n != size-offset || err != nil {
			t.Fatal(n, err)
		}
	}

	if fast.Load() == 0 || connections.Load() != 1 {
		t.Fatal(fast.Load(), connections.Load())
	}
	// A writer without ReaderFrom must retain the same payload framing.
	v, err := c.GetStreaming(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(size - 100)})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	dst := httptest.NewRecorder()
	if n, err := v.WriteToHTTP(dst); n != 100 || err != nil {
		t.Fatal(n, err)
	}

	if _, err := io.Copy(&offsetSink{offset: size - 100}, dst.Body); err != nil {
		t.Fatal(err)
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

	if v.stream.conn.reader.Buffered() != 45 {
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

type streamingWriter struct {
	http.ResponseWriter
	write func([]byte) (int, error)
}

func (w streamingWriter) Write(p []byte) (int, error) { return w.write(p) }

func TestStreamingBlockedWriterCapacityAndCancellation(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
		_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
		_, _ = io.WriteString(conn, "abc")
		_, _ = io.Copy(io.Discard, reader)
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}

	entered, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)

	done := make(chan error, 1)

	go func() {
		_, err := v.WriteToHTTP(streamingWriter{httptest.NewRecorder(), func(p []byte) (int, error) {
			close(entered)
			<-resume

			if string(p) != "ab" {
				t.Error("blocked scratch changed", string(p))
			}

			return len(p), nil
		}})
		done <- err
	}()

	orderedWait(t, entered)
	cancel()
	orderedWait(t, v.finished)

	if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 1 {
		t.Fatal("wrong canceled admission", c.Stats())
	}

	next, err := c.GetStreaming(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(next)

	_, err = next.WriteToHTTP(httptest.NewRecorder())
	assertKind(t, err, ErrorUnavailable)

	resume <- struct{}{}

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if len(c.copySlots) != 0 {
		t.Fatal("copy slot leaked")
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

func TestStreamingOptionsValidation(t *testing.T) {
	c := testClient(t, "unused", 1)
	for _, o := range []ReadOptions{{PageCredits: -1}, {PageCredits: 65}, {ByteCredits: PageSize - 1}, {ByteCredits: 64*PageSize + 1}} {
		_, err := c.GetStreaming(t.Context(), Request{}, o)
		assertKind(t, err, ErrorInvalidArgument)
	}

	_, err := c.GetStreaming(nil, Request{}) //nolint:staticcheck // Exercise invalid nil-context rejection.
	assertKind(t, err, ErrorInvalidArgument)
	_, err = c.GetStreaming(t.Context(), Request{}, ReadOptions{}, ReadOptions{})
	assertKind(t, err, ErrorInvalidArgument)

	if c.Stats().Dials != 0 {
		t.Fatal("invalid options dialed")
	}
}

type streamingTCPResponse struct{ *net.TCPConn }

func (w streamingTCPResponse) Header() http.Header { return make(http.Header) }
func (w streamingTCPResponse) WriteHeader(int)     {}

func TestStreamingFastDestinationCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v, err := c.GetStreaming(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(listener)

	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(peer)

	destination, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(destination)

	if err := destination.(*net.TCPConn).SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}

	if err := peer.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := v.WriteToHTTP(streamingTCPResponse{destination.(*net.TCPConn)}); done <- err }()

	if _, err := io.CopyN(io.Discard, peer, 64*1024); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("socket transfer did not cancel")
	}

	if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 {
		t.Fatal("canceled transfer retained admission")
	}
}

type streamingBadReaderFrom struct {
	http.ResponseWriter
	mode string
}

func (w streamingBadReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	lr := r.(*io.LimitedReader)

	switch w.mode {
	case "negative":
		return -1, nil
	case "excess":
		return lr.N + 1, nil
	case "short":
		_, err := io.Copy(io.Discard, lr)
		return 0, err
	}

	return 0, nil
}

func TestStreamingReaderFromFailures(t *testing.T) {
	for _, mode := range []string{"negative", "excess", "short", "no progress"} {
		t.Run(mode, func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponse(w, 0, 2*copyBufferSize, 2*copyBufferSize, `"v"`)
			}))
			c := testClient(t, path, 1)

			v, err := c.GetStreaming(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(v)

			n, err := v.WriteToHTTP(streamingBadReaderFrom{httptest.NewRecorder(), mode})
			if err == nil || n >= 2*copyBufferSize {
				t.Fatal("bad ReaderFrom accepted", n, err)
			}

			if mode == "no progress" {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("failed transfer retained admission")
			}
		})
	}
}

func TestStreamingTLSFallback(t *testing.T) {
	const size = 2*copyBufferSize + 17

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponseHead(w, 0, size, size, `"v"`)
		_, _ = io.CopyN(w, &offsetStream{}, size)
	}))
	c := testClient(t, path, 1)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := c.GetStreaming(r.Context(), Request{})
		if err != nil {
			t.Error(err)
			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.Itoa(size))

		if n, err := v.WriteToHTTP(w); n != size || err != nil {
			t.Error(n, err)
		}
	}))
	defer server.Close()

	r, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(r.Body)

	if n, err := io.Copy(&offsetSink{}, r.Body); n != size || err != nil {
		t.Fatal(n, err)
	}

	if c.Stats().BytesRead != size {
		t.Fatal(c.Stats())
	}
}

func TestStreamingHTTP2Success(t *testing.T) {
	for _, size := range []int64{0, 1, 2*copyBufferSize + 17} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				streamResponseHead(w, 0, size, size, `"v"`)
				_, _ = io.CopyN(w, &offsetStream{}, size)
			}))
			c := testClient(t, path, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 {
					t.Error("HTTP/2 not negotiated")
				}

				v, err := c.GetStreaming(r.Context(), Request{})
				if err != nil {
					t.Error(err)
					http.Error(w, "get failed", http.StatusBadGateway)

					return
				}
				defer closeBody(v)

				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))

				if n, err := v.WriteToHTTP(w); n != size || err != nil {
					t.Error(n, err)
				}
			}))
			server.EnableHTTP2 = true

			server.StartTLS()
			defer server.Close()
			// Exercise successive streams on the same HTTP/2 connection. Reading
			// to EOF observes END_STREAM and catches resets after the final Write.
			for range 5 {
				r, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}

				if r.ProtoMajor != 2 || r.StatusCode != http.StatusOK {
					closeBody(r.Body)
					t.Fatal(r.Proto, r.Status)
				}

				n, err := io.Copy(&offsetSink{}, r.Body)
				closeBody(r.Body)

				if n != size || err != nil {
					t.Fatal(n, err)
				}
			}

			if c.Stats().BytesRead != uint64(5*size) || c.Stats().ActiveBulk != 0 {
				t.Fatal(c.Stats())
			}
		})
	}
}

type streamingDeadlineRecorder struct {
	http.ResponseWriter
	expired atomic.Int64
}

func (w *streamingDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.expired.Add(1)
	}

	return nil
}

func TestStreamingSuccessDoesNotExpireDestination(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(0, 0, 0))
		_ = fakeSubscriptionFrame(conn, 2, 0, 0, 0)
	})

	v, err := c.GetStreaming(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	dst := &streamingDeadlineRecorder{ResponseWriter: httptest.NewRecorder()}
	if n, err := v.WriteToHTTP(dst); n != 0 || err != nil {
		t.Fatal(n, err)
	}

	if dst.expired.Load() != 0 {
		t.Fatal("successful completion expired destination deadline")
	}
}
