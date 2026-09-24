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
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientResourceOptions(t *testing.T) {
	for _, options := range []ClientOptions{
		{Concurrency: -1}, {MaxIdleConnections: -1}, {MaxActiveRequests: -1},
	} {
		if _, err := NewClient("/unused", options); err == nil {
			t.Fatal("accepted negative limit", options)
		}
	}

	for _, tc := range []struct {
		options               ClientOptions
		workers, idle, active int
	}{
		{ClientOptions{}, 8, 8, 0},
		{ClientOptions{Concurrency: 2}, 2, 2, 0},
		{ClientOptions{Concurrency: 2, MaxIdleConnections: 11, MaxActiveRequests: 3}, 2, 11, 3},
	} {
		c, err := NewClient("/unused", tc.options)
		if err != nil {
			t.Fatal(err)
		}

		if c.workers != tc.workers || c.streamPool.limit != tc.idle || c.owned.MaxIdleConns != tc.idle || c.owned.MaxIdleConnsPerHost != tc.idle {
			t.Fatal("resource controls coupled", tc)
		}

		if tc.active == 0 {
			if c.admission != nil {
				t.Fatal("default admission is limited")
			}
		} else if cap(c.admission.slots) != tc.active {
			t.Fatal("incorrect admission capacity")
		}
	}
}

func TestAdmissionSpeculationYields(t *testing.T) {
	a := &requestAdmission{slots: make(chan struct{}, 1)}

	p, err := a.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer p.release()

	if speculative, ok := a.tryAcquire(t.Context()); ok {
		speculative.release()
		t.Fatal("speculation admitted without spare capacity")
	}

	p.release()
	p.release()

	speculative, ok := a.tryAcquire(t.Context())
	if !ok {
		t.Fatal("spare capacity not available")
	}

	speculative.release()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, ok := a.tryAcquire(ctx); ok {
		t.Fatal("canceled speculation admitted")
	}

	if _, err := a.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if len(a.slots) != 0 {
		t.Fatal("permit leaked")
	}
}

func TestAdmissionStreamWaitCancellationAndTimeout(t *testing.T) {
	for _, action := range []string{"close", "timeout"} {
		t.Run(action, func(t *testing.T) {
			options := ClientOptions{MaxActiveRequests: 1}
			if action == "timeout" {
				options.Timeout = 30 * time.Millisecond
			}

			c := newTestClient(t, http.HandlerFunc(admissionFixture), options)

			o, err := c.Open(t.Context(), "/blob")
			if err != nil {
				t.Fatal(err)
			}

			p, err := c.admission.acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer p.release()

			s, err := o.Stream(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			done := make(chan error, 1)

			go func() { done <- s.Prepare() }()

			want := context.DeadlineExceeded

			if action == "close" {
				_ = s.Close()
				want = context.Canceled
			}

			select {
			case err := <-done:
				if !errors.Is(err, want) && (action != "close" || !errors.Is(err, net.ErrClosed)) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("stream admission wait did not stop")
			}

			if action == "timeout" {
				if _, err := c.Stat(t.Context(), "/blob"); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("HTTP timeout excludes admission wait", err)
				}
			}

			if len(c.admission.slots) != 1 {
				t.Fatal("waiter released someone else's permit")
			}

			p.release()
			assertAdmissionFree(t, c)
		})
	}
}

func TestAdmissionStreamReleasesAtPageBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))

		w.Header()["Content-Type"] = nil
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(PageSize+1))
			return
		}

		start, length, status := objectRange(r.Header, Metadata{Size: PageSize + 1})

		end := start + length - 1
		if status != http.StatusPartialContent || start != end {
			t.Error("unexpected boundary request", r.Header.Get("Range"))
		}

		w.Header().Set("Content-Length", "1")
		w.Header().Set("Content-Range", contentRange(start, end, PageSize+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
	}), ClientOptions{MaxActiveRequests: 1})

	o, err := c.Open(ctx, "/blob")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.ReadRange(ctx, PageSize-1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for range 2 {
		var b [1]byte
		if n, err := s.Read(b[:]); n != 1 || err != nil || b[0] != 'x' {
			t.Fatal(n, err, b)
		}
		// No extra Read/EOF/Close should be needed to unblock a foreground HEAD.
		if _, err := c.Stat(ctx, "/blob"); err != nil {
			t.Fatal("consumed page retained admission", err)
		}
	}
}

func TestAdmissionHEADFailuresRelease(t *testing.T) {
	for _, failure := range []string{"dial", "header", "validation", "status"} {
		t.Run(failure, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "header" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}

					_, _ = io.WriteString(conn, "invalid\r\n\r\n")
					_ = conn.Close()

					return
				}

				if failure == "status" {
					w.WriteHeader(http.StatusForbidden)
					return
				}

				w.Header().Set("ETag", "invalid")
				w.Header().Set("Content-Length", "3")
			}), ClientOptions{MaxActiveRequests: 1})

			if failure == "dial" {
				var err error

				c, err = NewClient(filepath.Join(socketDirectory(t), "absent"), ClientOptions{MaxActiveRequests: 1})
				if err != nil {
					t.Fatal(err)
				}
			}

			for range 2 {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				_, err := c.Stat(ctx, "/blob")

				cancel()

				if err == nil || errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("expected immediate HEAD failure", err)
				}

				assertAdmissionFree(t, c)
			}
		})
	}
}

func admissionFixture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", checksumTag([]byte("abc")))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "blob", time.Time{}, strings.NewReader("abc"))
}

func assertAdmissionFree(t *testing.T, c *Client) {
	t.Helper()

	p, ok := c.admission.tryAcquire(t.Context())
	if !ok {
		t.Fatal("request leaked admission")
	}

	p.release()
}

func TestAdmissionHeldUntilConsumedOrAbandoned(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, action := range []string{"consume", "close", "cancel"} {
			t.Run(fmt.Sprintf("raw=%t/%s", raw, action), func(t *testing.T) {
				c := newTestClient(t, http.HandlerFunc(admissionFixture), ClientOptions{MaxActiveRequests: 1})

				view, err := c.WithOriginData([]byte("view"))
				if err != nil {
					t.Fatal(err)
				}

				o, err := view.Open(t.Context(), "/blob")
				if err != nil {
					t.Fatal(err)
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				var body io.ReadCloser

				if raw {
					s, err := o.Stream(ctx)
					if err != nil {
						t.Fatal(err)
					}

					body = s
					if err := s.Prepare(); err != nil {
						t.Fatal(err)
					}
				} else {
					resp, err := o.page(ctx, 0, 2)
					if err != nil {
						t.Fatal(err)
					}

					body = resp.Body
				}

				defer body.Close()

				if p, ok := c.admission.tryAcquire(t.Context()); ok {
					p.release()
					t.Fatal("headers released permit before body consumption")
				}
				// A canceled waiter must neither dispatch nor steal this permit.
				wait, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
				defer stop()

				if _, err := c.Stat(wait, "/blob"); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("admission wait ignored context", err)
				}

				c.CloseIdleConnections()

				switch action {
				case "consume":
					data := make([]byte, 3)
					if _, err := io.ReadFull(body, data); err != nil || string(data) != "abc" {
						t.Fatal("idle cleanup interrupted active response", string(data), err)
					}
				case "close":
					if err := body.Close(); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				}
				// Cancellation must release even when the caller never reads again.
				next, done := context.WithTimeout(t.Context(), time.Second)
				defer done()

				if _, err := c.Stat(next, "/blob"); err != nil {
					t.Fatal("admission not released", err)
				}

				assertAdmissionFree(t, c)
			})
		}
	}
}

func TestAdmissionMixedOperationsAcrossViews(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		gated        atomic.Bool
		active, peak atomic.Int32
	)

	arrived := make(chan struct{}, 16)
	resume := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gated.Load() {
			n := active.Add(1)
			for old := peak.Load(); old < n; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}

			arrived <- struct{}{}

			select {
			case <-resume:
			case <-r.Context().Done():
			}

			active.Add(-1)
		}

		admissionFixture(w, r)
	}), ClientOptions{Concurrency: 8, MaxIdleConnections: 5, MaxActiveRequests: 2})

	view, err := c.WithOriginData([]byte("view"))
	if err != nil {
		t.Fatal(err)
	}

	o, err := view.Open(ctx, "/blob")
	if err != nil {
		t.Fatal(err)
	}

	gated.Store(true)

	results := make(chan error, 12)

	for i := range 12 {
		go func() {
			var err error

			switch i % 4 {
			case 0:
				_, err = c.Download(ctx, "/blob", make(sliceWriter, 3))
			case 1:
				_, err = view.Stat(ctx, "/blob")
			case 2:
				var data [3]byte

				_, err = o.ReadAt(ctx, data[:], 0)
				if err == nil && string(data[:]) != "abc" {
					err = errors.New("incorrect read")
				}
			case 3:
				var s *Stream

				s, err = o.Stream(ctx)
				if err == nil {
					var data []byte

					data, err = io.ReadAll(s)
					_ = s.Close()

					if err == nil && string(data) != "abc" {
						err = errors.New("incorrect stream")
					}
				}
			}

			results <- err
		}()
	}
	// Hold both admitted requests in the server while every other operation
	// contends across the two transport paths, then let all of them complete.
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			t.Fatal("requests did not reach configured parallelism")
		}
	}

	select {
	case <-arrived:
		t.Error("third request bypassed shared admission")
	case <-time.After(30 * time.Millisecond):
	}

	close(resume)

	for range 12 {
		select {
		case err := <-results:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Fatal("operations failed to finish")
		}
	}

	if peak.Load() != 2 {
		t.Fatal("incorrect peak", peak.Load())
	}

	if len(c.admission.slots) != 0 {
		t.Fatal("permits leaked")
	}
}

func TestAdmissionFailuresRelease(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, failure := range []string{"dial", "header", "validation", "body", "destination"} {
			t.Run(fmt.Sprintf("raw=%t/%s", raw, failure), func(t *testing.T) {
				c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if failure == "header" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}

						_, _ = io.WriteString(conn, "invalid\r\n\r\n")
						_ = conn.Close()

						return
					}

					w.Header().Set("ETag", checksumTag([]byte("abc")))
					w.Header().Set("Content-Length", "3")

					w.Header()["Content-Type"] = nil
					if failure == "validation" {
						w.Header().Set("ETag", checksumTag(nil))
					}

					if failure == "body" {
						_, _ = io.WriteString(w, "a")
					} else {
						_, _ = io.WriteString(w, "abc")
					}
				}), ClientOptions{MaxActiveRequests: 1})

				if failure == "dial" {
					var err error

					c, err = NewClient(filepath.Join(socketDirectory(t), "absent"), ClientOptions{MaxActiveRequests: 1})
					if err != nil {
						t.Fatal(err)
					}
				}

				o := &Object{client: c, target: "/blob", meta: Metadata{Size: 3, ETag: checksumTag([]byte("abc"))}}

				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()

				for range 2 {
					var err error

					if raw {
						s, openErr := o.Stream(ctx)
						if openErr != nil {
							t.Fatal(openErr)
						}

						dst := io.Discard
						if failure == "destination" {
							dst = admissionFailWriter{}
						}

						_, err = s.WriteTo(dst)
						_ = s.Close()
					} else {
						var dst io.WriterAt = make(sliceWriter, 3)
						if failure == "destination" {
							dst = failingWriter{io.ErrClosedPipe}
						}

						_, err = o.Download(ctx, dst)
					}

					if err == nil || errors.Is(err, context.DeadlineExceeded) {
						t.Fatal("expected immediate transfer failure", err)
					}

					assertAdmissionFree(t, c)
				}
			})
		}
	}
}

type admissionFailWriter struct{}

func (admissionFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestIndependentIdleCapacityReusesBurst(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprintf("raw=%t", raw), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			var accepts atomic.Int32

			arrived := make(chan struct{}, 3)
			resume := make(chan struct{}, 3)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- struct{}{}

				select {
				case <-resume:
				case <-r.Context().Done():
					return
				}

				admissionFixture(w, r)
			}))
			_ = server.Listener.Close()

			listener, err := net.Listen("unix", filepath.Join(socketDirectory(t), "burst"))
			if err != nil {
				t.Fatal(err)
			}

			server.Listener = listener
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					accepts.Add(1)
				}
			}

			server.Start()
			defer server.Close()

			c, err := NewClient(listener.Addr().String(), ClientOptions{Concurrency: 1, MaxIdleConnections: 3})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseIdleConnections()

			o := &Object{client: c, target: "/blob", meta: Metadata{Size: 3, ETag: checksumTag([]byte("abc")), ContentType: "application/octet-stream"}}

			for range 2 {
				var wg sync.WaitGroup
				for range 3 {
					wg.Go(func() {
						if raw {
							s, err := o.Stream(ctx)
							if err != nil {
								t.Error(err)
								return
							}
							defer s.Close()

							data, err := io.ReadAll(s)
							if err != nil || !bytes.Equal(data, []byte("abc")) {
								t.Error(string(data), err)
							}
						} else if _, err := c.Stat(ctx, "/blob"); err != nil {
							t.Error(err)
						}
					})
				}

				for range 3 {
					select {
					case <-arrived:
					case <-ctx.Done():
						t.Fatal("burst blocked by per-operation worker or idle limit")
					}
				}

				for range 3 {
					resume <- struct{}{}
				}

				wg.Wait()

				if accepts.Load() != 3 {
					t.Fatal("burst did not reuse accepted sockets", accepts.Load())
				}
			}
		})
	}
}

type blockingCloseConn struct {
	net.Conn
	started chan struct{}
	resume  chan struct{}
}

func (c *blockingCloseConn) Close() error {
	close(c.started)
	<-c.resume

	return nil
}

func (*blockingCloseConn) SetDeadline(time.Time) error { return nil }

func TestStreamPoolClosesOutsideMutex(t *testing.T) {
	for _, action := range []string{"expired", "overflow", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			conn := &blockingCloseConn{started: make(chan struct{}), resume: make(chan struct{})}

			var once sync.Once

			unblock := func() { once.Do(func() { close(conn.resume) }) }
			defer unblock()

			socket := &streamConn{Conn: conn, reader: bufio.NewReader(strings.NewReader(""))}
			pool := &streamPool{endpoint: filepath.Join(socketDirectory(t), "absent")}
			done := make(chan struct{})

			go func() {
				defer close(done)

				switch action {
				case "expired":
					pool.idle = []*streamConn{socket}
					_, _ = pool.get(t.Context())
				case "overflow":
					pool.put(socket)
				case "cleanup":
					pool.idle = []*streamConn{socket}
					pool.closeIdle()
				}
			}()

			select {
			case <-conn.started:
			case <-time.After(time.Second):
				t.Fatal("close not reached")
			}

			locked := make(chan struct{})

			go func() {
				pool.mu.Lock()
				if len(pool.idle) != 0 {
					t.Error("closing socket retained in idle pool")
				}
				pool.mu.Unlock()
				close(locked)
			}()

			select {
			case <-locked:
			case <-time.After(time.Second):
				t.Error("socket close held pool mutex")
			}

			unblock()
			<-done
			<-locked
		})
	}
}
