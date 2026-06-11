// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestObjectURL(t *testing.T) {
	cases := []struct {
		endpoint string
		bucket   string
		key      string
		want     string
	}{
		{"http://h:9000", "", "soaks3/obj-0", "http://h:9000/soaks3/obj-0"},
		{"http://h:9000/", "", "soaks3/obj-0", "http://h:9000/soaks3/obj-0"},
		{"http://h:9000///", "b", "k", "http://h:9000/b/k"},
		{"http://h:9000", "bucket", "soaks3/obj-1", "http://h:9000/bucket/soaks3/obj-1"},
	}

	for _, tc := range cases {
		g := &loadGen{opts: &runOptions{endpoint: tc.endpoint, bucket: tc.bucket}}
		if got := g.objectURL(tc.key); got != tc.want {
			t.Errorf("objectURL(%q) endpoint=%q bucket=%q = %q, want %q",
				tc.key, tc.endpoint, tc.bucket, got, tc.want)
		}
	}
}

func TestResolveRunConfigFromManifest(t *testing.T) {
	dir := t.TempDir()
	mpath := filepath.Join(dir, manifestName)

	if err := writeManifest(mpath, manifest{Count: 50, ObjectSize: 2048, KeyPrefix: "p/", Seed: 9}); err != nil {
		t.Fatal(err)
	}

	opts := &runOptions{manifest: mpath}

	count, objectSize, prefix, err := resolveRunConfig(opts)
	if err != nil {
		t.Fatal(err)
	}

	if count != 50 || objectSize != 2048 || prefix != "p/" {
		t.Errorf("got count=%d size=%d prefix=%q", count, objectSize, prefix)
	}
}

func TestResolveRunConfigRequiresCountOrManifest(t *testing.T) {
	if _, _, _, err := resolveRunConfig(&runOptions{}); err == nil {
		t.Fatal("expected error when neither --count nor --manifest set")
	}
}

func TestResolveRunConfigFromFlags(t *testing.T) {
	opts := &runOptions{count: 100, objectSize: "4KiB", keyPrefix: "soaks3/"}

	count, objectSize, prefix, err := resolveRunConfig(opts)
	if err != nil {
		t.Fatal(err)
	}

	if count != 100 || objectSize != 4096 || prefix != "soaks3/" {
		t.Errorf("got count=%d size=%d prefix=%q", count, objectSize, prefix)
	}
}

func TestRunLoadDrivesRequests(t *testing.T) {
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if !strings.HasPrefix(r.URL.Path, "/soaks3/obj-") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Length", "16")
		_, _ = w.Write(make([]byte, 16))
	}))
	defer srv.Close()

	opts := &runOptions{
		endpoint:    srv.URL,
		keyPrefix:   "soaks3/",
		count:       100,
		objectSize:  "16B",
		concurrency: 4,
		duration:    150 * time.Millisecond,
		metricsAddr: "",
		reportEvery: 0,
		timeout:     5 * time.Second,
		zipf:        defaultZipfConfig(),
	}

	if err := runLoad(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	if hits.Load() == 0 {
		t.Fatal("expected the stub frontend to receive requests")
	}
}

func TestRunLoadRangeRead(t *testing.T) {
	var sawRange atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			sawRange.Store(true)

			w.Header().Set("Content-Range", "bytes 0-7/1024")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 8))

			return
		}

		_, _ = w.Write(make([]byte, 1024))
	}))
	defer srv.Close()

	opts := &runOptions{
		endpoint:    srv.URL,
		keyPrefix:   "soaks3/",
		count:       20,
		objectSize:  "1KiB",
		concurrency: 2,
		duration:    100 * time.Millisecond,
		rangeRead:   true,
		rangeSize:   "8B",
		metricsAddr: "",
		timeout:     5 * time.Second,
		zipf:        defaultZipfConfig(),
	}

	if err := runLoad(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	if !sawRange.Load() {
		t.Fatal("expected ranged requests")
	}
}

func TestDoRequestCountsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
	}))
	defer srv.Close()

	km, _ := newKeyModel("soaks3/", 10)
	m := newMetrics()
	g := &loadGen{
		opts:       &runOptions{endpoint: srv.URL},
		km:         km,
		objectSize: 16,
		client:     &http.Client{Timeout: 5 * time.Second},
		metrics:    m,
	}

	sel, err := defaultZipfConfig().newSelector(10, 0)
	if err != nil {
		t.Fatal(err)
	}

	g.doRequest(context.Background(), sel, 0)

	if got := m.errTotal.Load(); got != 1 {
		t.Fatalf("errTotal = %d, want 1 (404 must be an error)", got)
	}

	if got := m.reqTotal.Load(); got != 1 {
		t.Fatalf("reqTotal = %d, want 1", got)
	}
}

func TestDoRequestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 16))
	}))
	defer srv.Close()

	km, _ := newKeyModel("soaks3/", 10)
	m := newMetrics()
	g := &loadGen{
		opts:       &runOptions{endpoint: srv.URL},
		km:         km,
		objectSize: 16,
		client:     &http.Client{Timeout: 5 * time.Second},
		metrics:    m,
	}

	sel, _ := defaultZipfConfig().newSelector(10, 0)
	g.doRequest(context.Background(), sel, 0)

	if got := m.errTotal.Load(); got != 0 {
		t.Fatalf("errTotal = %d, want 0", got)
	}

	if got := m.byteTotal.Load(); got != 16 {
		t.Fatalf("byteTotal = %d, want 16", got)
	}
}

func TestRunLoadCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 16))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	opts := &runOptions{
		endpoint:    srv.URL,
		keyPrefix:   "soaks3/",
		count:       10,
		objectSize:  "16B",
		concurrency: 2,
		duration:    0, // until cancelled
		metricsAddr: "",
		timeout:     5 * time.Second,
		zipf:        defaultZipfConfig(),
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)

	go func() { done <- runLoad(ctx, opts) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLoad returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runLoad did not stop after cancellation")
	}
}
