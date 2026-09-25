// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Each script gets a separate raw connection. net.Pipe makes body reads and
// connection disposal observable without socket scheduling or wall-clock sleeps.
func lookaheadScript(t *testing.T, ctx context.Context, pages ...scriptedPage) *Stream {
	t.Helper()

	c := &Client{endpoint: "http://localhost", header: make(http.Header), streamPool: &streamPool{limit: 8, speculative: make(chan struct{}, 8)}}
	t.Cleanup(c.CloseIdleConnections)

	for i := len(pages) - 1; i >= 0; i-- {
		page := pages[i]
		client, server := net.Pipe()
		done := make(chan struct{})

		c.streamPool.idle = append(c.streamPool.idle, &streamConn{Conn: client, reader: bufio.NewReader(client), parser: bufio.NewReader(nil), idle: time.Now()})

		t.Cleanup(func() { _ = client.Close(); _ = server.Close(); <-done })

		go func() {
			defer close(done)
			defer server.Close()

			r, err := http.ReadRequest(bufio.NewReader(server))
			if err != nil {
				return // Unused scripts are closed during cleanup.
			}

			if page.beforeHeaders != nil {
				page.beforeHeaders(r)
			}

			start, length, _ := objectRange(r.Header, Metadata{Size: PageSize + 8})

			status := page.status
			if status == 0 {
				status = 206
			}

			_, err = fmt.Fprintf(server, "HTTP/1.1 %d %s\r\nETag: %s\r\nContent-Length: %d\r\nContent-Range: bytes %d-%d/%d\r\n\r\n", status, http.StatusText(status), checksumTag(nil), length, start, start+length-1, PageSize+8)
			if err != nil {
				return
			}

			if page.beforeBody != nil {
				page.beforeBody()
			}

			_, _ = io.WriteString(server, page.body)
		}()
	}

	o := &Object{client: c, target: "/scripted", meta: Metadata{Size: PageSize + 8, ETag: checksumTag(nil)}}

	s, err := o.ReadRange(ctx, PageSize-8, 16)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestLookaheadCriticalPath(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				page := scriptedPage{body: "12345678", beforeHeaders: func(*http.Request) { time.Sleep(3 * time.Second) }, beforeBody: func() { time.Sleep(2 * time.Second) }}

				s := lookaheadScript(t, t.Context(), page, page)
				if !enabled {
					s.object.client.streamPool.speculative = nil
					// The script closes each response socket after its body.
				}

				if err := s.Prepare(); err != nil {
					t.Fatal(err)
				}
				// Force sequential mode to acquire the second scripted socket.
				s.page.responseClose = true
				dst := &delayedWriter{delay: 5 * time.Second}

				n, err := s.WriteTo(dst)
				if err != nil || n != 16 || dst.String() != "1234567812345678" {
					t.Fatal(n, err, dst.String())
				}

				wantHeaders, wantForward := 6*time.Second, 14*time.Second
				if enabled {
					wantHeaders, wantForward = 3*time.Second, 12*time.Second
				}

				stats := s.Stats()
				if stats.PageRequests != 2 || stats.PageRetries != 0 || stats.PageHeaderWait != wantHeaders || stats.ForwardDuration != wantForward || stats.BufferedBytes != 16 {
					t.Fatal(stats)
				}
			})
		})
	}
}

func TestLookaheadStartsAfterValidationAndDefersFailure(t *testing.T) {
	for _, status := range []int{206, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := make(chan struct{})

				var next atomic.Bool

				s := lookaheadScript(t, t.Context(), scriptedPage{status: status, beforeHeaders: func(*http.Request) { <-gate }, body: "12345678"}, scriptedPage{status: 401, beforeHeaders: func(*http.Request) { next.Store(true) }})
				done := make(chan error, 1)

				go func() { done <- s.Prepare() }()

				synctest.Wait()

				if next.Load() {
					t.Fatal("speculated before current headers validated")
				}

				close(gate)

				err := <-done

				synctest.Wait()

				if status == 403 {
					if err == nil || next.Load() {
						t.Fatal("speculated after rejection", err)
					}

					return
				}

				if err != nil || !next.Load() || s.Failure() != nil || s.Stats().PageRequests != 2 {
					t.Fatal("future failure escaped Prepare", err, s.Failure(), s.Stats())
				}

				var out bytes.Buffer

				n, err := s.WriteTo(&out)

				f := s.Failure()
				if err == nil || n != 8 || out.String() != "12345678" || f.PageOffset != PageSize || f.Offset != PageSize || f.StatusCode != 401 || s.Stats().PageRequests != 2 {
					t.Fatal(n, err, f, s.Stats())
				}
			})
		})
	}
}

func TestLookaheadPrefetchedRetry(t *testing.T) {
	for _, status := range []int{429, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				check := func(r *http.Request) {
					if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", PageSize, PageSize+7) || r.Header.Get("If-Match") != checksumTag(nil) {
						t.Error("retried a different page", r.Header)
					}
				}
				s := lookaheadScript(t, t.Context(), scriptedPage{body: "12345678"}, scriptedPage{status: status, beforeHeaders: check}, scriptedPage{body: "abcdefgh", beforeHeaders: check})
				dst := &delayedWriter{delay: time.Second}
				n, err := s.WriteTo(dst)

				stats := s.Stats()
				if err != nil || n != 16 || dst.String() != "12345678abcdefgh" || stats.PageRequests != 3 || stats.PageRetries != 1 || stats.PageHeaderWait != 0 || s.Failure() != nil {
					t.Fatal(n, err, stats, s.Failure())
				}
			})
		})
	}
}

func TestLookaheadCancellationAndAbandonment(t *testing.T) {
	for _, phase := range []string{"headers", "body", "retry"} {
		for _, action := range []string{"close", "cancel", "downstream", "deadline"} {
			t.Run(phase+"/"+action, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()

					page := scriptedPage{body: "abcdefgh"}

					switch phase {
					case "headers":
						page.beforeHeaders = func(*http.Request) { <-ctx.Done() }
					case "retry":
						page.status = 503
					}

					s := lookaheadScript(t, ctx, scriptedPage{body: "12345678"}, page)
					if err := s.Prepare(); err != nil {
						t.Fatal(err)
					}

					synctest.Wait()

					f := s.future
					if f == nil || len(s.object.client.streamPool.speculative) != 1 {
						t.Fatal("missing speculative owner")
					}

					if phase == "retry" && s.Stats().PageRetries != 0 {
						t.Fatal("backoff counted as dispatch")
					}

					switch action {
					case "close":
						_ = s.Close()
					case "cancel":
						cancel()
					case "deadline":
						time.Sleep(time.Second)
					case "downstream":
						_, err := s.WriteTo(&delayedWriter{err: io.ErrClosedPipe})
						if !errors.Is(err, io.ErrClosedPipe) {
							t.Fatal(err)
						}
					}

					<-f.joined

					if len(s.object.client.streamPool.speculative) != 0 || f.page.conn != nil {
						t.Fatal("abandoned future retained resources")
					}

					_ = s.Close()
					if stats := s.Stats(); stats.PageRequests < 2 || stats.PageRetries != stats.PageRequests-2 || s.future != nil || s.page.conn != nil {
						t.Fatal("lost canceled speculative attempts", stats)
					}
				})
			})
		}
	}
}

func lookaheadRange(t *testing.T, c *Client, target string, offset, length int64) *Stream {
	t.Helper()

	o, err := c.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(t.Context(), offset, length)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

// Generated pages use an offset-dependent pattern to detect reordered or
// duplicated bytes while keeping fixture and consumer scratch bounded.
func lookaheadOrigin(t *testing.T, size int64, observe func(*http.Request, int64)) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			return
		}

		start, length, status := objectRange(r.Header, Metadata{Size: size})
		if status != 206 || start/PageSize != (start+length-1)/PageSize || r.Header.Get("If-Match") != checksumTag(nil) || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("invalid pinned page", r.Header)
			w.WriteHeader(400)

			return
		}

		if observe != nil {
			observe(r, start)
		}

		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Range", contentRange(start, start+length-1, size))
		w.WriteHeader(206)

		buf := make([]byte, 32<<10)
		for off := start; off < start+length; {
			b := buf[:min(int64(len(buf)), start+length-off)]
			for i := range b {
				b[i] = byte((off + int64(i)) % 251)
			}

			n, err := w.Write(b)
			if err != nil {
				return
			}

			off += int64(n)
		}
	})
}

type patternWriter struct{ offset int64 }

func (w *patternWriter) Write(p []byte) (int, error) {
	for i, b := range p {
		if b != byte((w.offset+int64(i))%251) {
			return i, fmt.Errorf("unordered body at %d", w.offset+int64(i))
		}
	}

	w.offset += int64(len(p))

	return len(p), nil
}

func TestLookaheadRanges(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		size, offset, length, requests int64
	}{
		{"full", 2*PageSize + 23, 0, 2*PageSize + 23, 3},
		{"unaligned", PageSize + 23, PageSize - 3, 10, 2},
		{"short", 23, 3, 7, 1},
		{"empty-object", 0, 0, 0, 0},
		{"empty-range", PageSize + 23, PageSize, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, lookaheadOrigin(t, tc.size, nil), ClientOptions{PageLookahead: true})

			s := lookaheadRange(t, c, "/object", tc.offset, tc.length)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}

			n, err := s.WriteTo(&patternWriter{offset: tc.offset})
			if err != nil || n != tc.length || s.Stats().PageRequests != tc.requests || s.future != nil || len(c.streamPool.speculative) != 0 {
				t.Fatal(n, err, s.Stats())
			}

			if tc.length == 0 && s.Stats() != (TransferStats{}) {
				t.Fatal("empty stream did work", s.Stats())
			}
		})
	}
}

func TestLookaheadSharedBudgetAndCredentials(t *testing.T) {
	var gets atomic.Int64

	c := newTestClient(t, lookaheadOrigin(t, PageSize+8, func(r *http.Request, _ int64) {
		gets.Add(1)

		data, status := decodeOriginData(r.Header)
		if status != 0 || string(data) != r.URL.Query().Get("auth") {
			t.Error("credentials crossed views")
		}
	}), ClientOptions{PageLookahead: true})

	var holders []*Stream

	for i := range cap(c.streamPool.speculative) + 1 {
		input := []byte(fmt.Sprint(i))

		view, err := c.WithOriginData(input)
		if err != nil {
			t.Fatal(err)
		}

		clear(input)

		s := lookaheadRange(t, view, fmt.Sprintf("/object?auth=%d", i), PageSize-8, 16)
		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}

		if i < cap(c.streamPool.speculative) {
			if s.future == nil {
				t.Fatal("free slot not used")
			}

			holders = append(holders, s)
		} else {
			if s.future != nil {
				t.Fatal("exceeded shared budget")
			}
			// No speculative permit is available, but foreground must finish.
			if n, err := s.WriteTo(&patternWriter{offset: PageSize - 8}); err != nil || n != 16 {
				t.Fatal("budget blocked foreground", n, err)
			}
		}
	}

	_ = holders[0].Close()

	s := lookaheadRange(t, c, "/object", PageSize-8, 16)
	if err := s.Prepare(); err != nil || s.future == nil || len(c.streamPool.speculative) != cap(c.streamPool.speculative) {
		t.Fatal("permit not released", err)
	}

	_ = s.Close()
	for _, s := range holders {
		_ = s.Close()
	}

	if len(c.streamPool.speculative) != 0 {
		t.Fatal("permits leaked")
	}
}

func TestLookaheadTwoSlotBound(t *testing.T) {
	seen := make(chan int64, 4)
	c := newTestClient(t, lookaheadOrigin(t, 3*PageSize, func(_ *http.Request, start int64) { seen <- start }), ClientOptions{PageLookahead: true})

	s := lookaheadRange(t, c, "/object", 0, 3*PageSize)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	for _, want := range []int64{0, PageSize} {
		select {
		case got := <-seen:
			if got != want {
				t.Fatal(got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("future did not overlap current body")
		}
	}

	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	f := s.future
	_ = s.Close()

	<-f.joined

	if s.Stats().PageRequests != 2 || len(seen) != 0 || len(c.streamPool.speculative) != 0 || len(c.streamPool.idle) != 0 {
		t.Fatal("more than two slots or abandoned sockets pooled", s.Stats())
	}
}

func TestLookaheadTerminalFutureErrors(t *testing.T) {
	for _, mode := range []string{"auth", "version", "range", "length", "type", "chunked", "chunked503", "truncated", "transport", "retry-budget"} {
		t.Run(mode, func(t *testing.T) {
			var first, next atomic.Int64

			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Type", "application/octet-stream")

				if r.Method == "HEAD" {
					w.Header().Set("Content-Length", fmt.Sprint(PageSize+8))
					return
				}

				start, length, _ := objectRange(r.Header, Metadata{Size: PageSize + 8})
				w.Header().Set("Content-Length", fmt.Sprint(length))
				w.Header().Set("Content-Range", contentRange(start, start+length-1, PageSize+8))

				if start < PageSize {
					first.Add(1)
					w.WriteHeader(206)
					_, _ = io.WriteString(w, "12345678")

					return
				}

				next.Add(1)

				switch mode {
				case "auth":
					w.Header().Set("WWW-Authenticate", "Bearer registry")
					w.WriteHeader(401)

					return
				case "version":
					w.Header().Set("ETag", checksumTag([]byte("new")))
				case "range":
					w.Header().Set("Content-Range", "bytes 0-7/8")
				case "length":
					w.Header().Set("Content-Length", "7")
				case "type":
					w.Header().Set("Content-Type", "text/plain")
				case "chunked", "chunked503":
					w.Header().Del("Content-Length")

					status := 206
					if mode == "chunked503" {
						status = 503
					}

					w.WriteHeader(status)
					w.(http.Flusher).Flush()

					return
				case "transport":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}

					_ = conn.Close()

					return
				case "retry-budget":
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(503)

					return
				}

				w.WriteHeader(206)

				if mode == "truncated" {
					_, _ = io.WriteString(w, "short")
				}
			}), ClientOptions{PageLookahead: true})

			s := lookaheadRange(t, c, "/object", PageSize-8, 16)
			if err := s.Prepare(); err != nil {
				t.Fatal("future rejected first page", err)
			}

			var out bytes.Buffer

			_, err := s.WriteTo(&out)

			f := s.Failure()
			if err == nil || first.Load() != 1 || next.Load() != 1 || f == nil || f.PageOffset != PageSize || f.Offset < PageSize || s.Stats().PageRequests != 2 || s.Stats().PageRetries != 0 || len(c.streamPool.speculative) != 0 {
				t.Fatal("replayed or lost future failure", err, f, s.Stats())
			}

			if !bytes.HasPrefix(out.Bytes(), []byte("12345678")) {
				t.Fatal("lost earlier page", out.String())
			}

			if mode == "version" && !errors.Is(err, ErrVersionChanged) || mode == "truncated" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal(err)
			}

			if mode == "auth" {
				var status *HTTPError
				if !errors.As(err, &status) || status.StatusCode != 401 || status.WWWAuthenticate != "Bearer registry" {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestLookaheadConcurrentCloseAndStats(t *testing.T) {
	c := newTestClient(t, lookaheadOrigin(t, 2*PageSize, nil), ClientOptions{PageLookahead: true})
	for range 16 {
		s := lookaheadRange(t, c, "/object", 0, 2*PageSize)
		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Go(func() { _, _ = s.WriteTo(io.Discard) })
		wg.Go(func() { _ = s.Stats() })
		wg.Go(func() { _ = s.Close() })
		wg.Go(func() { _ = s.Close() })
		wg.Wait()

		if s.future != nil || s.page.conn != nil || len(c.streamPool.speculative) != 0 {
			t.Fatal("Close did not join")
		}
	}
}

func TestLookaheadResidualHeaderWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := lookaheadScript(t, t.Context(),
			scriptedPage{body: "12345678", beforeHeaders: func(*http.Request) { time.Sleep(time.Second) }},
			scriptedPage{body: "abcdefgh", beforeHeaders: func(*http.Request) { time.Sleep(5 * time.Second) }})
		dst := &delayedWriter{delay: 2 * time.Second}
		n, err := s.WriteTo(dst)

		stats := s.Stats()
		if err != nil || n != 16 || dst.String() != "12345678abcdefgh" || stats.PageHeaderWait != 4*time.Second || stats.ForwardDuration != 4*time.Second {
			t.Fatal("overlap counted as consumer wait", n, err, stats)
		}
	})
}

func TestLookaheadRetryExhaustion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pages := []scriptedPage{{body: "12345678"}}
		for range pageRetries + 1 {
			pages = append(pages, scriptedPage{status: 503})
		}

		s := lookaheadScript(t, t.Context(), pages...)
		dst := &delayedWriter{delay: 5 * time.Second}
		n, err := s.WriteTo(dst)
		stats, f := s.Stats(), s.Failure()

		var status *HTTPError
		if !errors.As(err, &status) || status.StatusCode != 503 || n != 8 || dst.String() != "12345678" || stats.PageRequests != 6 || stats.PageRetries != 4 || stats.PageHeaderWait != 0 || f.PageOffset != PageSize || f.Offset != PageSize {
			t.Fatal(n, err, stats, f)
		}
	})
}

func TestLookaheadStaleSocketNotReplayed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := lookaheadScript(t, t.Context(), scriptedPage{body: "12345678"})
		client, server := net.Pipe()
		_ = server.Close()
		pool := s.object.client.streamPool
		pool.idle = append([]*streamConn{{Conn: client, reader: bufio.NewReader(client), parser: bufio.NewReader(nil), idle: time.Now()}}, pool.idle...)

		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}

		synctest.Wait()

		var out bytes.Buffer

		n, err := s.WriteTo(&out)

		stats, f := s.Stats(), s.Failure()
		if err == nil || n != 8 || out.String() != "12345678" || stats.PageRequests != 2 || stats.PageRetries != 0 || f.Operation != "page_request" || f.PageOffset != PageSize || f.Offset != PageSize {
			t.Fatal("replayed stale socket", n, err, stats, f)
		}
	})
}

func TestLookaheadAdoptedBodyKeepsFullDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		s := lookaheadScript(t, ctx,
			scriptedPage{body: "12345678", beforeHeaders: func(*http.Request) { time.Sleep(time.Second) }},
			scriptedPage{body: "abcdefgh", beforeBody: func() { <-ctx.Done() }})
		if err := s.Prepare(); err != nil {
			t.Fatal(err)
		}

		deadline, _ := s.ctx.Deadline()

		futureDeadline, _ := s.future.page.ctx.Deadline()
		if deadline != futureDeadline {
			t.Fatal("future refreshed deadline")
		}

		dst := &delayedWriter{delay: time.Second}
		start := time.Now()
		n, err := s.WriteTo(dst)

		var timeout net.Error
		if (!errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout())) || n != 8 || time.Since(start) != 4*time.Second || s.future != nil || s.page.conn != nil || len(s.object.client.streamPool.speculative) != 0 {
			t.Fatal("adopted body outlived deadline", n, err, s.Failure())
		}
	})
}

func TestLookaheadAdoptedOwnerReturnsSocketSafely(t *testing.T) {
	c := newTestClient(t, lookaheadOrigin(t, PageSize+8, nil), ClientOptions{PageLookahead: true})

	s := lookaheadRange(t, c, "/object", PageSize-8, 16)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	f := s.future
	owner := f.page

	var first [8]byte
	if n, err := s.read(first[:]); err != nil || n != len(first) {
		t.Fatal(n, err)
	}

	if err := s.nextPage(); err != nil {
		t.Fatal(err)
	}

	if s.page != owner || f.page != nil || s.future != nil || owner.ctx.Err() != nil || len(c.streamPool.speculative) != 0 {
		t.Fatal("adoption did not transfer a live owner")
	}

	conn := owner.conn

	if n, err := s.WriteTo(&patternWriter{offset: PageSize}); err != nil || n != 8 {
		t.Fatal(n, err)
	}

	if owner.conn != nil || owner.ctx.Err() != context.Canceled || s.Stats().PageRequests != 2 {
		t.Fatal("completed owner retained resources or lost attempts", s.Stats())
	}

	// The adopted socket must remain reusable after its owner and original
	// stream are canceled, even while a new stream is using that exact socket.
	next := lookaheadRange(t, c, "/object", PageSize, 8)
	if err := next.Prepare(); err != nil {
		t.Fatal(err)
	}

	if next.page.conn != conn {
		t.Fatal("adopted socket was not returned to the pool")
	}

	_ = s.Close()

	if n, err := next.WriteTo(&patternWriter{offset: PageSize}); err != nil || n != 8 {
		t.Fatal("old owner canceled a reused socket", n, err)
	}

	if s.Stats().PageRequests != 2 || next.Stats().PageRequests != 1 {
		t.Fatal("ownership retirement counted attempts twice", s.Stats(), next.Stats())
	}
}

func TestLookaheadAdoptedOwnerCancellation(t *testing.T) {
	for _, action := range []string{"close", "cancel"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				body := make(chan struct{})
				defer close(body)

				s := lookaheadScript(t, ctx, scriptedPage{body: "12345678"},
					scriptedPage{body: "abcdefgh", beforeBody: func() { <-body }})
				if err := s.Prepare(); err != nil {
					t.Fatal(err)
				}

				f := s.future
				owner := f.page

				var first [8]byte
				if n, err := s.read(first[:]); err != nil || n != len(first) {
					t.Fatal(n, err)
				}

				if err := s.nextPage(); err != nil {
					t.Fatal(err)
				}

				if s.page != owner || f.page != nil || owner.ctx.Err() != nil {
					t.Fatal("adoption changed ownership or canceled the page")
				}

				done := make(chan error, 1)

				go func() { _, err := s.WriteTo(io.Discard); done <- err }()

				synctest.Wait()

				if action == "close" {
					_ = s.Close()
				} else {
					cancel()
				}

				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal("adopted body did not stop", err)
				}

				failure := s.Failure()
				if owner.conn != nil || owner.ctx.Err() != context.Canceled || failure.Operation != "page_body" || failure.PageOffset != PageSize || failure.Offset != PageSize || failure.StatusCode != 206 || s.Stats().PageRequests != 2 {
					t.Fatal("lost adopted ownership or diagnostics", failure, s.Stats())
				}
			})
		})
	}
}
