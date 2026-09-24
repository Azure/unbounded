// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// Small first/last ranges straddle real page boundaries. The middle page is
// generated in bounded chunks, including nonconstant bytes to check ordering.
func prefetchFixture(t *testing.T, size int64, before func(http.ResponseWriter, *http.Request, int64) bool) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Type", "application/octet-stream")

		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(size))
			return
		}

		start, length, status := objectRange(r.Header, Metadata{Size: size})
		if status != 206 || start/PageSize != (start+length-1)/PageSize || r.Header.Get("If-Match") != checksumTag(nil) {
			t.Error("invalid pinned page request", r.Header)
			w.WriteHeader(416)

			return
		}

		w.Header().Set("Content-Length", fmt.Sprint(length))
		w.Header().Set("Content-Range", contentRange(start, start+length-1, size))

		if before != nil && !before(w, r, start) {
			return
		}

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

func openPrefetchRange(t *testing.T, c *Client, ctx context.Context, start, length int64) *Stream {
	t.Helper()

	o, err := c.Open(ctx, "/blob")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(ctx, start, length)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}

func waitPrefetch(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("prefetch synchronization timed out")
	}
}

func TestStreamPrefetchOverlapAndBounds(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			first, second := make(chan struct{}), make(chan struct{})
			resume := make(chan struct{})

			var once sync.Once

			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()

			c := newTestClient(t, prefetchFixture(t, PageSize+100, func(w http.ResponseWriter, r *http.Request, start int64) bool {
				if data, status := decodeOriginData(r.Header); status != 0 || string(data) != "prefetch-view" {
					t.Error("lost origin-data view")
				}

				if start < PageSize {
					w.WriteHeader(206)
					w.(http.Flusher).Flush()
					close(first)

					select {
					case <-resume:
					case <-r.Context().Done():
						return false
					}
				} else {
					if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", PageSize, PageSize+6) {
						t.Error("prefetch exceeded partial range", r.Header.Get("Range"))
					}

					close(second)
				}

				return true
			}), ClientOptions{StreamPrefetch: enabled, MaxActiveRequests: 2})

			view, err := c.WithOriginData([]byte("prefetch-view"))
			if err != nil {
				t.Fatal(err)
			}

			s := openPrefetchRange(t, view, t.Context(), PageSize-3, 10)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}

			waitPrefetch(t, first)

			if enabled {
				waitPrefetch(t, second) // Current body cannot finish until resume.
			} else {
				select {
				case <-second:
					t.Fatal("default stream speculated")
				case <-time.After(20 * time.Millisecond):
				}
			}

			unblock()

			got, err := io.ReadAll(s)

			want := make([]byte, 10)
			for i := range want {
				want[i] = byte((PageSize - 3 + int64(i)) % 251)
			}

			if err != nil || !bytes.Equal(got, want) {
				t.Fatal(got, want, err)
			}

			if len(c.admission.slots) != 0 {
				t.Fatal("permits leaked")
			}
		})
	}
}

type prefetchVerifier struct{ offset int64 }

func (v *prefetchVerifier) Write(b []byte) (int, error) {
	for i, value := range b {
		if value != byte((v.offset+int64(i))%251) {
			return i, fmt.Errorf("incorrect byte at %d", v.offset+int64(i))
		}
	}

	v.offset += int64(len(b))

	return len(b), nil
}

func TestStreamPrefetchOnePageAhead(t *testing.T) {
	requests := make(chan int64, 4)
	c := newTestClient(t, prefetchFixture(t, 2*PageSize+100, func(_ http.ResponseWriter, _ *http.Request, start int64) bool {
		requests <- start
		return true
	}), ClientOptions{StreamPrefetch: true})

	s := openPrefetchRange(t, c, t.Context(), PageSize-3, PageSize+10)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	for _, want := range []int64{PageSize - 3, PageSize} {
		select {
		case got := <-requests:
			if got != want {
				t.Fatal(got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing request")
		}
	}

	select {
	case off := <-requests:
		t.Fatal("more than one page ahead", off)
	case <-time.After(20 * time.Millisecond):
	}

	v := &prefetchVerifier{offset: PageSize - 3}
	if n, err := s.WriteTo(v); n != PageSize+10 || err != nil {
		t.Fatal(n, err)
	}

	if off := <-requests; off != 2*PageSize {
		t.Fatal(off)
	}
}

func TestStreamPrefetchDeferredFailures(t *testing.T) {
	for _, failure := range []string{"etag", "type", "range", "length", "status", "header", "body"} {
		t.Run(failure, func(t *testing.T) {
			c := newTestClient(t, prefetchFixture(t, PageSize+3, func(w http.ResponseWriter, _ *http.Request, start int64) bool {
				if start < PageSize {
					return true
				}

				switch failure {
				case "etag":
					w.Header().Set("ETag", checksumTag([]byte("other")))
				case "type":
					w.Header().Set("Content-Type", "text/plain")
				case "range":
					w.Header().Set("Content-Range", "bytes 0-2/3")
				case "length":
					w.Header().Set("Content-Length", "2")
				case "status":
					w.WriteHeader(403)
					return false
				case "header":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return false
					}

					_, _ = io.WriteString(conn, "invalid\r\n\r\n")
					_ = conn.Close()

					return false
				case "body":
					w.WriteHeader(206)
					w.(http.Flusher).Flush()

					return false
				}

				return true
			}), ClientOptions{StreamPrefetch: true, MaxActiveRequests: 2})

			s := openPrefetchRange(t, c, t.Context(), PageSize-3, 6)
			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}
			// Wait until preparation completes to force the error to precede the
			// successful current-page read, rather than rely on scheduling.
			waitPrefetch(t, s.next.ready)

			got := make([]byte, 3)
			if n, err := io.ReadFull(s, got); n != 3 || err != nil {
				t.Fatal("speculative error truncated current page", n, err)
			}

			if n, err := s.Read(got); n != 0 || err == nil {
				t.Fatal("missing deferred error", n, err)
			} else {
				switch failure {
				case "etag":
					if !errors.Is(err, ErrVersionChanged) {
						t.Fatal(err)
					}
				case "type", "range", "length":
					if !errors.Is(err, ErrProtocol) {
						t.Fatal(err)
					}
				case "body":
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatal(err)
					}
				case "status":
					var status *HTTPError
					if !errors.As(err, &status) || status.StatusCode != 403 {
						t.Fatal(err)
					}
				}
			}

			if len(c.admission.slots) != 0 {
				t.Fatal("failure leaked permits")
			}
		})
	}
}

func TestStreamPrefetchCleanup(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		for _, action := range []string{"cancel", "close", "timeout", "idle", "destination"} {
			t.Run(fmt.Sprintf("prepared=%t/%s", prepared, action), func(t *testing.T) {
				arrived, disconnected := make(chan struct{}), make(chan struct{})

				options := ClientOptions{StreamPrefetch: true, MaxActiveRequests: 2}
				if action == "timeout" {
					options.Timeout = 200 * time.Millisecond
				}

				var (
					requests int
					mu       sync.Mutex
				)

				c := newTestClient(t, prefetchFixture(t, PageSize+3, func(w http.ResponseWriter, r *http.Request, start int64) bool {
					if start < PageSize {
						return true
					}

					mu.Lock()
					requests++
					first := requests == 1
					mu.Unlock()

					if !first {
						return true
					}

					if prepared {
						w.WriteHeader(206)
						w.(http.Flusher).Flush()
					}

					close(arrived)
					<-r.Context().Done()
					close(disconnected)

					return false
				}), options)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				s := openPrefetchRange(t, c, ctx, PageSize-3, 6)
				if err := s.Prepare(); err != nil {
					t.Fatal(err)
				}

				waitPrefetch(t, arrived)

				if prepared {
					waitPrefetch(t, s.next.ready)
				}

				want := error(context.Canceled)

				switch action {
				case "cancel":
					cancel()
				case "close":
					_ = s.Close()
					want = net.ErrClosed
				case "timeout":
					<-s.ctx.Done()

					want = context.DeadlineExceeded
				case "idle":
					c.CloseIdleConnections()
				case "destination":
					if _, err := s.WriteTo(admissionFailWriter{}); !errors.Is(err, io.ErrClosedPipe) {
						t.Fatal(err)
					}

					want = io.ErrClosedPipe
				}

				waitPrefetch(t, disconnected)

				if action == "cancel" || action == "timeout" {
					// Cancellation alone must free both permits without Read/Close.
					wait, stop := context.WithTimeout(t.Context(), time.Second)
					defer stop()

					p1, err := c.admission.acquire(wait)
					if err != nil {
						t.Fatal(err)
					}
					defer p1.release()

					p2, err := c.admission.acquire(wait)
					if err != nil {
						t.Fatal(err)
					}

					p1.release()
					p2.release()
				}

				if action == "idle" {
					got, err := io.ReadAll(s)
					if err != nil || len(got) != 6 {
						t.Fatal(got, err)
					}
				} else if _, err := s.Read(make([]byte, 1)); !errors.Is(err, want) {
					t.Fatal(err, want)
				}

				_ = s.Close()

				if len(c.admission.slots) != 0 {
					t.Fatal("cleanup leaked permits")
				}

				c.streamPool.mu.Lock()
				pending := len(c.streamPool.pending)
				c.streamPool.mu.Unlock()

				if pending != 0 {
					t.Fatal("pending registration leaked")
				}
			})
		}
	}
}

func TestStreamPrefetchCompetingAdmission(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			c := newTestClient(t, prefetchFixture(t, PageSize+3, nil), ClientOptions{StreamPrefetch: true, MaxActiveRequests: limit})

			o, err := c.Open(ctx, "/blob")
			if err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			for range 12 {
				wg.Go(func() {
					s, err := o.ReadRange(ctx, PageSize-3, 6)
					if err != nil {
						t.Error(err)
						return
					}
					defer s.Close()

					if err := s.Prepare(); err != nil {
						t.Error(err)
						return
					}

					if limit == 1 && s.next != nil {
						t.Error("speculation with no spare capacity")
					}

					v := &prefetchVerifier{offset: PageSize - 3}
					if n, err := s.WriteTo(v); n != 6 || err != nil {
						t.Error(n, err)
					}
				})
			}

			wg.Wait()

			if len(c.admission.slots) != 0 {
				t.Fatal("permits leaked")
			}
		})
	}
}

func TestStreamPrefetchIdleCleanupDuringAdoption(t *testing.T) {
	arrived, disconnected := make(chan struct{}), make(chan struct{})

	var once sync.Once

	c := newTestClient(t, prefetchFixture(t, PageSize+3, func(_ http.ResponseWriter, r *http.Request, start int64) bool {
		if start < PageSize {
			return true
		}

		blocked := false

		once.Do(func() { blocked = true })

		if !blocked {
			return true
		}

		close(arrived)
		<-r.Context().Done()
		close(disconnected)

		return false
	}), ClientOptions{StreamPrefetch: true, MaxActiveRequests: 2})

	s := openPrefetchRange(t, c, t.Context(), PageSize-3, 6)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}

	waitPrefetch(t, arrived)

	if _, err := io.ReadFull(s, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)

	go func() { _, err := io.ReadAll(s); result <- err }()
	// Cleanup must remain able to cancel headers even while adoption waits.
	done := make(chan struct{})

	go func() { c.CloseIdleConnections(); close(done) }()

	waitPrefetch(t, done)
	waitPrefetch(t, disconnected)

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground adoption deadlocked cleanup")
	}

	if len(c.admission.slots) != 0 {
		t.Fatal("permits leaked")
	}
}

func TestStreamPrefetchCancelBeforeHandoff(t *testing.T) {
	c, err := NewClient("/unused", ClientOptions{StreamPrefetch: true, MaxActiveRequests: 1})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s := &Stream{object: &Object{client: c}, ctx: ctx, cancel: cancel, pageEnd: PageSize, end: PageSize + 1}
	c.streamPool.mu.Lock() // Hold publication after the speculative permit acquisition.
	done := make(chan struct{})

	go func() { s.startPrefetch(); close(done) }()

	deadline := time.After(5 * time.Second)

	for len(c.admission.slots) == 0 {
		select {
		case <-deadline:
			c.streamPool.mu.Unlock()
			t.Fatal("permit not acquired")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	c.streamPool.mu.Unlock()
	waitPrefetch(t, done)

	_ = s.Close()

	if len(c.admission.slots) != 0 {
		t.Fatal("pre-handoff cancellation leaked permit")
	}
}
