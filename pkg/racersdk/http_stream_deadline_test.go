// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Model a destination with a shorter write budget than the source read budget.
// Real HTTP/2 expires that budget by resetting the stream, even when no Write
// is in progress. Clearing the deadline must reach the actual HTTP/2 writer.
type streamingShortDeadline struct{ http.ResponseWriter }

func (w streamingShortDeadline) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() && d.After(time.Now()) {
		d = time.Now().Add(50 * time.Millisecond)
	}

	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(d)
}

func TestStreamingHTTP2UpstreamWaits(t *testing.T) {
	for _, phase := range []string{"first", "body", "page", "Complete"} {
		t.Run(phase, func(t *testing.T) {
			const size = uint64(PageSize) + 3

			pause := func(at string) {
				if at == phase {
					time.Sleep(200 * time.Millisecond)
				}
			}
			c := rawSubscriptionClient(t, func(conn net.Conn, reader *bufio.Reader, _ []byte) {
				_, _ = io.WriteString(conn, subscriptionHead(size, 0, size))

				pause("first")

				_ = fakeSubscriptionFrame(conn, 1, 0, 0, uint32(PageSize))
				_, _ = io.CopyN(conn, &offsetStream{}, copyBufferSize)

				pause("body")

				_, _ = io.CopyN(conn, &offsetStream{offset: copyBufferSize}, int64(PageSize)-copyBufferSize)
				if !orderedRelease(t, reader, 0, uint32(PageSize)) {
					return
				}

				pause("page")

				_ = fakeSubscriptionFrame(conn, 1, 1, uint64(PageSize), 3)
				_, _ = io.CopyN(conn, &offsetStream{offset: int64(PageSize)}, 3)

				if !orderedRelease(t, reader, 1, 3) {
					return
				}

				pause("Complete")

				_ = fakeSubscriptionFrame(conn, 2, 2, size, 0)
			})
			c.config.BodyReadTimeout = 3 * time.Second
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				v, err := c.GetStreaming(r.Context(), Request{}, ReadOptions{PageCredits: 1})
				if err != nil {
					t.Error(err)
					return
				}
				defer closeBody(v)

				w.Header().Set("Content-Length", strconv.FormatUint(size, 10))

				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}

				if n, err := v.WriteToHTTP(streamingShortDeadline{w}); n != int64(size) || err != nil {
					t.Errorf("stream: bytes=%d err=%v", n, err)
					panic(http.ErrAbortHandler)
				}
			}))
			server.EnableHTTP2 = true

			server.StartTLS()
			defer server.Close()

			server.Client().Timeout = 10 * time.Second

			resp, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(resp.Body)

			n, err := io.Copy(&offsetSink{}, resp.Body)
			if resp.ProtoMajor != 2 || n != int64(size) || err != nil {
				t.Fatalf("proto=%s bytes=%d err=%v", resp.Proto, n, err)
			}
		})
	}
}

type streamingDeadlineState struct {
	http.ResponseWriter
	mu       sync.Mutex
	deadline time.Time
}

func (w *streamingDeadlineState) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.deadline = d

	return nil
}

func TestStreamingOperationClearPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	dst := &streamingDeadlineState{ResponseWriter: httptest.NewRecorder()}
	h := &streamingHTTP{value: &Value{ctx: ctx}, controller: http.NewResponseController(dst)}
	interrupt := time.Now().Add(-time.Second)

	cancel()

	if err := dst.SetWriteDeadline(interrupt); err != nil {
		t.Fatal(err)
	}

	if err := h.clearWriteDeadline(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if !dst.deadline.Equal(interrupt) {
		t.Fatal("operation cleanup erased cancellation deadline")
	}
}

type streamingHTTP2BlockedWriter struct {
	http.ResponseWriter
	active  atomic.Bool
	written atomic.Int64
}

func (w *streamingHTTP2BlockedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *streamingHTTP2BlockedWriter) Write(p []byte) (int, error) {
	w.active.Store(true)
	defer w.active.Store(false)

	n, err := w.ResponseWriter.Write(p)
	w.written.Add(int64(n))

	return n, err
}

func TestStreamingHTTP2BlockedDestinationCancellation(t *testing.T) {
	path := clientPeer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamResponse(w, 0, int64(PageSize), int64(PageSize), `"v"`)
	}))
	c := testClient(t, path, 1)
	c.config.BodyReadTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	destination := make(chan *streamingHTTP2BlockedWriter, 1)
	done := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		v, err := c.GetStreaming(ctx, Request{})
		if err != nil {
			done <- err
			return
		}
		defer closeBody(v)

		w.Header().Set("Content-Length", strconv.FormatUint(uint64(PageSize), 10))

		if err := http.NewResponseController(w).Flush(); err != nil {
			done <- err
			return
		}

		dst := &streamingHTTP2BlockedWriter{ResponseWriter: w}
		destination <- dst

		_, err = v.WriteToHTTP(dst)
		done <- err

		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}))
	server.EnableHTTP2 = true

	server.StartTLS()
	defer server.Close()

	server.Client().Timeout = 5 * time.Second

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(resp.Body)

	if resp.ProtoMajor != 2 {
		t.Fatal("HTTP/2 not negotiated")
	}

	var dst *streamingHTTP2BlockedWriter
	select {
	case dst = <-destination:
	case <-time.After(3 * time.Second):
		t.Fatal("destination not started")
	}
	// Do not consume the body: exhaust the client's stream flow-control window.
	// Require an actual Write to stay active with no progress before canceling.
	deadline := time.Now().Add(3 * time.Second)
	blocked := false

	for time.Now().Before(deadline) {
		before := dst.written.Load()

		time.Sleep(100 * time.Millisecond)

		if before > 0 && dst.active.Load() && dst.written.Load() == before {
			blocked = true
			break
		}
	}

	if !blocked {
		t.Fatal("destination never blocked in HTTP/2 Write")
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked HTTP/2 destination did not cancel")
	}

	if c.Stats().ActiveBulk != 0 || len(c.copySlots) != 0 {
		t.Fatal("canceled HTTP/2 transfer retained admission")
	}
}
