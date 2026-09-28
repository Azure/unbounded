// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestPullRangeMetrics(t *testing.T) {
	var (
		starts, failures, bytes, callbacks atomic.Int64
		mu                                 sync.Mutex
	)

	requests := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			return
		}

		switch r.URL.Path {
		case "/v2/fallback/blobs/":
			w.WriteHeader(404)
			return
		case "/v2/failed/blobs/":
			w.WriteHeader(503)
			return
		}

		w.Header().Set("Content-Length", "4")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "0123")
	}))
	defer srv.Close()

	c, err := New(&config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: srv.URL}}},
		WithMetrics(func(kind string) {
			if kind != "config" {
				t.Errorf("kind=%s", kind)
			}

			starts.Add(1)
		},
			func(kind, class string) {
				if kind != "config" || class != "transient" {
					t.Errorf("failure=%s/%s", kind, class)
				}

				failures.Add(1)
			}),
		WithByteMetrics(func(kind string, n int64) {
			if kind != "config" {
				t.Errorf("bytes kind=%s", kind)
			}

			bytes.Add(n)
			callbacks.Add(1)
		}),
		WithRequestMetrics(func(method string, status int) {
			mu.Lock()
			defer mu.Unlock()

			requests[method]++

			if status != 200 && status != 206 && status != 404 && status != 503 {
				t.Errorf("status=%d", status)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	ref := ifaces.OriginRef{Registry: "reg", Repository: "fallback", Kind: ifaces.KindConfig}
	for _, partial := range []bool{false, true} {
		body, _, _, err := c.PullRange(context.Background(), ref, 4)
		if err != nil {
			t.Fatal(err)
		}

		if partial {
			_, err = io.ReadFull(body, make([]byte, 2))
		} else {
			_, err = io.Copy(io.Discard, body)
		}

		if err != nil {
			t.Fatal(err)
		}

		_ = body.Close()
		_ = body.Close()
	}

	if _, _, err := c.Head(context.Background(), ref); err != nil {
		t.Fatal(err)
	}

	ref.Repository = "failed"
	if _, _, _, err := c.PullRange(context.Background(), ref, 4); err == nil {
		t.Fatal("expected upstream failure")
	}

	if _, _, _, err := c.PullRange(context.Background(), ref, 0); err == nil {
		t.Fatal("expected invalid range")
	}

	if starts.Load() != 4 || failures.Load() != 2 || bytes.Load() != 6 || callbacks.Load() != 2 {
		t.Fatalf("starts=%d failures=%d bytes=%d callbacks=%d", starts.Load(), failures.Load(), bytes.Load(), callbacks.Load())
	}

	mu.Lock()
	defer mu.Unlock()

	if requests["GET"] != 5 || requests["HEAD"] != 1 {
		t.Fatalf("requests=%v", requests)
	}
}

type closingMetricBody struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *closingMetricBody) Read(p []byte) (int, error) {
	close(b.entered)
	<-b.closed

	p[0] = 'x'

	return 1, io.ErrUnexpectedEOF
}

func (b *closingMetricBody) Close() error { b.once.Do(func() { close(b.closed) }); return nil }

func TestByteMetricsCloseDuringRead(t *testing.T) {
	upstream := &closingMetricBody{entered: make(chan struct{}), closed: make(chan struct{})}

	var bytes, calls atomic.Int64

	body := &countingReadCloser{ReadCloser: upstream, onFinish: func(n int64) { bytes.Add(n); calls.Add(1) }}

	done := make(chan struct{})
	go func() { _, _ = body.Read(make([]byte, 1)); close(done) }()

	<-upstream.entered

	_ = body.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked Read")
	}

	_ = body.Close()

	if bytes.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("bytes=%d calls=%d", bytes.Load(), calls.Load())
	}
}

func TestPullRangeByteMetricsTruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.Header().Set("Content-Range", "bytes 0-3/10")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "01")
	}))
	defer srv.Close()

	var bytes, calls atomic.Int64

	c, err := New(&config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg", Endpoint: srv.URL}}}, WithByteMetrics(func(_ string, n int64) { bytes.Add(n); calls.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}

	body, _, _, err := c.PullRange(context.Background(), ifaces.OriginRef{Registry: "reg", Repository: "image"}, 4)
	if err != nil {
		t.Fatal(err)
	}

	_, err = io.ReadAll(body)
	_ = body.Close()

	if err == nil || bytes.Load() != 2 || calls.Load() != 1 {
		t.Fatalf("err=%v bytes=%d calls=%d", err, bytes.Load(), calls.Load())
	}
}
