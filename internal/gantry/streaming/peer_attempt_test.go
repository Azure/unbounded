// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/httprange"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/streaming"
)

type peerRangeFunc func(context.Context, string, digest.Digest, httprange.Range) (io.ReadCloser, int64, string, error)

func (f peerRangeFunc) FetchRangeFromPeer(ctx context.Context, addr string, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, string, error) {
	return f(ctx, addr, d, requested)
}

func servePeerAttempt(t *testing.T, server *streaming.Server, ctx context.Context, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, streaming.HandlerPrefix+"https://westus.data.azurecr.io/?d=sha256:"+testDigest, nil).WithContext(ctx)
	req.Header.Set("Range", rangeHeader)

	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)

	return response
}

func TestCallerCancellationDoesNotPenalizePeer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	discovery := &discoveryStub{providers: []ifaces.Provider{{NodeID: "one", Addr: "one"}, {NodeID: "two", Addr: "two"}}}
	origin := &originStub{body: []byte("orig"), total: 4}
	calls := 0

	var attempted string

	peer := peerRangeFunc(func(_ context.Context, addr string, _ digest.Digest, _ httprange.Range) (io.ReadCloser, int64, string, error) {
		calls++
		attempted = addr

		if calls == 1 {
			cancel()
			return nil, 0, "", fmt.Errorf("caller disconnected: %w", context.Canceled)
		}

		return io.NopCloser(strings.NewReader("peer")), 4, "", nil
	})

	server, err := streaming.NewServer(&rangeStoreStub{err: &ifaces.ErrNotFound{}}, discovery, peer, origin,
		streaming.Options{URLPolicy: streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}}})
	if err != nil {
		t.Fatal(err)
	}

	servePeerAttempt(t, server, ctx, "bytes=0-3")

	if calls != 1 || origin.calls != 0 {
		t.Fatalf("canceled request attempted peers/origin = %d/%d, want 1/0", calls, origin.calls)
	}

	discovery.set(ifaces.Provider{NodeID: "retry", Addr: attempted})

	response := servePeerAttempt(t, server, t.Context(), "bytes=0-3")
	if calls != 2 || origin.calls != 0 || response.Body.String() != "peer" {
		t.Fatalf("healthy peer was penalized: calls=%d origin=%d response=%q", calls, origin.calls, response.Body.String())
	}

	canceled, stop := context.WithCancel(t.Context())
	stop()
	servePeerAttempt(t, server, canceled, "bytes=0-3")

	if calls != 2 || origin.calls != 0 {
		t.Fatal("already-canceled request attempted a remote source")
	}
}

func TestPeerTimeoutLeavesOriginFallbackUsable(t *testing.T) {
	origin := &originStub{body: []byte("orig"), total: 4}
	calls := 0
	peer := peerRangeFunc(func(ctx context.Context, _ string, _ digest.Digest, _ httprange.Range) (io.ReadCloser, int64, string, error) {
		calls++

		<-ctx.Done()

		return nil, 0, "", ctx.Err()
	})
	discovery := &discoveryStub{providers: []ifaces.Provider{{NodeID: "stalled", Addr: "stalled"}}}

	server, err := streaming.NewServer(&rangeStoreStub{err: &ifaces.ErrNotFound{}}, discovery, peer, origin,
		streaming.Options{URLPolicy: streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}}, PeerAttemptTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	response := servePeerAttempt(t, server, ctx, "bytes=0-3")
	if response.Code != http.StatusPartialContent || response.Body.String() != "orig" || ctx.Err() != nil {
		t.Fatalf("fallback = %d %q caller=%v", response.Code, response.Body.String(), ctx.Err())
	}

	response = servePeerAttempt(t, server, ctx, "bytes=0-3")
	if calls != 1 || origin.calls != 2 || response.Body.String() != "orig" {
		t.Fatalf("real timeout not suppressed: peers=%d origin=%d", calls, origin.calls)
	}
}

func TestPartialPeerMissLeavesOtherRangesUsable(t *testing.T) {
	calls := 0
	peer := peerRangeFunc(func(_ context.Context, _ string, d digest.Digest, requested httprange.Range) (io.ReadCloser, int64, string, error) {
		calls++

		if requested.Start == 4 {
			return nil, 0, "", &ifaces.ErrNotFound{Digest: d}
		}

		return io.NopCloser(strings.NewReader("peer")), 8, "", nil
	})
	origin := &originStub{body: []byte("orig"), total: 8}

	server, err := streaming.NewServer(&rangeStoreStub{err: &ifaces.ErrNotFound{}},
		&discoveryStub{providers: []ifaces.Provider{{NodeID: "partial", Addr: "partial"}}}, peer, origin,
		streaming.Options{URLPolicy: streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}}})
	if err != nil {
		t.Fatal(err)
	}

	servePeerAttempt(t, server, t.Context(), "bytes=4-7")
	servePeerAttempt(t, server, t.Context(), "bytes=4-7")

	response := servePeerAttempt(t, server, t.Context(), "bytes=0-3")
	if calls != 2 || origin.calls != 2 || response.Body.String() != "peer" {
		t.Fatalf("partial holder suppressed: peers=%d origin=%d response=%q", calls, origin.calls, response.Body.String())
	}
}
