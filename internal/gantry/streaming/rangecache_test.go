// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
	"github.com/Azure/unbounded/internal/gantry/streaming"
)

// A cold image has no complete blob anywhere, so the cache is the only thing
// that stops every index read from reaching origin.
func TestCachedRangeIsReusedWithoutTouchingOrigin(t *testing.T) {
	t.Parallel()

	local := &rangeStoreStub{err: &ifaces.ErrNotFound{}}
	discovery := &discoveryStub{}
	peer := &peerStub{}
	origin := &originStub{body: []byte("INDX"), total: 4096}

	cache, err := rangecache.New(t.TempDir()+"/ranges", 1<<20)
	if err != nil {
		t.Fatalf("rangecache.New: %v", err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	var announced int

	server, err := streaming.NewServer(local, discovery, peer, origin, streaming.Options{
		URLPolicy:           streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}},
		Cache:               cache,
		MaxCachedRangeBytes: 1 << 20,
		OnCached:            func(digest.Digest) { announced++ },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rawOrigin := "https://westus.data.azurecr.io/?d=sha256:" + testDigest + "&sig=secret"

	get := func() *httptest.ResponseRecorder {
		t.Helper()

		req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+rawOrigin, nil)
		req.Header.Set("Range", "bytes=0-3")

		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)

		return rec
	}

	first := get()
	if first.Code != http.StatusPartialContent || first.Body.String() != "INDX" {
		t.Fatalf("first response = %d %q, want 206 INDX", first.Code, first.Body.String())
	}

	second := get()
	if second.Code != http.StatusPartialContent || second.Body.String() != "INDX" {
		t.Fatalf("second response = %d %q, want 206 INDX", second.Code, second.Body.String())
	}

	if origin.calls != 1 {
		t.Errorf("origin.calls = %d, want 1 (second read must come from cache)", origin.calls)
	}

	if announced == 0 {
		t.Error("OnCached never fired, so peers could not discover this range source")
	}
}

func TestRemoteRangeCacheAdmission(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"peer", "origin"} {
		for _, mode := range []string{"admitted", "full", "truncated", "empty", "store failure"} {
			t.Run(source+"/"+mode, func(t *testing.T) {
				t.Parallel()

				cacheDir := t.TempDir() + "/ranges"

				cache, err := rangecache.New(cacheDir, 4)
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = cache.Close() })

				d := digest.MustParse("sha256:" + testDigest)
				body := []byte("INDXextra")
				wantStatus := http.StatusPartialContent
				wantOutcome := "success"
				wantBytes := int64(4)

				switch mode {
				case "full":
					other := digest.MustParse("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
					if _, err := cache.Put(other, 4, 0, bytes.NewReader([]byte("full"))); err != nil {
						t.Fatal(err)
					}
				case "truncated":
					body = []byte("IN")
					wantStatus, wantOutcome, wantBytes = http.StatusBadGateway, "error", 2
				case "empty":
					body = nil
					wantStatus, wantOutcome, wantBytes = http.StatusBadGateway, "error", 0
				case "store failure":
					if err := os.Remove(cacheDir); err != nil {
						t.Fatal(err)
					}

					wantStatus, wantOutcome, wantBytes = http.StatusBadGateway, "error", 0
				}

				discovery := &discoveryStub{}
				if source == "peer" {
					discovery.set(ifaces.Provider{NodeID: "peer-a", Addr: "10.0.0.2:5001"})
				}

				peer := &peerStub{body: body, total: 4096}
				origin := &originStub{body: body, total: 4096}
				announced, observed := 0, 0

				server, err := streaming.NewServer(
					&rangeStoreStub{err: &ifaces.ErrNotFound{}},
					discovery, peer, origin, streaming.Options{
						URLPolicy:           streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}},
						Cache:               cache,
						MaxCachedRangeBytes: 4,
						OnCached:            func(digest.Digest) { announced++ },
						Metrics: streaming.MetricsHooks{
							OnRequest: func(gotSource, outcome string, _ time.Duration, n int64) {
								observed++

								if gotSource != source || outcome != wantOutcome || n != wantBytes {
									t.Errorf("observation = (%s, %s, %d), want (%s, %s, %d)", gotSource, outcome, n, source, wantOutcome, wantBytes)
								}
							},
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}

				req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+"https://westus.data.azurecr.io/?d="+d.String(), nil)
				req.Header.Set("Range", "bytes=2-5")

				rec := httptest.NewRecorder()
				server.ServeHTTP(rec, req)

				if rec.Code != wantStatus {
					t.Fatalf("response = %d %q, want %d", rec.Code, rec.Body.String(), wantStatus)
				}

				if wantStatus == http.StatusPartialContent {
					if rec.Body.String() != "INDX" || rec.Header().Get("Content-Range") != "bytes 2-5/4096" ||
						rec.Header().Get("Content-Length") != "4" || rec.Header().Get("Content-Type") != "application/octet-stream" {
						t.Errorf("response body = %q, headers = %v", rec.Body.String(), rec.Header())
					}
				}

				wantAnnounced := 0
				if mode == "admitted" {
					wantAnnounced = 1
				}

				if announced != wantAnnounced || observed != 1 {
					t.Errorf("announced = %d, observed = %d, want %d, 1", announced, observed, wantAnnounced)
				}

				if cache.Has(d, httprange.Range{Start: 2, End: 5}) != (mode == "admitted") {
					t.Error("cache coverage does not match admission")
				}

				if mode == "full" && (!cache.Full() || cache.HasAny(d)) {
					t.Error("full cache admitted the remote digest")
				}

				if (source == "peer" && (peer.calls != 1 || origin.calls != 0)) ||
					(source == "origin" && (peer.calls != 0 || origin.calls != 1)) {
					t.Errorf("peer calls = %d, origin calls = %d, want only %s", peer.calls, origin.calls, source)
				}
			})
		}
	}
}
