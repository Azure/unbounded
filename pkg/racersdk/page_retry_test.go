// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPageRetryPinnedFirstAndLaterPage(t *testing.T) {
	for _, status := range []int{429, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var (
				head, first, second atomic.Int32
				last                time.Time
			)

			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Type", "application/octet-stream")

				if r.Method == "HEAD" {
					head.Add(1)
					w.Header().Set("Content-Length", fmt.Sprint(PageSize+8))

					return
				}

				if r.RequestURI != "/pinned?x=1&x=2" || r.Header.Get("If-Match") != checksumTag(nil) || r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("request identity changed", r.RequestURI, r.Header)
				}

				start, end, body, count := PageSize-8, PageSize-1, "12345678", &first
				if r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", PageSize, PageSize+7) {
					start, end, body, count = PageSize, PageSize+7, "abcdefgh", &second
				} else if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", start, end) {
					t.Error("range changed", r.Header)
				}

				if count.Add(1) == 1 {
					last = time.Now()

					w.Header().Set("Content-Length", "6")
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "reject")

					return
				}

				if time.Since(last) < 100*time.Millisecond {
					t.Error("retry hot loop")
				}

				w.Header().Set("Content-Length", "8")
				w.Header().Set("Content-Range", contentRange(start, end, PageSize+8))
				w.WriteHeader(206)
				_, _ = io.WriteString(w, body)
			}), ClientOptions{})

			o, err := c.Open(t.Context(), "/pinned?x=1&x=2")
			if err != nil {
				t.Fatal(err)
			}

			s, err := o.ReadRange(t.Context(), PageSize-8, 16)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			if err := s.Prepare(); err != nil {
				t.Fatal(err)
			}

			var body bytes.Buffer

			n, err := s.WriteTo(&body)
			if err != nil || n != 16 || body.String() != "12345678abcdefgh" || head.Load() != 1 || first.Load() != 2 || second.Load() != 2 || s.Failure() != nil {
				t.Fatal(n, err, body.String(), head.Load(), first.Load(), second.Load(), s.Failure())
			}
		})
	}
}

func TestPageRetryTerminalResponses(t *testing.T) {
	for _, mode := range []string{"401", "403", "404", "412", "500", "etag", "range", "truncated", "chunked503", "bad-retry-after", "long-retry-after"} {
		t.Run(mode, func(t *testing.T) {
			var gets atomic.Int32

			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "8")

				if r.Method == "HEAD" {
					return
				}

				gets.Add(1)
				w.Header().Set("Content-Range", "bytes 0-7/8")

				switch mode {
				case "etag":
					w.Header().Set("ETag", checksumTag([]byte("changed")))
				case "range":
					w.Header().Set("Content-Range", "bytes 1-8/9")
				case "truncated":
					w.WriteHeader(206)
					_, _ = io.WriteString(w, "short")

					return
				case "chunked503":
					w.Header().Del("Content-Length")
					w.WriteHeader(503)
					w.(http.Flusher).Flush()

					return
				case "bad-retry-after", "long-retry-after":
					hint := "not-a-date"
					if mode == "long-retry-after" {
						hint = "60"
					}

					w.Header().Set("Retry-After", hint)
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(503)

					return
				default:
					var status int

					_, _ = fmt.Sscan(mode, &status)

					w.Header().Set("Content-Length", "0")
					w.WriteHeader(status)

					return
				}

				w.WriteHeader(206)
				_, _ = io.WriteString(w, "12345678")
			}), ClientOptions{})

			o, err := c.Open(t.Context(), "/object")
			if err != nil {
				t.Fatal(err)
			}

			s, err := o.Stream(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			_, err = s.WriteTo(io.Discard)
			if err == nil || gets.Load() != 1 || s.conn != nil || len(c.streamPool.idle) != 0 {
				t.Fatal("retried terminal failure or retained connection", err, gets.Load())
			}
		})
	}
}

func TestPageRetryHintAndTotalDeadline(t *testing.T) {
	for _, mode := range []string{"hint", "budget", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			var gets atomic.Int32

			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "8")

				if r.Method == "HEAD" {
					return
				}

				n := gets.Add(1)
				if mode == "deadline" && n > 1 {
					<-r.Context().Done()
					return
				}

				if n == 1 {
					w.Header().Set("Content-Length", "0")

					if mode != "deadline" {
						w.Header().Set("Retry-After", "1")
					}

					w.WriteHeader(503)

					return
				}

				w.Header().Set("Content-Range", "bytes 0-7/8")
				w.WriteHeader(206)
				_, _ = io.WriteString(w, "12345678")
			}), ClientOptions{})

			o, err := c.Open(t.Context(), "/object")
			if err != nil {
				t.Fatal(err)
			}

			budget := 3 * time.Second
			if mode != "hint" {
				budget = 350 * time.Millisecond
			}

			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()

			s, err := o.Stream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			start := time.Now()
			n, err := s.WriteTo(io.Discard)

			switch mode {
			case "hint":
				if err != nil || n != 8 || gets.Load() != 2 || time.Since(start) < time.Second {
					t.Fatal(n, err, gets.Load(), time.Since(start))
				}
			case "budget":
				var status *HTTPError
				if !errors.As(err, &status) || status.StatusCode != 503 || gets.Load() != 1 {
					t.Fatal(err, gets.Load())
				}
			case "deadline":
				var timeout interface{ Timeout() bool }

				deadline, _ := ctx.Deadline()

				streamDeadline, _ := s.ctx.Deadline()
				if (!errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &timeout) || !timeout.Timeout())) || gets.Load() != 2 || time.Since(start) > time.Second || s.conn != nil || len(c.streamPool.idle) != 0 || deadline != streamDeadline {
					t.Fatal(err, gets.Load(), time.Since(start))
				}
			}
		})
	}
}

func TestPageRetryDelayBounds(t *testing.T) {
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for _, hint := range []string{"", "0", "1", now.Add(time.Second).Format(http.TimeFormat), now.Add(-time.Second).Format(http.TimeFormat)} {
		for retry := range pageRetries {
			d, ok := pageRetryDelay(hint, retry, now)

			base := 100 * time.Millisecond << retry
			if !ok || d < base || d > 2*time.Second || ((hint == "1" || hint == now.Add(time.Second).Format(http.TimeFormat)) && d < time.Second) {
				t.Fatal(hint, retry, d, ok)
			}
		}
	}

	for _, hint := range []string{"-1", "nonsense", "999999999999999999999999999999", "6", now.Add(time.Hour).Format(http.TimeFormat)} {
		if d, ok := pageRetryDelay(hint, 0, now); ok {
			t.Fatal(hint, d)
		}
	}
}

func TestPageRetryCumulativeHintBudget(t *testing.T) {
	var gets atomic.Int32

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))
		w.Header().Set("Content-Length", "8")

		if r.Method == "HEAD" {
			return
		}

		gets.Add(1)
		w.Header().Set("Content-Length", "0")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(503)
	}), ClientOptions{Timeout: 10 * time.Second})

	o, err := c.Open(t.Context(), "/object")
	if err != nil {
		t.Fatal(err)
	}

	s, err := o.Stream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	start := time.Now()
	err = s.Prepare()

	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != 503 || gets.Load() != 3 || time.Since(start) < 4*time.Second || time.Since(start) > 6*time.Second || s.conn != nil {
		t.Fatal("hint budget reset, ignored, or lost terminal status", err, gets.Load(), time.Since(start))
	}
}

func TestPageRetryClientTimeoutAndContextCancel(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelContext), func(t *testing.T) {
			waiting := make(chan struct{}, 1)

			var gets atomic.Int32

			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Length", "8")

				if r.Method == "HEAD" {
					return
				}

				n := gets.Add(1)
				if n == 1 {
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(503)
					w.(http.Flusher).Flush()
					<-r.Context().Done()

					waiting <- struct{}{}

					return
				}

				<-r.Context().Done()
			}), ClientOptions{Timeout: 500 * time.Millisecond})

			o, err := c.Open(t.Context(), "/object")
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			s, err := o.Stream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			start := time.Now()

			done := make(chan error, 1)
			go func() { done <- s.Prepare() }()

			select {
			case <-waiting:
			case <-time.After(time.Second):
				t.Fatal("did not close rejected connection")
			}

			if cancelContext {
				cancel()
			}

			select {
			case err := <-done:
				if err == nil || time.Since(start) > time.Second || s.conn != nil || len(c.streamPool.idle) != 0 {
					t.Fatal(err)
				}

				if cancelContext && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}

				if !cancelContext && gets.Load() != 2 {
					t.Fatal(gets.Load())
				}
			case <-time.After(time.Second):
				t.Fatal("operation budget refreshed")
			}
		})
	}
}
