// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mirror_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/origin"
	"github.com/Azure/unbounded/internal/gantry/transfer"
)

// stubColdStart returns a canned provider list on any digest. Used to
// simulate the cold-start orchestrator's verdict at the mirror boundary
// without spinning up libp2p hosts.
type stubColdStart struct {
	providers []ifaces.Provider
	err       error
	calls     int32
}

type countingPeerDialer struct {
	mu     sync.Mutex
	bodies map[string]map[string][]byte
	counts map[string]int
}

func newCountingPeerDialer() *countingPeerDialer {
	return &countingPeerDialer{
		bodies: map[string]map[string][]byte{},
		counts: map[string]int{},
	}
}

func (d *countingPeerDialer) Put(addr string, dg digest.Digest, body []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.bodies[addr] == nil {
		d.bodies[addr] = map[string][]byte{}
	}

	d.bodies[addr][dg.String()] = append([]byte(nil), body...)
}

func (d *countingPeerDialer) FetchFromPeer(_ context.Context, addr string, ref ifaces.OriginRef) (io.ReadCloser, int64, error) {
	d.mu.Lock()
	d.counts[addr]++
	bodyByDigest := d.bodies[addr]
	d.mu.Unlock()

	if bodyByDigest == nil {
		return nil, 0, &ifaces.ErrNotFound{Digest: ref.Digest}
	}

	body, ok := bodyByDigest[ref.Digest.String()]
	if !ok {
		return nil, 0, &ifaces.ErrNotFound{Digest: ref.Digest}
	}

	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

func (d *countingPeerDialer) Calls(addr string) int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.counts[addr]
}

func (s *stubColdStart) Resolve(_ context.Context, _ digest.Digest, _ ifaces.OriginRefKind, _, _ string, _ int64) (*mirror.ColdStartResolution, error) {
	atomic.AddInt32(&s.calls, 1)

	if s.err != nil {
		return nil, s.err
	}

	return &mirror.ColdStartResolution{Providers: s.providers, Outcome: "stub"}, nil
}

// When DHT returns empty, the cold-start orchestrator is consulted, and
// its returned providers must be passed into the peer fetch loop - NOT
// falling through to origin.
func TestMirror_ColdStart_EmptyDHTRoutedThroughColdStartHit(t *testing.T) {
	body := []byte("served via cold-start hit")
	d := digestOf(body)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	cs := &stubColdStart{
		providers: []ifaces.Provider{{NodeID: "cs-peer", Addr: peerAddr}},
	}

	srv, originHits, peerFetches := newMirrorWithColdStart(t,
		map[digest.Digest][]byte{d: body},
		nil, // empty DHT
		cs,
	)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, _ := io.ReadAll(resp.Body)
	if string(got) != string(body) {
		t.Errorf("body mismatch: got %q want %q", got, body)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 1 {
		t.Errorf("peer fetches = %d, want 1", *peerFetches)
	}

	if atomic.LoadInt32(&cs.calls) != 1 {
		t.Errorf("cold-start invocations = %d, want 1", cs.calls)
	}
}

// When cold-start returns a sentinel error (rule 1 / rule 4 /
// exhaustion), the mirror must respond 5xx - NOT origin-pull.
func TestMirror_ColdStart_SentinelErrorReturns503(t *testing.T) {
	body := []byte("would-be served")
	d := digestOf(body)
	cs := &stubColdStart{err: errors.New("coldstart: failure short-circuit (auth)")}

	srv, originHits, _ := newMirrorWithColdStart(t,
		map[digest.Digest][]byte{d: body},
		nil, // empty DHT
		cs,
	)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (cold-start sentinel)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (cold-start sentinel must not origin-pull)", *originHits)
	}
}

// When the DHT already has providers, cold-start must NOT be invoked
// - the warm path runs directly per the design doc.
func TestMirror_ColdStart_WarmPathSkipsResolver(t *testing.T) {
	body := []byte("warm path bytes")
	d := digestOf(body)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	cs := &stubColdStart{}

	srv, originHits, _ := newMirrorWithColdStart(t,
		map[digest.Digest][]byte{d: body},
		map[digest.Digest][]ifaces.Provider{d: {{NodeID: "p", Addr: peerAddr}}},
		cs,
	)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0", *originHits)
	}

	if atomic.LoadInt32(&cs.calls) != 0 {
		t.Errorf("cold-start calls = %d, want 0 (warm path must skip resolver)", cs.calls)
	}
}

// newMirrorWithColdStart is a variant of newMirrorWithPeer that wires
// WithColdStart. The cold-start resolver may be nil, in which case
// fallthrough behaviour is preserved.
func newMirrorWithColdStart(t *testing.T, originBlobs map[digest.Digest][]byte, providers map[digest.Digest][]ifaces.Provider, cs mirror.ColdStartResolver) (*httptest.Server, *int32, *int32) {
	t.Helper()
	srv, _, _, originHits, peerFetches := newMirrorWithPeer(t, originBlobs, providers)
	// Discard the previous httptest.Server and rebuild with the same
	// upstream + cache but adding ColdStart. The simplest way is to
	// repeat the construction.
	t.Cleanup(srv.Close)

	// Re-derive the upstream URL and cache by walking back to the
	// helper's pieces. Simpler: just construct a fresh stack.
	srv2, hits, peerHits := buildColdStartMirror(t, originBlobs, providers, cs)
	_ = originHits  //nolint:errcheck // best-effort
	_ = peerFetches //nolint:errcheck // best-effort

	return srv2, hits, peerHits
}

// buildColdStartMirror mirrors newMirrorWithPeer but wires WithColdStart.
func buildColdStartMirror(t *testing.T, originBlobs map[digest.Digest][]byte, providers map[digest.Digest][]ifaces.Provider, cs mirror.ColdStartResolver) (*httptest.Server, *int32, *int32) {
	t.Helper()

	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)

		path := r.URL.Path
		ref := ""

		for _, sep := range []string{"/blobs/", "/manifests/"} {
			if idx := stringsLastIndex(path, sep); idx >= 0 {
				ref = path[idx+len(sep):]
				break
			}
		}

		d, err := digest.Parse(ref)
		if err != nil {
			w.WriteHeader(404)
			return
		}

		body, ok := originBlobs[d]
		if !ok {
			w.WriteHeader(404)
			return
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: up.URL},
		},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	for d, p := range providers {
		dht.Inject(d, p...)
	}

	var peerFetches int32

	client := transfer.NewClient()
	m := mirror.New(cfg, c, oc,
		mirror.WithDiscovery(dht, client),
		mirror.WithColdStart(cs),
		mirror.WithPeerMetrics(func(o string) {
			if o == "hit" {
				atomic.AddInt32(&peerFetches, 1)
			}
		}, nil),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	return srv, &originHits, &peerFetches
}

// TestMirror_PeerProvidersExhausted_ConsultsColdStart covers the stale-provider
// invariant: a non-empty DHT provider set where every candidate is stale must
// still flow into cold-start, independent of DHT health score.
func TestMirror_PeerProvidersExhausted_ConsultsColdStart(t *testing.T) {
	body := []byte("the canonical bytes for this digest")
	d := digestOf(body)

	// Peer that has the blob (cold-start surfaces it on retry).
	freshCache := fakes.NewCache()
	freshCache.Put(d, body)
	freshAddr := startPeerTransfer(t, freshCache)

	// Dead peer that the DHT thinks has it (stale provider record).
	deadCache := fakes.NewCache() // empty
	deadAddr := startPeerTransfer(t, deadCache)

	staleProviders := map[digest.Digest][]ifaces.Provider{
		d: {{NodeID: "peer-stale", Addr: deadAddr}},
	}
	cs := &stubColdStart{
		providers: []ifaces.Provider{{NodeID: "peer-fresh", Addr: freshAddr}},
	}

	srv, _, originHits, peerFetches := buildColdStartMirrorExposingDHT(t,
		map[digest.Digest][]byte{d: body}, staleProviders, cs)

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %q; want 200 (cold-start should have surfaced fresh peer)", resp.StatusCode, got)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (cold-start returned fresh peer)", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 1 {
		t.Errorf("peer hits = %d, want 1 (the fresh peer)", *peerFetches)
	}
}

// TestMirror_StaleOnlyFilteredProviders_ConsultsColdStartWithoutRefetch
// pins the narrower branch where DHT providers are filtered by the
// local stale-provider cache before any fetch attempt. The required control
// flow is: non-empty DHT -> all candidates filtered -> dhtStaleOnly metric ->
// cold-start resolver. The stale peer must NOT be re-fetched on the second
// request.
func TestMirror_StaleOnlyFilteredProviders_ConsultsColdStartWithoutRefetch(t *testing.T) {
	body := []byte("served-via-cold-start-after-prefilter")
	d := digestOf(body)

	const (
		staleAddr = "peer-stale:5001"
		freshAddr = "peer-fresh:5001"
	)

	peerDialer := newCountingPeerDialer()
	peerDialer.Put(freshAddr, d, body)

	dht := fakes.NewDHT()
	dht.Inject(d, ifaces.Provider{NodeID: "peer-stale", Addr: staleAddr})
	cs := &stubColdStart{providers: []ifaces.Provider{{NodeID: "peer-fresh", Addr: freshAddr}}}
	originPuller := fakes.NewOriginPuller()
	local := &writerSpyCache{}
	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{{Name: "reg.example.com", Endpoint: "http://unused"}},
	}

	var (
		staleOnlyCount int32
		filteredCount  int32
	)

	m := mirror.New(cfg, local, originPuller,
		mirror.WithLiveStreamThrough(),
		mirror.WithDiscovery(dht, peerDialer),
		mirror.WithColdStart(cs),
		mirror.WithDhtStaleOnlyMetric(func() { atomic.AddInt32(&staleOnlyCount, 1) }),
		mirror.WithStaleProviderFilteredMetric(func(n int) { atomic.AddInt32(&filteredCount, int32(n)) }),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	first, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	got, _ := io.ReadAll(first.Body)
	first.Body.Close()

	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, body = %q; want 200", first.StatusCode, got)
	}

	if string(got) != string(body) {
		t.Fatalf("first body = %q, want %q", got, body)
	}

	second, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	got, _ = io.ReadAll(second.Body) //nolint:errcheck // best-effort in test
	second.Body.Close()

	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, body = %q; want 200", second.StatusCode, got)
	}

	if string(got) != string(body) {
		t.Fatalf("second body = %q, want %q", got, body)
	}

	if calls := peerDialer.Calls(staleAddr); calls != 1 {
		t.Fatalf("stale peer fetch calls = %d, want 1 (second request must filter stale peer before any fetch)", calls)
	}

	if calls := peerDialer.Calls(freshAddr); calls != 2 {
		t.Fatalf("fresh peer fetch calls = %d, want 2 (cold-start provider should satisfy both requests)", calls)
	}

	if n := atomic.LoadInt32(&cs.calls); n != 2 {
		t.Fatalf("cold-start invocations = %d, want 2", n)
	}

	if n := atomic.LoadInt32(&staleOnlyCount); n != 1 {
		t.Fatalf("dht stale-only count = %d, want 1 (second request should hit the filtered-all-candidates branch)", n)
	}

	if n := atomic.LoadInt32(&filteredCount); n != 1 {
		t.Fatalf("stale provider filtered count = %d, want 1", n)
	}

	if n := originPuller.PullCount(d); n != 0 {
		t.Fatalf("origin pull count = %d, want 0", n)
	}

	if calls := local.Calls(); calls != 0 {
		t.Fatalf("local writer calls = %d, want 0 (live stream-through must not ingest during either request)", calls)
	}
}

// buildColdStartMirrorExposingDHT is buildColdStartMirror with the *fakes.DHT
// returned so the test can tune Health. Duplication is intentional -
// tests that need DHT control are a small minority.
func buildColdStartMirrorExposingDHT(t *testing.T, originBlobs map[digest.Digest][]byte, providers map[digest.Digest][]ifaces.Provider, cs mirror.ColdStartResolver) (*httptest.Server, *fakes.DHT, *int32, *int32) {
	t.Helper()

	var originHits int32

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&originHits, 1)

		path := r.URL.Path
		ref := ""

		for _, sep := range []string{"/blobs/", "/manifests/"} {
			if idx := stringsLastIndex(path, sep); idx >= 0 {
				ref = path[idx+len(sep):]
				break
			}
		}

		d, err := digest.Parse(ref)
		if err != nil {
			w.WriteHeader(404)
			return
		}

		body, ok := originBlobs[d]
		if !ok {
			w.WriteHeader(404)
			return
		}

		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body) //nolint:errcheck // best-effort write
	}))
	t.Cleanup(up.Close)

	cfg := &config.Config{
		UpstreamRegistries: []config.UpstreamRegistry{
			{Name: "reg.example.com", Endpoint: up.URL},
		},
	}
	c := fakes.NewCache()

	oc, err := origin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dht := fakes.NewDHT()
	for d, p := range providers {
		dht.Inject(d, p...)
	}

	var peerFetches int32

	client := transfer.NewClient()
	m := mirror.New(cfg, c, oc,
		mirror.WithDiscovery(dht, client),
		mirror.WithColdStart(cs),
		mirror.WithPeerMetrics(func(o string) {
			if o == "hit" {
				atomic.AddInt32(&peerFetches, 1)
			}
		}, nil),
	)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)

	return srv, dht, &originHits, &peerFetches
}

// TestMirror_DHTLookupError_ConsultsColdStart guards the residual half
// of finding 7 from the May-2026 review: when FindProviders returns an
// *error* (timeout / network glitch - distinct from "returned no
// providers"), the mirror must still consult cold-start instead of
// short-circuiting straight to a direct origin pull. Cold-start has
// independent provider sources (HRW membership + in-flight dedup +
// local cache) that don't depend on the DHT, so it can still produce a
// useful answer; falling straight to origin would bypass direct-origin-fallback
// rate-limiting and stampede the registry during a DHT outage.
func TestMirror_DHTLookupError_ConsultsColdStart(t *testing.T) {
	body := []byte("served via cold-start after DHT error")
	d := digestOf(body)

	peerCache := fakes.NewCache()
	peerCache.Put(d, body)
	peerAddr := startPeerTransfer(t, peerCache)

	cs := &stubColdStart{
		providers: []ifaces.Provider{{NodeID: "cs-peer", Addr: peerAddr}},
	}

	srv, dht, originHits, peerFetches := buildColdStartMirrorExposingDHT(t,
		map[digest.Digest][]byte{d: body}, nil, cs)

	// Program the DHT to error on FindProviders (simulates a timeout
	// or transport hiccup, not an empty result set).
	dht.SetFindProvidersError(errors.New("dht: bootstrap incomplete"))

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != 200 {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %q; want 200 (cold-start should have surfaced peer despite DHT error)", resp.StatusCode, got)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (cold-start returned a peer, no origin pull expected)", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 1 {
		t.Errorf("peer hits = %d, want 1", *peerFetches)
	}

	if atomic.LoadInt32(&cs.calls) != 1 {
		t.Errorf("cold-start invocations = %d, want 1 (DHT error must route through cold-start)", cs.calls)
	}
}

// TestMirror_DHTLookupError_ColdStartExhausted_ReturnsExhausted guards
// the direct-origin-fallback-eligibility half of the same fix: when cold-start returns
// ErrColdStartExhausted after a DHT error, the mirror must surface the
// ColdExhausted result so direct-origin-fallback can gate a direct-origin pull instead of
// short-circuiting to an ungated origin fetch.
func TestMirror_DHTLookupError_ColdStartExhausted_NF5Eligible(t *testing.T) {
	body := []byte("would be served via NF5")
	d := digestOf(body)

	// Cold-start returns the exhaustion sentinel so the path is
	// direct-origin-fallback-eligible; the mirror responds 503 here because no direct-origin-fallback is
	// wired in this fixture (matching how stubColdStart sentinel-err
	// tests already work above).
	cs := &stubColdStart{err: mirror.ErrColdStartExhausted}

	srv, dht, originHits, peerFetches := buildColdStartMirrorExposingDHT(t,
		map[digest.Digest][]byte{d: body}, nil, cs)
	dht.SetFindProvidersError(errors.New("dht: lookup timeout"))

	resp, err := http.Get(srv.URL + "/v2/r/blobs/" + d.String())
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (cold-start exhausted, NF5 unwired)", resp.StatusCode)
	}

	if atomic.LoadInt32(originHits) != 0 {
		t.Errorf("origin hits = %d, want 0 (NF5 unwired, must NOT bypass to origin)", *originHits)
	}

	if atomic.LoadInt32(peerFetches) != 0 {
		t.Errorf("peer hits = %d, want 0", *peerFetches)
	}

	if atomic.LoadInt32(&cs.calls) != 1 {
		t.Errorf("cold-start invocations = %d, want 1", cs.calls)
	}
}

// minimal lastIndex helper (avoids importing strings just for this).
func stringsLastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}

	return -1
}
