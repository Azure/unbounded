// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// These timeouts only fail a stuck test; channel handshakes determine ordering.
const testWatchdog = 10 * time.Second

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case value := <-ch:
		return value
	case <-time.After(testWatchdog):
		t.Fatal("timed out waiting for test handshake")

		var zero T

		return zero
	}
}

func clientForTest(t *testing.T, endpoint string, concurrency int) *racersdk.Client {
	t.Helper()

	c, err := racersdk.NewClient(endpoint, racersdk.ClientOptions{Concurrency: concurrency})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(c.CloseIdleConnections)

	return c
}

func assertMetrics(t *testing.T, reg *prometheus.Registry, wantBytes float64, wantSuccess, wantError uint64) {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)

	for _, family := range families {
		for _, metric := range family.Metric {
			result := ""

			for _, label := range metric.Label {
				if label.GetName() == "result" {
					result = label.GetValue()
				}
			}

			key := family.GetName() + "/" + result
			seen[key] = true

			wantCount := wantSuccess
			if result == "error" {
				wantCount = wantError
			}

			switch family.GetName() {
			case "racer_loadgen_received_bytes_total":
				if got := metric.GetCounter().GetValue(); got != wantBytes {
					t.Errorf("received bytes = %g, want %g", got, wantBytes)
				}
			case "racer_loadgen_downloads_total":
				if got := metric.GetCounter().GetValue(); got != float64(wantCount) {
					t.Errorf("%s downloads = %g, want %d", result, got, wantCount)
				}
			case "racer_loadgen_download_duration_seconds":
				h := metric.GetHistogram()
				if h.GetSampleCount() != wantCount || h.GetSampleSum() < 0 || wantCount > 0 && h.GetSampleSum() <= 0 {
					t.Errorf("%s duration count/sum = %d/%g; want count %d and positive duration for attempts", result, h.GetSampleCount(), h.GetSampleSum(), wantCount)
				}
			}
		}
	}

	for _, key := range []string{
		"racer_loadgen_received_bytes_total/", "racer_loadgen_downloads_total/success", "racer_loadgen_downloads_total/error",
		"racer_loadgen_download_duration_seconds/success", "racer_loadgen_download_duration_seconds/error",
	} {
		if !seen[key] {
			t.Errorf("missing metric %s", key)
		}
	}
}

type memoryWriterAt struct {
	mu   sync.Mutex
	data []byte
}

func (w *memoryWriterAt) WriteAt(p []byte, off int64) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if off < 0 || off > int64(len(w.data)) || int64(len(p)) > int64(len(w.data))-off {
		return 0, fmt.Errorf("out-of-bounds write at %d, length %d", off, len(p))
	}

	return copy(w.data[off:], p), nil
}

func TestSDKOriginMultipagePayloadAndSuccessMetrics(t *testing.T) {
	d := datasetForTest(t, config{footprint: 3 * (2*racersdk.PageSize + 137), objectSize: 2*racersdk.PageSize + 137, ttl: 23 * time.Second})
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	assertMetrics(t, reg, 0, 0, 0)

	origin, _ := racersdk.NewOrigin(d)

	meta, err := d.Stat(context.Background(), d.target(1), nil)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex

	heads := 0

	var ranges []string

	server := unixTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.RequestURI != d.target(1) {
			t.Errorf("target = %q, want %q", r.RequestURI, d.target(1))
		}

		switch r.Method {
		case http.MethodHead:
			heads++

			if r.Header.Get("Range") != "" {
				t.Error("HEAD carried a payload Range")
			}
		case http.MethodGet:
			if heads == 0 || r.Header.Get("If-Match") != meta.ETag {
				t.Errorf("GET must follow HEAD and pin its ETag: heads=%d If-Match=%q", heads, r.Header.Get("If-Match"))
			}

			ranges = append(ranges, r.Header.Get("Range"))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		mu.Unlock()
		origin.ServeHTTP(w, r)
	}))
	defer server.Close()

	client := clientForTest(t, server.URL, 3)

	ctx, cancel := context.WithTimeout(context.Background(), testWatchdog)
	defer cancel()

	dst := &memoryWriterAt{data: make([]byte, d.size)}

	gotMeta, err := client.Download(ctx, d.target(1), dst)
	if err != nil {
		t.Fatal(err)
	}

	if gotMeta.Size != meta.Size || gotMeta.ETag != meta.ETag || gotMeta.TTL == nil || *gotMeta.TTL != *meta.TTL {
		t.Fatalf("SDK metadata = %+v, want %+v", gotMeta, meta)
	}

	expected := make([]byte, d.size)
	if n, err := sourceForTest(t, d, 1).ReadAt(expected, 0); n != len(expected) || err != nil {
		t.Fatalf("reference read = %d, %v", n, err)
	}

	if !bytes.Equal(dst.data, expected) {
		t.Fatal("SDK multipage payload differs from the source")
	}

	if err := download(ctx, client, d.target(1), testWatchdog, m); err != nil {
		t.Fatal(err)
	}

	assertMetrics(t, reg, float64(d.size), 1, 0)
	mu.Lock()
	defer mu.Unlock()

	wantRanges := []string{
		fmt.Sprintf("bytes=0-%d", racersdk.PageSize-1),
		fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, 2*racersdk.PageSize-1),
		fmt.Sprintf("bytes=%d-%d", 2*racersdk.PageSize, d.size-1),
	}
	wantRanges = append(wantRanges, wantRanges...)
	sort.Strings(wantRanges)
	sort.Strings(ranges)

	if heads != 2 || !reflect.DeepEqual(ranges, wantRanges) {
		t.Fatalf("wire requests: HEADs=%d ranges=%v; want 2 HEADs and %v", heads, ranges, wantRanges)
	}
}

func TestDownloadPartialFailureMetrics(t *testing.T) {
	d := datasetForTest(t, config{footprint: 2*racersdk.PageSize + 137, objectSize: 2*racersdk.PageSize + 137})
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	origin, _ := racersdk.NewOrigin(d)

	var heads, gets atomic.Int64

	const partialBytes = 12345

	server := unixTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
		} else {
			gets.Add(1)
		}

		if r.Method == http.MethodGet && r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, 2*racersdk.PageSize-1) {
			// Retain the real origin's valid framing/validator, but close the
			// response early so bytes already consumed must count as an error.
			recorded := httptest.NewRecorder()
			origin.ServeHTTP(recorded, r)

			for key, values := range recorded.Header() {
				w.Header()[key] = append([]string(nil), values...)
			}

			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes()[:partialBytes])

			return
		}

		origin.ServeHTTP(w, r)
	}))
	defer server.Close()
	// One page worker makes the complete first page and truncated second page
	// deterministic; no later page can race the failure.
	client := clientForTest(t, server.URL, 1)

	err := download(context.Background(), client, d.target(0), testWatchdog, m)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated download = %v, want unexpected EOF", err)
	}

	assertMetrics(t, reg, float64(racersdk.PageSize+partialBytes), 0, 1)

	if heads.Load() != 1 || gets.Load() != 2 {
		t.Errorf("HEADs/GETs = %d/%d, want 1/2", heads.Load(), gets.Load())
	}
}

func TestDownloadCanceledAfterCompletedPage(t *testing.T) {
	d := datasetForTest(t, config{footprint: racersdk.PageSize + 137, objectSize: racersdk.PageSize + 137})
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	origin, _ := racersdk.NewOrigin(d)
	blocked := make(chan struct{}, 1)
	requestCanceled := make(chan struct{}, 1)

	var heads, gets atomic.Int64

	server := unixTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
		} else {
			gets.Add(1)
		}

		if r.Method == http.MethodGet && r.Header.Get("Range") == fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, d.size-1) {
			blocked <- struct{}{}

			<-r.Context().Done()

			requestCanceled <- struct{}{}

			return
		}

		origin.ServeHTTP(w, r)
	}))
	defer server.Close()

	client := clientForTest(t, server.URL, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- download(ctx, client, d.target(0), testWatchdog, m) }()

	await(t, blocked)
	// Dispatch of page two proves all of page one was consumed and counted.
	assertMetrics(t, reg, float64(racersdk.PageSize), 0, 0)
	cancel()

	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled download = %v", err)
	}

	await(t, requestCanceled)
	assertMetrics(t, reg, float64(racersdk.PageSize), 0, 1)

	if heads.Load() != 1 || gets.Load() != 2 {
		t.Errorf("HEADs/GETs = %d/%d, want 1/2", heads.Load(), gets.Load())
	}
}

func TestRunLoadBoundsConcurrencyAndCancelsInflight(t *testing.T) {
	const workers, pages = 3, 2

	c := config{
		footprint: 8 * racersdk.PageSize, objectSize: 8 * racersdk.PageSize,
		concurrency: workers, pageConcurrency: pages, exponent: 1, seed: 42, timeout: time.Minute,
	}
	d := datasetForTest(t, c)
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	origin, _ := racersdk.NewOrigin(d)

	var heads, gets, active, peak atomic.Int64

	entered := make(chan struct{}, 64)
	exited := make(chan struct{}, 64)
	// All pages are held until cancellation. This saturates both worker bounds
	// without depending on server speed or sleeps to create overlap.
	server := unixTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			origin.ServeHTTP(w, r)

			return
		}

		gets.Add(1)

		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}

		entered <- struct{}{}

		<-r.Context().Done()
		active.Add(-1)

		exited <- struct{}{}
	}))
	defer server.Close()

	c.endpoint = server.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- runLoad(ctx, c, d, m) }()

	for i := 0; i < workers*pages; i++ {
		await(t, entered)
	}

	cancel()

	if err := await(t, done); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < workers*pages; i++ {
		await(t, exited)
	}

	server.Close()

	if heads.Load() != workers || gets.Load() != workers*pages || peak.Load() != workers*pages || active.Load() != 0 {
		t.Fatalf("HEADs=%d GETs=%d peak=%d active=%d; want %d/%d/%d/0",
			heads.Load(), gets.Load(), peak.Load(), active.Load(), workers, workers*pages, workers*pages)
	}

	assertMetrics(t, reg, 0, 0, workers)
}

func TestRunLoadAlreadyCanceledAndInvalidEndpoint(t *testing.T) {
	var requests atomic.Int64

	server := unixTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := config{endpoint: server.URL, footprint: 1, objectSize: 1, concurrency: 3, pageConcurrency: 2, timeout: time.Minute}
	d := datasetForTest(t, c)
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runLoad(ctx, c, d, m); err != nil {
		t.Fatal(err)
	}

	if requests.Load() != 0 {
		t.Errorf("already canceled load sent %d requests", requests.Load())
	}

	assertMetrics(t, reg, 0, 0, 0)

	c.endpoint = "http://example.com/invalid-prefix"
	if err := runLoad(context.Background(), c, d, m); err == nil {
		t.Error("invalid endpoint accepted")
	}

	assertMetrics(t, reg, 0, 0, 0)
}

func TestOriginRangeResponse(t *testing.T) {
	d := datasetForTest(t, config{footprint: 257, objectSize: 257})
	origin, _ := racersdk.NewOrigin(d)

	for _, tc := range []struct {
		rangeHeader  string
		status       int
		contentRange string
		off, size    int
	}{
		{"bytes=7-19", 206, "bytes 7-19/257", 7, 13},
		{"bytes=250-999", 206, "bytes 250-256/257", 250, 7},
		{"bytes=257-", 416, "bytes */257", 0, 0},
	} {
		t.Run(tc.rangeHeader, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, d.target(0), nil)
			r.Header.Set("Range", tc.rangeHeader)

			w := httptest.NewRecorder()
			origin.ServeHTTP(w, r)

			if w.Code != tc.status || w.Header().Get("Content-Range") != tc.contentRange || w.Header().Get("Content-Length") != strconv.Itoa(tc.size) {
				t.Fatalf("range response = %d, %v", w.Code, w.Header())
			}

			want := make([]byte, tc.size)
			if _, err := sourceForTest(t, d, 0).ReadAt(want, int64(tc.off)); err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(w.Body.Bytes(), want) {
				t.Error("range payload differs from source")
			}
		})
	}
}
