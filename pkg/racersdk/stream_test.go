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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type finalCreditBlockedConn struct {
	net.Conn
	started     chan struct{}
	closed      chan struct{}
	writeResult chan error
	once        sync.Once
}

func (c *finalCreditBlockedConn) Write(p []byte) (int, error) {
	if len(p) == 12 {
		close(c.started)

		n, err := c.Conn.Write(p)
		c.writeResult <- err

		return n, err
	}

	return c.Conn.Write(p)
}

func (c *finalCreditBlockedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.closed) })

	return err
}

func TestBufferedCompleteBeforeFinalCredit(t *testing.T) {
	for _, api := range []string{"Get", "OpenPages"} {
		for _, mode := range []string{"success", "malformed", "missing", "canceled"} {
			t.Run(api+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				started, closed, peerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
				writeResult := make(chan error, 1)
				c := rawSubscriptionClient(t, func(conn net.Conn, _ *bufio.Reader, _ []byte) {
					defer close(peerDone)

					_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
					_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
					_, _ = io.WriteString(conn, "abc")

					select {
					case <-started:
					case <-closed:
						return
					}

					switch mode {
					case "canceled":
						cancel()
					case "missing":
						return
					default:
						length := uint64(3)
						if mode == "malformed" {
							length++
						}

						_ = fakeSubscriptionFrame(conn, 2, 1, length, 0)
					}

					<-closed
				})
				poolConfig := c.bulk.config
				dial := poolConfig.Dial
				poolConfig.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := dial(ctx, network, address)
					if err != nil {
						return nil, err
					}

					return &finalCreditBlockedConn{Conn: conn, started: started, closed: closed, writeResult: writeResult}, nil
				}
				c.configurePools(poolConfig)

				var (
					data []byte
					err  error
				)

				if api == "Get" {
					v, openErr := c.Get(ctx, Request{}, ReadOptions{PageCredits: 1})
					if openErr != nil {
						t.Fatal(openErr)
					}
					defer closeBody(v)

					data, err = io.ReadAll(v)
					orderedClean(t, v)
				} else {
					s, openErr := c.OpenPages(ctx, Request{}, ReadOptions{PageCredits: 1})
					if openErr != nil {
						t.Fatal(openErr)
					}
					defer closeBody(s)

					var page *PageLease

					page, err = s.Next()
					if page != nil {
						data = append(data, page.Data...)
						if releaseErr := page.Release(); releaseErr != nil {
							t.Fatal(releaseErr)
						}

						if _, nextErr := s.Next(); nextErr != io.EOF {
							t.Fatal(nextErr)
						}
					}

					if s.bytesHeld != 0 || len(s.outstanding) != 0 {
						t.Fatal("retained final accounting")
					}
				}

				orderedWait(t, peerDone)

				if writeErr := <-writeResult; !errors.Is(writeErr, io.ErrClosedPipe) && !errors.Is(writeErr, net.ErrClosed) {
					t.Fatal("final credit write was not interrupted by closure", writeErr)
				}

				if mode == "success" {
					if err != nil || string(data) != "abc" {
						t.Fatal("Complete overridden by blocked final credit", string(data), err)
					}
				} else {
					if len(data) != 0 {
						t.Fatal("invalid final bytes exposed", string(data))
					}

					switch mode {
					case "malformed":
						assertKind(t, err, ErrorProtocol)
					case "missing":
						if !errors.Is(err, io.ErrUnexpectedEOF) {
							t.Fatal(err)
						}
					case "canceled":
						if !errors.Is(err, context.Canceled) {
							t.Fatal(err)
						}
					}
				}

				if c.Stats().ActiveBulk != 0 {
					t.Fatal("retained admission", c.Stats())
				}
			})
		}
	}
}

func TestBufferedFinalCreditGate(t *testing.T) {
	for _, api := range []string{"Get", "OpenPages"} {
		for _, mode := range []string{"success", "malformed", "missing"} {
			t.Run(api+"/"+mode, func(t *testing.T) {
				released := make(chan struct{})

				resume := make(chan struct{})
				defer close(resume)

				peerDone := make(chan struct{})
				c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, head []byte) {
					defer close(peerDone)

					if headHeaders(head).Get("Racer-Page-Credits") != "1" {
						t.Error("not a one-credit subscription")
					}

					_, _ = io.WriteString(conn, subscriptionHead(3, 0, 3))
					_ = fakeSubscriptionFrame(conn, 1, 0, 0, 3)
					_, _ = io.WriteString(conn, "abc")

					var credit [12]byte
					if _, err := io.ReadFull(reader, credit[:]); err != nil {
						return
					}

					if binary.BigEndian.Uint64(credit[:8]) != 0 || binary.BigEndian.Uint32(credit[8:]) != 3 {
						t.Error("incorrect final credit", credit)
						return
					}

					close(released)
					<-resume

					if mode == "missing" {
						return
					}

					length := uint64(3)
					if mode == "malformed" {
						length++
					}

					_ = fakeSubscriptionFrame(conn, 2, 1, length, 0)

					if n, _ := io.Copy(io.Discard, reader); n != 0 {
						t.Error("duplicate final credit", n)
					}
				})

				type result struct {
					data []byte
					page *PageLease
					err  error
				}

				done := make(chan result, 1)

				var (
					s   *PageStream
					v   *Value
					err error
				)

				if api == "Get" {
					v, err = c.Get(t.Context(), Request{}, ReadOptions{PageCredits: 1})
					if err != nil {
						t.Fatal(err)
					}
					defer closeBody(v)

					s = v.stream

					go func() {
						data := make([]byte, 3)

						n, err := v.Read(data)
						done <- result{data: data[:n], err: err}
					}()
				} else {
					s, err = c.OpenPages(t.Context(), Request{}, ReadOptions{PageCredits: 1})
					if err != nil {
						t.Fatal(err)
					}
					defer closeBody(s)

					go func() { page, err := s.Next(); done <- result{page: page, err: err} }()
				}

				select {
				case <-released:
				case got := <-done:
					t.Fatal("receive ended before final credit", got.err)
				case <-time.After(2 * time.Second):
					t.Fatal("peer did not receive final credit before Complete")
				}

				select {
				case got := <-done:
					t.Fatal("final bytes exposed before Complete", got)
				default:
				}

				resume <- struct{}{}

				got := <-done
				if mode == "success" {
					if got.page != nil {
						got.data = append([]byte(nil), got.page.Data...)
						if err := got.page.Release(); err != nil {
							t.Fatal(err)
						}

						if err := got.page.Release(); err != nil || got.page.Data != nil {
							t.Fatal("final release is not idempotent", err)
						}

						if _, err := s.Next(); err != io.EOF {
							t.Fatal(err)
						}
					}

					if got.err != nil || string(got.data) != "abc" {
						t.Fatal(string(got.data), got.err)
					}

					if v != nil {
						if n, err := v.Read(make([]byte, 1)); n != 0 || err != io.EOF {
							t.Fatal(n, err)
						}
					}
				} else {
					if got.page != nil || len(got.data) != 0 {
						t.Fatal("invalid final page exposed", got)
					}

					if mode == "malformed" {
						assertKind(t, got.err, ErrorProtocol)
					} else if !errors.Is(got.err, io.ErrUnexpectedEOF) {
						t.Fatal(got.err)
					}
				}

				if v != nil {
					orderedClean(t, v)
				} else {
					closeBody(s)
				}

				orderedWait(t, peerDone)
				s.mu.Lock()
				defer s.mu.Unlock()

				if s.bytesHeld != 0 || len(s.outstanding) != 0 || c.Stats().ActiveBulk != 0 {
					t.Fatal("final page retained accounting or admission", s.bytesHeld, s.outstanding, c.Stats())
				}
			})
		}
	}
}

func TestPageStreamFinalCreditPreservesOutstandingLease(t *testing.T) {
	for _, mode := range []string{"success", "malformed", "missing"} {
		t.Run(mode, func(t *testing.T) {
			const (
				first = uint64(PageSize) - 2
				end   = uint64(PageSize) + 3
			)

			peerDone := make(chan struct{})
			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				defer close(peerDone)

				_, _ = io.WriteString(conn, subscriptionHead(end, first, end))
				_ = fakeSubscriptionFrame(conn, 1, 0, first, 2)
				_, _ = io.WriteString(conn, "ab")
				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
				_, _ = io.WriteString(conn, "xyz")

				if !orderedRelease(t, reader, 1, 3) {
					return
				}

				if mode == "missing" {
					return
				}

				length := uint64(5)
				if mode == "malformed" {
					length++
				}

				_ = fakeSubscriptionFrame(conn, 2, 2, length, 0)

				if n, _ := io.Copy(io.Discard, reader); n != 0 {
					t.Error("unexpected credit after terminal frame", n)
				}
			})

			s, err := c.OpenPages(t.Context(), Request{}, ReadOptions{Offset: ByteOffset(first), PageCredits: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(s)

			p, err := s.Next()
			if err != nil {
				t.Fatal(err)
			}

			last, err := s.Next()
			if mode == "success" {
				if err != nil || last == nil || string(last.Data) != "xyz" {
					t.Fatal(last, err)
				}

				if err := last.Release(); err != nil {
					t.Fatal(err)
				}

				if err := last.Release(); err != nil {
					t.Fatal(err)
				}
			} else {
				if last != nil {
					t.Fatal("invalid final lease exposed")
				}

				if mode == "malformed" {
					assertKind(t, err, ErrorProtocol)
				} else if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
			}

			s.mu.Lock()
			held, count, length := s.bytesHeld, len(s.outstanding), s.outstanding[0]
			s.mu.Unlock()

			if held != 2 || count != 1 || length != 2 || string(p.Data) != "ab" {
				t.Fatal("final credit changed outstanding lease", held, count, length, string(p.Data))
			}

			releaseErr := p.Release()
			if releaseErr != err {
				t.Fatal("unexpected outstanding release error", releaseErr, err)
			}

			if s.bytesHeld != 0 || len(s.outstanding) != 0 || p.Data != nil {
				t.Fatal("outstanding release retained accounting")
			}

			closeBody(s)
			orderedWait(t, peerDone)
		})
	}
}

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
	c := streamFakeClient(t, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Key()[0] == 1 {
			return originMeta(0), nil, nil
		}

		if r.Operation() == OperationHead {
			return originMeta(9), nil, nil
		}

		return originMeta(9), io.NopCloser(strings.NewReader("012345678")), nil
	})

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

	select {
	case first := <-entered:
		seen[first] = true
	case <-time.After(3 * time.Second):
		t.Fatal("pages did not open concurrently")
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

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription waited for its body")
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

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker not started")
	}

	closeBody(v)

	select {
	case err := <-done:
		assertKind(t, err, ErrorClosed)
	case <-time.After(3 * time.Second):
		t.Fatal("consumer not canceled")
	}

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("worker not canceled")
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
	for _, test := range []struct {
		name    string
		options ReadOptions
	}{
		{"page credit", ReadOptions{PageCredits: 1}},
		{"byte credit", ReadOptions{PageCredits: 64, ByteCredits: PageSize}},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			options := test.options
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
		})
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
						if state != "complete queued" || binary.BigEndian.Uint64(frame[:8]) != 1 || binary.BigEndian.Uint32(frame[8:]) != 3 {
							t.Error("cleanup prematurely returned held credit")
						}
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
	poolConfig := c.bulk.config
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
