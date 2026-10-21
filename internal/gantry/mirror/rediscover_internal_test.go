// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

func TestClassifyPeerFetchError_Busy429(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		wantOutcome peerFetchOutcomeKind
		wantLabel   string
	}{
		{"too_many_requests", http.StatusTooManyRequests, peerFetchOutcomeBusy, "busy"},
		{"service_unavailable", http.StatusServiceUnavailable, peerFetchOutcomePeerServerError, "server_error"},
		{"forbidden", http.StatusForbidden, peerFetchOutcomeAuthOrConfigError, "auth_or_config"},
		{"teapot", http.StatusTeapot, peerFetchOutcomeProtocolError, "protocol_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, label := classifyPeerFetchError(&ifaces.ErrPeerHTTPStatus{PeerAddr: "peer:5001", StatusCode: tc.status})
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}

			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

func TestPeerAttemptSummary_BusyIsNotAllStale(t *testing.T) {
	// A round in which every attempt returned busy (429) must NOT be treated
	// as all-stale-or-filtered: the peers are alive and will serve once their
	// load drops, so we must not escalate as if they were dead.
	busy := updatePeerSummary(peerAttemptSummary{attempted: 1}, peerFetchOutcomeBusy)
	if busy.busy != 1 {
		t.Fatalf("busy count = %d, want 1", busy.busy)
	}

	if busy.allStaleOrFiltered() {
		t.Error("busy-only round must not be all-stale-or-filtered")
	}

	// A stale-only round, by contrast, IS all-stale-or-filtered.
	stale := updatePeerSummary(peerAttemptSummary{attempted: 1}, peerFetchOutcomeStaleProvider)
	if !stale.allStaleOrFiltered() {
		t.Error("stale-only round should be all-stale-or-filtered")
	}
}

func TestPeerAttemptSummary_ConfirmedBusySurvivesOtherFailures(t *testing.T) {
	for _, outcome := range []peerFetchOutcomeKind{
		peerFetchOutcomeUnavailable,
		peerFetchOutcomePeerServerError,
		peerFetchOutcomeAuthOrConfigError,
		peerFetchOutcomeProtocolError,
		peerFetchOutcomeStall,
	} {
		summary := updatePeerSummary(peerAttemptSummary{attempted: 2, busy: 1}, outcome)
		if !summary.capacityConstrained() {
			t.Errorf("confirmed busy peer was discarded after outcome %v", outcome)
		}
	}
}

type disappearingBusyPeer struct {
	onHead       func()
	fetches      int
	fetchOffset  int64
	metadataAuth string
	metadataRef  ifaces.OriginRef
	metadataErr  error
}

func (p *disappearingBusyPeer) FetchFromPeer(_ context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, string, error) {
	p.fetches++
	p.fetchOffset = ref.Offset

	return nil, 0, "", &ifaces.ErrPeerHTTPStatus{PeerAddr: addr, StatusCode: http.StatusTooManyRequests}
}

func (p *disappearingBusyPeer) HeadFromPeer(ctx context.Context, _ string, ref ifaces.OriginRef) (int64, string, error) {
	p.metadataAuth = registryauth.Authorization(ctx)
	p.metadataRef = ref
	p.onHead()

	return 2, "application/octet-stream", p.metadataErr
}

func TestBusyRetryStopsWhenProviderBecomesKnownStale(t *testing.T) {
	d := digest.MustParse("sha256:abababababababababababababababababababababababababababababababab")
	provider := ifaces.Provider{NodeID: "busy", Addr: "busy:5001"}
	dht := fakes.NewDHT()
	dht.Inject(d, provider)

	peer := &disappearingBusyPeer{}
	server := New(&config.Config{}, fakes.NewCache(), fakes.NewOriginPuller(),
		WithDiscovery(dht, peer), WithLiveStreamThrough(),
		WithPeerRediscover(time.Minute, time.Millisecond),
	)
	// Another request can observe the deletion after this metadata probe
	// succeeds but before the busy request's next lookup.
	peer.onHead = func() { server.markProviderStale(d, provider) }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+d.String(), nil)
	recorder := httptest.NewRecorder()

	result := server.tryPeerFallback(ctx, recorder, req, d, ifaces.KindBlob, "reg.example.com", "repo", server.logger)
	if ctx.Err() != nil {
		t.Fatal("busy request kept waiting until cancellation despite its provider becoming stale")
	}

	if result != peerFallbackPartial {
		t.Fatalf("result = %v, want the already-started response to terminate", result)
	}

	if peer.fetches != 1 {
		t.Fatalf("peer fetches = %d, want no retry of a known-stale provider", peer.fetches)
	}

	if recorder.Body.Len() != 0 {
		t.Fatalf("error text was appended to the content stream: %q", recorder.Body.String())
	}
}

func TestBusyMetadataDoesNotForwardRegistryAuthorization(t *testing.T) {
	d := digest.MustParse("sha256:bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc")
	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "busy", Addr: "busy:5001"})

	peer := &disappearingBusyPeer{onHead: func() {}}
	server := New(&config.Config{}, fakes.NewCache(), fakes.NewOriginPuller(), WithDiscovery(dht, peer))
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer test-registry-credential")

	_, _, usable := server.headFromPeers(ctx, ifaces.OriginRef{Digest: d, Kind: ifaces.KindBlob}, server.logger)
	if !usable {
		t.Fatal("busy metadata probe did not find the peer")
	}

	if peer.metadataAuth != "" {
		t.Fatal("busy metadata forwarded request-scoped registry credentials")
	}

	if registryauth.Authorization(ctx) == "" {
		t.Fatal("busy metadata changed the caller's authorization context")
	}
}

func TestBusyFetchConfirmsMetadataWithoutRegistryAuthorization(t *testing.T) {
	d := digest.MustParse("sha256:bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc")
	provider := ifaces.Provider{NodeID: "busy", Addr: "busy:5001"}
	heads := 0
	peer := &disappearingBusyPeer{onHead: func() { heads++ }}
	server := New(&config.Config{}, fakes.NewCache(), fakes.NewOriginPuller(), WithDiscovery(fakes.NewDHT(), peer))
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer test-only")
	req := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+d.String(), nil)

	result := server.fetchOneProvider(ctx, httptest.NewRecorder(), req, d, ifaces.KindBlob,
		"reg.example.com", "repo", provider, time.Second, nil, server.logger)
	if result.outcome != peerFetchOutcomeBusy || heads != 1 {
		t.Fatalf("outcome = %v, metadata probes = %d; want busy after one successful HEAD", result.outcome, heads)
	}

	if peer.metadataAuth != "" {
		t.Fatal("busy metadata forwarded request-scoped registry credentials")
	}

	if registryauth.Authorization(ctx) == "" {
		t.Fatal("busy metadata changed the caller's authorization context")
	}
}

func TestBusyFetchConfirmsWholeDigestWhenResuming(t *testing.T) {
	d := digest.MustParse("sha256:bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc")
	provider := ifaces.Provider{NodeID: "busy", Addr: "busy:5001"}

	cases := []struct {
		name    string
		err     error
		outcome peerFetchOutcomeKind
	}{
		{"found", nil, peerFetchOutcomeBusy},
		{"not_found", &ifaces.ErrNotFound{Digest: d}, peerFetchOutcomeStaleProvider},
		{"head_busy", &ifaces.ErrPeerHTTPStatus{StatusCode: http.StatusTooManyRequests}, peerFetchOutcomeProtocolError},
		{"head_server_error", &ifaces.ErrPeerHTTPStatus{StatusCode: http.StatusServiceUnavailable}, peerFetchOutcomePeerServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peer := &disappearingBusyPeer{onHead: func() {}, metadataErr: tc.err}
			server := New(&config.Config{}, fakes.NewCache(), fakes.NewOriginPuller(), WithDiscovery(fakes.NewDHT(), peer))
			w := httptest.NewRecorder()

			stream := &livePeerStream{}
			if err := stream.begin(w, d, 2, ifaces.KindBlob, "application/octet-stream"); err != nil {
				t.Fatal(err)
			}

			if _, err := stream.verifier.Write([]byte("a")); err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodGet, "/v2/repo/blobs/"+d.String(), nil)

			result := server.fetchOneProvider(context.Background(), w, req, d, ifaces.KindBlob,
				"reg.example.com", "repo", provider, time.Second, stream, server.logger)
			if result.outcome != tc.outcome {
				t.Fatalf("outcome = %v, want %v", result.outcome, tc.outcome)
			}

			if peer.fetchOffset != 1 || peer.metadataRef.Offset != 0 {
				t.Fatalf("GET/HEAD offsets = %d/%d, want resume at 1 and whole-digest metadata", peer.fetchOffset, peer.metadataRef.Offset)
			}
		})
	}
}

func TestBusyConfirmationRequiresPeerMetadata(t *testing.T) {
	peer := struct{ ifaces.PeerDialer }{&disappearingBusyPeer{}}
	server := New(&config.Config{}, fakes.NewCache(), fakes.NewOriginPuller(), WithDiscovery(fakes.NewDHT(), peer))

	outcome := server.confirmBusyProvider(context.Background(), httptest.NewRecorder(),
		ifaces.OriginRef{}, ifaces.Provider{Addr: "busy:5001"}, nil, server.logger)
	if outcome != peerFetchOutcomeProtocolError {
		t.Fatalf("outcome = %v, want unsupported metadata to prevent a busy wait", outcome)
	}
}

func TestJitteredBackoff_WithinBounds(t *testing.T) {
	base := time.Second
	lo := base - base/4
	hi := base + base/4

	for i := 0; i < 2000; i++ {
		got := jitteredBackoff(base)
		if got < lo || got > hi {
			t.Fatalf("jitteredBackoff(%v) = %v, want within [%v, %v]", base, got, lo, hi)
		}
	}

	if got := jitteredBackoff(0); got != 0 {
		t.Errorf("jitteredBackoff(0) = %v, want 0", got)
	}
}
