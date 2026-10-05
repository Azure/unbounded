// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientContinuationAbsoluteDeadline(t *testing.T) {
	const (
		budget    = 2 * time.Second
		pageDelay = 1100 * time.Millisecond
		size      = 3*int64(PageSize) + 13
	)

	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		controller := http.NewResponseController(w)
		// Model the listener's progress deadline. A multipage exchange may outlive
		// one budget while every bounded page write still makes progress.
		if err := controller.SetWriteDeadline(time.Now().Add(budget)); err != nil {
			t.Error(err)
			return
		}

		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()

		first, last := fixtureRange(t, r, size)

		streamResponseHead(w, int64(first), int64(last-first)+1, size, `"v"`)

		if err := controller.Flush(); err != nil {
			return
		}

		for start := int64(first); start <= int64(last); start += int64(PageSize) {
			if err := controller.SetWriteDeadline(time.Now().Add(budget)); err != nil {
				t.Error(err)
				return
			}

			timer := time.NewTimer(pageDelay)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				return
			}

			length := min(int64(PageSize), int64(last)-start+1)
			if _, err := io.CopyN(w, repeatedByte('x'), length); err != nil {
				return
			}

			if err := controller.Flush(); err != nil {
				return
			}
		}
	}))
	c := testClient(t, path, 1)

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	started := time.Now()

	n, err := io.Copy(io.Discard, v)
	if err != nil || n != size-int64(PageSize) {
		t.Fatalf("continuations exceeded a single request budget: %d %v", n, err)
	}

	if time.Since(started) <= budget || calls.Load() != 1 {
		t.Fatal("test did not exercise a progressing multipage remainder beyond one budget")
	}
}

func TestClientLaterContinuationCancellation(t *testing.T) {
	for _, bodyStarted := range []bool{false, true} {
		t.Run(strconv.FormatBool(bodyStarted), func(t *testing.T) {
			for _, action := range []string{"context", "value", "client"} {
				t.Run(action, func(t *testing.T) {
					const size = 2*int64(PageSize) + 2

					var calls atomic.Int32

					entered, stopped := make(chan struct{}), make(chan struct{})
					path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						streamResponseHead(w, 0, size, size, `"v"`)
						_, _ = io.CopyN(w, repeatedByte('x'), int64(PageSize))

						if bodyStarted {
							_, _ = w.Write([]byte("x"))

							if err := http.NewResponseController(w).Flush(); err != nil {
								t.Error(err)
							}
						}

						close(entered)
						<-r.Context().Done()
						close(stopped)
					}))
					c := testClient(t, path, 1)

					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()

					v, err := c.Get(ctx, Request{})
					if err != nil {
						t.Fatal(err)
					}

					if n, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil || n != int64(PageSize) {
						t.Fatal(n, err)
					}

					if n, err := v.Read(nil); n != 0 || err != nil || calls.Load() != 1 || len(c.slots) != 1 {
						t.Fatal("page boundary eagerly continued or released capacity", n, err)
					}

					waitCtx, stopWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer stopWait()

					if _, err := c.Get(waitCtx, Request{}); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("live Value did not retain its slot", err)
					}

					done := make(chan error, 1)

					go func() { _, err := io.Copy(io.Discard, v); done <- err }()

					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("later continuation not opened")
					}

					switch action {
					case "context":
						cancel()
					case "value":
						closeBody(v)
					case "client":
						closeBody(c)
					}

					select {
					case err := <-done:
						if action == "context" {
							if !errors.Is(err, context.Canceled) {
								t.Fatal(err)
							}
						} else {
							assertKind(t, err, ErrorClosed)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("later continuation retained after cancellation")
					}

					select {
					case <-stopped:
					case <-time.After(3 * time.Second):
						t.Fatal("continuation connection retained")
					}

					closeBody(v)

					if len(c.slots) != 0 || calls.Load() != 1 {
						t.Fatal("cancellation retained capacity or retried")
					}
				})
			}
		})
	}
}

func TestPageStreamUnorderedReleaseAndCompletion(t *testing.T) {
	const (
		first = uint64(PageSize) - 2
		end   = uint64(PageSize) + 3
	)

	var calls atomic.Int32

	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
		calls.Add(1)

		if !strings.HasPrefix(string(head), "POST /v2/objects/") || headHeaders(head).Get("Racer-Ordered") != "0" {
			t.Error("not unordered v2")
		}

		_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
		_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
		_, _ = conn.Write([]byte("xyz"))

		var release [12]byte
		if _, err := io.ReadFull(reader, release[:]); err != nil {
			return
		}

		if binary.BigEndian.Uint64(release[:8]) != 1 || binary.BigEndian.Uint32(release[8:]) != 3 {
			t.Error("incorrect release")
		}

		_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
		_, _ = conn.Write([]byte("ab"))
		_ = fakeSubscriptionFrame(conn, 2, 2, 5, 0)
	})

	s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), Length: 5, PageCredits: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(s)

	p, err := s.Next()
	if err != nil || p.Number != 1 || p.Offset != ByteOffset(PageSize) || string(p.Data) != "xyz" {
		t.Fatal(p, err)
	}

	result := make(chan *PageLease, 1)
	errors := make(chan error, 1)

	go func() { next, err := s.Next(); result <- next; errors <- err }()

	select {
	case <-result:
		t.Fatal("Next bypassed held credit")
	case <-time.After(20 * time.Millisecond):
	}

	if err := p.Release(); err != nil {
		t.Fatal(err)
	}

	if err := p.Release(); err != nil || p.Data != nil {
		t.Fatal("release not idempotent", err)
	}

	next := <-result
	if err := <-errors; err != nil || next.Number != 0 || next.Offset != ByteOffset(first) || string(next.Data) != "ab" {
		t.Fatal(next, err)
	}

	if err := next.Release(); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Next(); err != io.EOF {
		t.Fatal(err)
	}

	if calls.Load() != 1 || c.Stats().Dials != 1 || c.Stats().ActiveBulk != 0 {
		t.Fatal(c.Stats())
	}
}

func TestPageStreamMalformedFrames(t *testing.T) {
	for _, mode := range []string{"kind", "number", "offset", "length", "short", "missing complete", "complete count", "complete bytes", "complete length"} {
		t.Run(mode, func(t *testing.T) {
			c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
				kind, number, offset, length := byte(1), uint64(0), uint64(0), uint32(3)

				switch mode {
				case "kind":
					kind = 7
				case "number":
					number = 1
				case "offset":
					offset = 1
				case "length":
					length = 4
				}

				_ = fakeSubscriptionFrame(conn, kind, number, offset, length)
				if mode == "short" {
					_, _ = conn.Write([]byte("ab"))
					return
				}

				_, _ = conn.Write([]byte("abc"))

				if mode == "missing complete" {
					return
				}

				number, offset, length = 1, 3, 0

				switch mode {
				case "complete count":
					number++
				case "complete bytes":
					offset++
				case "complete length":
					length++
				}

				_ = fakeSubscriptionFrame(conn, 2, number, offset, length)
			})

			s, err := c.OpenPages(t.Context(), Request{})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if p != nil || err == nil || err == io.EOF {
				t.Fatal("invalid frame exposed", p, err)
			}

			if mode == "short" || mode == "missing complete" {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
			} else {
				assertKind(t, err, ErrorProtocol)
			}

			if _, again := s.Next(); again != err {
				t.Fatal("nonterminal failure", again)
			}

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("admission leaked")
			}
		})
	}
}

func TestPageStreamPartialAndEmptyRanges(t *testing.T) {
	c, cleanup, err := newFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Key()[0] == 1 {
			return originMeta(0), nil, nil
		}

		if r.Operation() == OperationHead {
			return originMeta(9), nil, nil
		}

		return originMeta(9), io.NopCloser(strings.NewReader("012345678")), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	for _, offset := range []ByteOffset{3, 9} {
		o := ReadOptions{Offset: offset}
		request := Request{}

		if offset == 3 {
			o.Length = 3
		} else {
			o.Offset = 0
			request.Key[0] = 1
		}

		s, err := c.OpenPages(t.Context(), request, o)
		if err != nil {
			t.Fatal(err)
		}

		p, err := s.Next()
		if offset == 3 {
			if err != nil || p.Offset != 3 || string(p.Data) != "345" {
				t.Fatal(p, err)
			}

			if err := p.Release(); err != nil || p.Data != nil {
				t.Fatal("release retained payload", err)
			}

			p, err = s.Next()
		}

		if p != nil || err != io.EOF {
			t.Fatal("missing Complete", p, err)
		}

		closeBody(s)

		if c.Stats().ActiveBulk != 0 {
			t.Fatal("range retained admission")
		}
	}
}

func TestPageStreamCallerFailureReleasesOwnership(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
		first, end := uint64(PageSize)-1, uint64(PageSize)+1
		_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
		_ = fakeSubscriptionFrame(conn, 1, 0, first, 1)
		_, _ = io.WriteString(conn, "x")
		_, _ = io.Copy(io.Discard, reader)
	})

	s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(PageSize - 1), PageCredits: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(s)

	p, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	failed := errors.New("destination failed")

	_, err = (writeFunc(func([]byte) (int, error) { return 0, failed })).Write(p.Data)
	if !errors.Is(err, failed) {
		t.Fatal(err)
	}
	// The caller owns cleanup after its destination fails. Close must not
	// invalidate a held public lease, and Release must work after Close.
	closeBody(s)

	if string(p.Data) != "x" || c.Stats().ActiveBulk != 0 {
		t.Fatal("close invalidated lease or retained admission")
	}

	assertKind(t, p.Release(), ErrorClosed)
	assertKind(t, p.Release(), ErrorClosed)

	if p.Data != nil || s.bytesHeld != 0 || len(s.outstanding) != 0 {
		t.Fatal("release retained ownership")
	}
}

func TestPageStreamByteCreditWaitAndCancellation(t *testing.T) {
	for _, action := range []string{"release", "context", "stream", "client"} {
		t.Run(action, func(t *testing.T) {
			const (
				first = uint64(PageSize) - 1
				end   = 2*uint64(PageSize) + 1
			)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
				_ = fakeSubscriptionFrame(conn, 1, 0, first, 1)
				_, _ = io.WriteString(conn, "x")

				var release [12]byte
				if _, err := io.ReadFull(reader, release[:]); err != nil {
					return
				}

				if action != "release" {
					t.Error("close emitted a release")
				}

				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), uint32(PageSize))
				_, _ = io.CopyN(conn, repeatedByte('y'), int64(PageSize))
				_, _ = io.Copy(io.Discard, reader)
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s, err := c.OpenPages(ctx, Request{}, ReadOptions{Offset: ByteOffset(first), PageCredits: 2, ByteCredits: PageSize})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)

			go func() {
				next, err := s.Next()
				if next != nil {
					if next.Number != 1 || len(next.Data) != int(PageSize) {
						t.Error("wrong resumed page")
					}

					closeBody(s)

					_ = next.Release()
				}

				result <- err
			}()

			select {
			case err := <-result:
				t.Fatal("Next did not wait for byte credit", err)
			case <-time.After(20 * time.Millisecond):
			}

			switch action {
			case "release":
				if err := p.Release(); err != nil {
					t.Fatal(err)
				}
			case "context":
				cancel()
			case "stream":
				closeBody(s)
			case "client":
				closeBody(c)
			}

			select {
			case err := <-result:
				switch action {
				case "release":
					if err != nil {
						t.Fatal(err)
					}
				case "context":
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				default:
					assertKind(t, err, ErrorClosed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("credit waiter did not unblock")
			}

			_ = p.Release()

			if c.Stats().ActiveBulk != 0 {
				t.Fatal("retained admission")
			}
		})
	}
}

func TestPageStreamDuplicateAndOrderedValidation(t *testing.T) {
	for _, ordered := range []bool{false, true} {
		t.Run(strconv.FormatBool(ordered), func(t *testing.T) {
			const (
				first = uint64(PageSize) - 1
				end   = uint64(PageSize) + 1
			)

			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 1)
				_, _ = io.WriteString(conn, "x")

				if ordered {
					return
				}

				var release [12]byte
				if _, err := io.ReadFull(reader, release[:]); err != nil {
					return
				}

				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 1)
			})

			s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), Ordered: ordered})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if !ordered {
				if err != nil {
					t.Fatal(err)
				}

				if err := p.Release(); err != nil {
					t.Fatal(err)
				}

				p, err = s.Next()
			}

			if p != nil {
				t.Fatal("invalid page exposed")
			}

			assertKind(t, err, ErrorProtocol)
		})
	}
}

func TestPageStreamHeadAndOptionsValidation(t *testing.T) {
	base := subscriptionHead(3, 0, 3)
	for _, head := range []string{
		strings.Replace(base, "Content-Length: 45", "Content-Length: 44", 1),
		strings.Replace(base, "Racer-Object-Length: 3", "Racer-Object-Length: 03", 1),
		strings.Replace(base, "Racer-Range-End: 3", "Racer-Range-End: 4", 1),
		strings.Replace(base, "Racer-Range-Start: 0", "Racer-Range-Start: 1", 1),
		strings.Replace(base, "Racer-Expires-At: 0", "Racer-Expires-At: 9223372036854775808", 1),
		strings.Replace(base, "Connection: close", "Connection: keep-alive", 1),
		strings.Replace(base, "ETag: \"v\"", "ETag: W/\"v\"", 1),
		strings.Replace(base, "Racer-Range-End: 3", "Racer-Range-End: 3\r\nracer-range-end: 3", 1),
		strings.Replace(base, "Content-Type: application/octet-stream", "Content-Type: text/plain", 1),
		"HTTP/1.1 416 Range Not Satisfiable\r\nContent-Length: 0\r\n\r\n",
	} {
		c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) { _, _ = io.WriteString(conn, head) })
		_, err := c.OpenPages(t.Context(), Request{})
		assertKind(t, err, ErrorProtocol)

		if c.Stats().ActiveBulk != 0 {
			t.Fatal("bad head retained admission")
		}
	}

	c := testClient(t, "unused", 1)
	for _, o := range []ReadOptions{{PageCredits: -1}, {PageCredits: 65}, {ByteCredits: PageSize - 1}, {ByteCredits: 64*PageSize + 1}} {
		_, err := c.OpenPages(t.Context(), Request{}, o)
		assertKind(t, err, ErrorInvalidArgument)
	}

	if c.Stats().Dials != 0 {
		t.Fatal("invalid credits dialed")
	}
}

func TestPageStreamReleaseAfterTerminalAndIntervalBound(t *testing.T) {
	c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
		_, _ = io.WriteString(conn, subscriptionHead(1, 0, 1))
		_ = fakeSubscriptionFrame(conn, 1, 0, 0, 1)
		_, _ = io.WriteString(conn, "x")
		_ = fakeSubscriptionFrame(conn, 2, 1, 1, 0)
	})

	s, err := c.OpenPages(t.Context(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(s)

	p, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Next(); err != io.EOF {
		t.Fatal(err)
	}

	if string(p.Data) != "x" {
		t.Fatal("EOF invalidated lease")
	}

	if err := p.Release(); err != nil {
		t.Fatal(err)
	}

	if err := p.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestPageStreamReleaseConcurrentWithFinalRead(t *testing.T) {
	const (
		first = uint64(PageSize) - 1
		end   = uint64(PageSize) + 1
	)

	for range 20 {
		c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
			_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
			_ = fakeSubscriptionFrame(conn, 1, 0, first, 1)
			_, _ = io.WriteString(conn, "a")
			_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 1)
			_, _ = io.WriteString(conn, "b")
			_ = fakeSubscriptionFrame(conn, 2, 2, 2, 0)
		})

		s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first)})
		if err != nil {
			t.Fatal(err)
		}

		p, err := s.Next()
		if err != nil {
			t.Fatal(err)
		}

		released := make(chan error, 1)

		go func() { released <- p.Release() }()

		next, err := s.Next()
		if err != nil || string(next.Data) != "b" {
			t.Fatal(next, err)
		}

		if _, err := s.Next(); err != io.EOF {
			t.Fatal(err)
		}

		if err := <-released; err != nil {
			t.Fatal("final close raced release", err)
		}

		if err := next.Release(); err != nil {
			t.Fatal(err)
		}

		closeBody(s)
	}
}

func TestValueWindowOrderedAndBounded(t *testing.T) {
	const size = 5*int64(PageSize) + 17

	entered := make(chan uint64, 8)
	release := make(chan struct{})

	var calls atomic.Int32

	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if r.Header.Get("Racer-Ordered") != "1" || r.Header.Get("Racer-Page-Credits") != "3" {
			t.Error("ordered credits lost")
		}

		streamResponseHead(w, 0, size, size, `"v"`)

		_, _ = io.CopyN(w, &offsetStream{}, int64(PageSize))
		entered <- uint64(PageSize)

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.CopyN(w, &offsetStream{offset: int64(PageSize)}, size-int64(PageSize))
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		_, err := io.Copy(&offsetSink{offset: int64(PageSize)}, v)
		done <- err
	}()

	seen := make(map[uint64]bool)

	for range 1 {
		select {
		case first := <-entered:
			seen[first] = true
		case <-time.After(3 * time.Second):
			t.Fatal("pages did not open concurrently")
		}
	}

	if len(seen) != 1 || !seen[uint64(PageSize)] {
		t.Fatal("unexpected page window", seen)
	}

	if calls.Load() != 1 || len(c.slots) != 1 {
		t.Fatal("window exceeded pool bounds")
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordered stream stalled")
	}

	if calls.Load() != 1 || len(c.slots) != 0 {
		t.Fatal("wrong requests or retained permits")
	}
}

func TestBootstrapPrefetchUsesOnlySpareAdmission(t *testing.T) {
	// The old bootstrap option is gone. Subscription read-ahead needs only one
	// admission slot, regardless of the configured page credits.
	entered := make(chan struct{}, 2)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, 4*int64(PageSize), 4*int64(PageSize), `"v"`)
		w.(http.Flusher).Flush()

		entered <- struct{}{}

		<-r.Context().Done()
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	for range 1 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("subscription waited for its body")
		}
	}

	if len(c.slots) != 1 || c.Stats().Dials != 1 {
		t.Fatal("prefetch admission incorrect")
	}

	closeBody(v)

	if len(c.slots) != 0 {
		t.Fatal("prefetch retained admission")
	}
}

func TestValueWindowCloseCancelsEveryWorker(t *testing.T) {
	entered := make(chan struct{}, 3)
	stopped := make(chan struct{}, 3)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, 8*int64(PageSize), 8*int64(PageSize), `"v"`)
		_, _ = io.CopyN(w, repeatedByte('x'), int64(PageSize))

		entered <- struct{}{}

		<-r.Context().Done()

		stopped <- struct{}{}
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, v, int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	for range 1 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("worker not started")
		}
	}

	closeBody(v)

	select {
	case err := <-done:
		assertKind(t, err, ErrorClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("consumer not canceled")
	}

	for range 1 {
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("worker not canceled")
		}
	}

	if len(c.slots) != 0 {
		t.Fatal("Close retained permits")
	}
}

func TestValueWindowRefillsBeforeLaterPagesFinish(t *testing.T) {
	const size = 5 * int64(PageSize)

	entered := make(chan int64, 8)
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamResponseHead(w, 0, size, size, `"v"`)

		for page := range int64(4) {
			entered <- page * int64(PageSize)

			if _, err := io.CopyN(w, &offsetStream{offset: page * int64(PageSize)}, int64(PageSize)); err != nil {
				return
			}
		}

		entered <- 4 * int64(PageSize)

		<-r.Context().Done()
	}))
	c := testClient(t, path, 3)
	c.config.PageWindow = 3

	v, err := c.Get(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(v)

	if _, err := io.CopyN(io.Discard, v, 2*int64(PageSize)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { _, err := io.Copy(io.Discard, v); done <- err }()

	seen := make(map[int64]bool)
	for !seen[4*int64(PageSize)] {
		select {
		case offset := <-entered:
			seen[offset] = true
		case <-time.After(3 * time.Second):
			t.Fatal("window did not refill", seen)
		}
	}

	closeBody(v)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not stop")
	}

	if len(c.slots) != 0 {
		t.Fatal("retained permits")
	}
}
