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
	"testing"
	"time"
)

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
	dial := c.dial
	c.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		return orderedReleaseFailureConn{conn}, err
	}

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
	io.ReadCloser
	closed chan struct{}
}

func (b orderedCloseSignal) Close() error {
	err := b.ReadCloser.Close()
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
