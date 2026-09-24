// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/streaming"
)

type rangeStoreStub struct {
	body  []byte
	total int64
	err   error
}

func (s *rangeStoreStub) OpenRange(context.Context, digest.Digest, httprange.Range) (io.ReadCloser, int64, error) {
	if s.err != nil {
		return nil, s.total, s.err
	}

	return io.NopCloser(bytes.NewReader(s.body)), s.total, nil
}

type discoveryStub struct {
	mu        sync.Mutex
	providers []ifaces.Provider
}

func (s *discoveryStub) FindProviders(context.Context, digest.Digest) ([]ifaces.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]ifaces.Provider(nil), s.providers...), nil
}

func (s *discoveryStub) set(providers ...ifaces.Provider) {
	s.mu.Lock()

	s.providers = append([]ifaces.Provider(nil), providers...)
	s.mu.Unlock()
}

type peerStub struct {
	body  []byte
	total int64
	calls int
}

func (s *peerStub) FetchRangeFromPeer(context.Context, string, digest.Digest, httprange.Range) (io.ReadCloser, int64, string, error) {
	s.calls++

	return io.NopCloser(bytes.NewReader(s.body)), s.total, "application/octet-stream", nil
}

type originStub struct {
	body      []byte
	total     int64
	calls     int
	originRaw string
	requested httprange.Range
}

func (s *originStub) FetchRange(_ context.Context, origin streaming.OriginURL, requested httprange.Range) (io.ReadCloser, int64, string, error) {
	s.calls++
	s.originRaw = origin.Raw
	s.requested = requested

	return io.NopCloser(bytes.NewReader(s.body)), s.total, "application/octet-stream", nil
}

func TestServerTransitionsFromOriginToPeer(t *testing.T) {
	t.Parallel()

	local := &rangeStoreStub{err: &ifaces.ErrNotFound{}}
	discovery := &discoveryStub{}
	peer := &peerStub{body: []byte("peer"), total: 10}
	origin := &originStub{body: []byte("orig"), total: 10}

	server, err := streaming.NewServer(local, discovery, peer, origin, streaming.Options{
		URLPolicy: streaming.URLPolicy{
			AllowedHostSuffixes: []string{".data.azurecr.io"},
		},
		PeerLookupTimeout: time.Second,
		MaxPeerAttempts:   2,
	})
	if err != nil {
		t.Fatal(err)
	}

	rawOrigin := "https://westus.data.azurecr.io/?d=sha256:" + testDigest + "&sig=secret"
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+rawOrigin, nil)
		req.Header.Set("Range", "bytes=2-5")

		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)

		return response
	}

	first := request()
	if first.Code != http.StatusPartialContent || first.Body.String() != "orig" {
		t.Fatalf("first response = %d %q, want origin 206", first.Code, first.Body.String())
	}

	if origin.calls != 1 || peer.calls != 0 {
		t.Fatalf("first calls: origin=%d peer=%d", origin.calls, peer.calls)
	}

	discovery.set(ifaces.Provider{NodeID: "peer-a", Addr: "10.0.0.2:5001"})

	second := request()
	if second.Code != http.StatusPartialContent || second.Body.String() != "peer" {
		t.Fatalf("second response = %d %q, want peer 206", second.Code, second.Body.String())
	}

	if origin.calls != 1 || peer.calls != 1 {
		t.Fatalf("second calls: origin=%d peer=%d", origin.calls, peer.calls)
	}

	if origin.originRaw != rawOrigin || origin.requested != (httprange.Range{Start: 2, End: 5}) {
		t.Fatalf("origin request changed: raw=%q range=%+v", origin.originRaw, origin.requested)
	}
}

func TestServerPrefersLocalCompleteBlob(t *testing.T) {
	t.Parallel()

	local := &rangeStoreStub{body: []byte("locl"), total: 10}
	discovery := &discoveryStub{}
	peer := &peerStub{}
	origin := &originStub{}

	server, err := streaming.NewServer(local, discovery, peer, origin, streaming.Options{
		URLPolicy:         streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}},
		PeerLookupTimeout: time.Second,
		MaxPeerAttempts:   1,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+"https://westus.data.azurecr.io/?d=sha256:"+testDigest, nil)
	req.Header.Set("Range", "bytes=2-5")

	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)

	if response.Code != http.StatusPartialContent || response.Body.String() != "locl" {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}

	if peer.calls != 0 || origin.calls != 0 {
		t.Fatalf("peer=%d origin=%d, want zero", peer.calls, origin.calls)
	}
}

func TestServerRejectsInvalidOriginBeforeDependencies(t *testing.T) {
	t.Parallel()

	local := &rangeStoreStub{}
	discovery := &discoveryStub{}
	peer := &peerStub{}
	origin := &originStub{}

	server, err := streaming.NewServer(local, discovery, peer, origin, streaming.Options{
		URLPolicy:         streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}},
		PeerLookupTimeout: time.Second,
		MaxPeerAttempts:   1,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+"https://attacker.example/?d=sha256:"+testDigest, nil)
	req.Header.Set("Range", "bytes=0-0")

	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}

	if peer.calls != 0 || origin.calls != 0 {
		t.Fatalf("peer=%d origin=%d, want zero", peer.calls, origin.calls)
	}
}

func TestServerStartupGate(t *testing.T) {
	t.Parallel()

	local := &rangeStoreStub{body: []byte("x"), total: 1}

	server, err := streaming.NewServer(local, &discoveryStub{}, &peerStub{}, &originStub{}, streaming.Options{
		URLPolicy:         streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}},
		PeerLookupTimeout: time.Second,
		MaxPeerAttempts:   1,
		StartupGated:      true,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+"https://westus.data.azurecr.io/?d=sha256:"+testDigest, nil)
		req.Header.Set("Range", "bytes=0-0")

		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)

		return response
	}

	if got := request().Code; got != http.StatusServiceUnavailable {
		t.Fatalf("before MarkReady status = %d, want 503", got)
	}

	server.MarkReady()

	if got := request().Code; got != http.StatusPartialContent {
		t.Fatalf("after MarkReady status = %d, want 206", got)
	}

	readyRequest := func() int {
		request := httptest.NewRequest(http.MethodGet, streaming.ReadinessPath, nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)

		return response.Code
	}

	if got := readyRequest(); got != http.StatusOK {
		t.Fatalf("ready endpoint status = %d, want 200", got)
	}

	server.Drain()

	if got := readyRequest(); got != http.StatusServiceUnavailable {
		t.Fatalf("draining endpoint status = %d, want 503", got)
	}
}
