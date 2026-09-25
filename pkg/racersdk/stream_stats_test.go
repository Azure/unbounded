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
	"testing"
	"testing/synctest"
	"time"
)

// Scripted pages run over an in-memory HTTP connection so synctest controls time.
// Hooks can sleep or wait on channels to independently gate headers and payload.
// Real UDS/splice coverage remains in splice_linux_test.go.
type scriptedPage struct {
	beforeHeaders func(*http.Request)
	beforeBody    func()
	status        int
	body          string
}

func scriptedStream(t *testing.T, ctx context.Context, pages ...scriptedPage) *Stream {
	t.Helper()

	client, server := net.Pipe()
	done := make(chan struct{})

	t.Cleanup(func() { _ = server.Close(); <-done })
	t.Cleanup(func() { _ = client.Close() })

	pool := &streamPool{limit: 1, idle: []*streamConn{{
		Conn: client, reader: bufio.NewReader(client), parser: bufio.NewReader(nil), idle: time.Now(),
	}}}
	c := &Client{endpoint: "http://localhost", header: make(http.Header), streamPool: pool}
	t.Cleanup(c.CloseIdleConnections)

	// A short interval crossing a real page boundary avoids page-sized fixtures.
	o := &Object{client: c, target: "/scripted", meta: Metadata{Size: PageSize + 8, ETag: checksumTag(nil)}}

	s, err := o.ReadRange(ctx, PageSize-8, int64(len(pages))*8)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = s.Close() })

	go func() {
		defer close(done)
		defer server.Close()

		reader := bufio.NewReader(server)
		for i, page := range pages {
			r, err := http.ReadRequest(reader)
			if err != nil {
				t.Error(err)
				return
			}

			start := PageSize - 8 + int64(i)*8
			if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", start, start+7) || r.Header.Get("If-Match") != o.meta.ETag {
				t.Error("lost pinned page identity", r.Header)
			}

			if page.beforeHeaders != nil {
				page.beforeHeaders(r)
			}

			status := page.status
			if status == 0 {
				status = 206
			}

			_, err = fmt.Fprintf(server, "HTTP/1.1 %d %s\r\nETag: %s\r\nContent-Length: 8\r\nContent-Range: bytes %d-%d/%d\r\n\r\n", status, http.StatusText(status), o.meta.ETag, start, start+7, o.meta.Size)
			if err != nil {
				return // Cancellation may discard a delayed response.
			}

			if page.beforeBody != nil {
				page.beforeBody()
			}

			if _, err := io.WriteString(server, page.body); err != nil {
				return
			}
		}
	}()

	return s
}

type delayedWriter struct {
	bytes.Buffer
	delay time.Duration
	err   error
}

func (w *delayedWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)

	if w.err != nil {
		return 0, w.err
	}

	return w.Buffer.Write(p)
}

func TestTransferStatsSeparateHeadersAndForwarding(t *testing.T) {
	for _, prepare := range []bool{false, true} {
		t.Run(fmt.Sprint(prepare), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				page := scriptedPage{
					beforeHeaders: func(*http.Request) { time.Sleep(3 * time.Second) },
					beforeBody:    func() { time.Sleep(2 * time.Second) },
					body:          "12345678",
				}

				s := scriptedStream(t, t.Context(), page, page)
				if prepare {
					if err := s.Prepare(); err != nil {
						t.Fatal(err)
					}

					if err := s.Prepare(); err != nil {
						t.Fatal(err)
					}

					stats := s.Stats()
					if stats.PageRequests != 1 || stats.PageHeaderWait != 3*time.Second || stats.ForwardDuration != 0 {
						t.Fatal("Prepare must count only once", stats)
					}
				}

				dst := &delayedWriter{delay: 5 * time.Second}

				n, err := s.WriteTo(dst)
				if err != nil || n != 16 || dst.String() != "1234567812345678" {
					t.Fatal(n, err, dst.String())
				}

				stats := s.Stats()
				if stats.PageRequests != 2 || stats.PageRetries != 0 || stats.PageHeaderWait != 6*time.Second || stats.ForwardDuration != 14*time.Second || stats.BufferedBytes != 16 {
					t.Fatal("header/body/backpressure attribution", stats)
				}

				time.Sleep(7 * time.Second)

				if n, err := s.WriteTo(io.Discard); n != 0 || err != nil {
					t.Fatal(n, err)
				}

				_ = s.Close()
				if s.Stats() != stats {
					t.Fatal("idle time or cleanup changed stats", s.Stats(), stats)
				}
			})
		})
	}
}

func TestTransferStatsFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"headers", "downstream", "cancel", "retry_cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				page := scriptedPage{body: "12345678", beforeHeaders: func(*http.Request) { time.Sleep(3 * time.Second) }}
				dst := &delayedWriter{}

				switch mode {
				case "headers":
					page.status = 403
				case "downstream":
					dst.delay, dst.err = 5*time.Second, io.ErrClosedPipe
				case "cancel":
					go func() { time.Sleep(time.Second); cancel() }()
				case "retry_cancel":
					page.status = 503

					go func() { time.Sleep(3*time.Second + 50*time.Millisecond); cancel() }()
				}

				s := scriptedStream(t, ctx, page)

				n, err := s.WriteTo(dst)
				if err == nil || n != 0 {
					t.Fatal(n, err)
				}

				wantHeaders, wantForward := 3*time.Second, time.Duration(0)
				if mode == "cancel" {
					wantHeaders = time.Second

					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				}

				if mode == "downstream" {
					wantForward = 5 * time.Second
				}

				if mode == "retry_cancel" {
					wantHeaders = 3*time.Second + 50*time.Millisecond

					if !errors.Is(err, context.Canceled) || s.Failure().Operation != "page_retry_wait" {
						t.Fatal("canceled backoff", err, s.Failure())
					}
				}

				stats := s.Stats()
				if stats.PageRequests != 1 || stats.PageRetries != 0 || stats.PageHeaderWait != wantHeaders || stats.ForwardDuration != wantForward {
					t.Fatal("failure attribution", stats)
				}
			})
		})
	}
}
