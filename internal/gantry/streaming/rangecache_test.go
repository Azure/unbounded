// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
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
