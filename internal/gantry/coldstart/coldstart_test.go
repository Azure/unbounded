// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/hrw"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/ifaces/fakes"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

// stubCoord returns canned PullIntent / PleasePullOutcome per node.
type stubCoord struct {
	mu              sync.Mutex
	intents         map[ifaces.NodeID]ifaces.PullIntent
	intentErrs      map[ifaces.NodeID]error
	intentCalls     int
	pleasePullCalls []ifaces.NodeID
	pleasePullRegs  []string
	pleasePullRepos []string
	pleasePullKinds []ifaces.OriginRefKind
	pleasePullDgs   [][]digest.Digest
	pleasePullAuth  []string
	pleasePullErrs  map[ifaces.NodeID]error
	// pleasePullOutcomes, when non-nil for a given (node, digest), is
	// returned verbatim from PleasePull. Used by rule-7 cooldown /
	// failure / unspecified tests to simulate the puller's state
	// changing between pull_intent_query and please_pull.
	pleasePullOutcomes map[ifaces.NodeID]map[digest.Digest]ifaces.PleasePullOutcome
}

func (s *stubCoord) PullIntentQuery(_ context.Context, id ifaces.NodeID, _ digest.Digest) (ifaces.PullIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.intentCalls++
	if err, ok := s.intentErrs[id]; ok {
		return ifaces.PullIntent{}, err
	}

	return s.intents[id], nil
}

func (s *stubCoord) PleasePull(ctx context.Context, id ifaces.NodeID, registry, repository string, kind ifaces.OriginRefKind, ds []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pleasePullCalls = append(s.pleasePullCalls, id)
	s.pleasePullRegs = append(s.pleasePullRegs, registry)
	s.pleasePullRepos = append(s.pleasePullRepos, repository)
	s.pleasePullKinds = append(s.pleasePullKinds, kind)
	s.pleasePullAuth = append(s.pleasePullAuth, registryauth.Authorization(ctx))
	// Copy ds so the caller can reuse the slice without aliasing.
	dsCopy := append([]digest.Digest(nil), ds...)

	s.pleasePullDgs = append(s.pleasePullDgs, dsCopy)
	if err, ok := s.pleasePullErrs[id]; ok {
		return nil, err
	}

	out := make([]ifaces.PleasePullOutcome, len(ds))
	for i, d := range ds {
		if perNode, ok := s.pleasePullOutcomes[id]; ok {
			if o, ok := perNode[d]; ok {
				out[i] = o
				continue
			}
		}

		out[i] = ifaces.PleasePullOutcome{Digest: d, Outcome: ifaces.PleasePullStarted}
	}

	return out, nil
}

// stubDisco implements coldstart.Discovery. providers is the canned
// FindProviders response; calls increments per call so tests can
// arrange "DHT empty then non-empty".
type stubDisco struct {
	mu        sync.Mutex
	providers [][]ifaces.Provider
	idx       int
	health    float64
}

func (s *stubDisco) FindProviders(_ context.Context, _ digest.Digest) ([]ifaces.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.idx >= len(s.providers) {
		if len(s.providers) == 0 {
			return nil, nil
		}

		return s.providers[len(s.providers)-1], nil
	}

	out := s.providers[s.idx]
	s.idx++

	return out, nil
}

func (s *stubDisco) Health() float64 {
	if s.health == 0 {
		return 1.0
	}

	return s.health
}

// helper: build a Resolver against the supplied test doubles plus a
// 4-node cluster (n0, n1, n2, n3) with self=n3 by default.
func buildResolver(t *testing.T, coord ifaces.Coordinator, disco coldstart.Discovery, self ifaces.NodeID, members []ifaces.Node, metrics coldstart.MetricsHooks, now func() time.Time) *coldstart.Resolver {
	t.Helper()

	mems := fakes.NewMembers(self, members...)
	infl := inflight.New(inflight.DefaultStalls(), now)

	return coldstart.New(coldstart.Options{
		Members:   mems,
		Discovery: disco,
		Coord:     coord,
		Inflight:  infl,
		Now:       now,
		HrwK:      3,
		HrwScope:  hrw.ScopeCluster,
		Metrics:   metrics,
		// Short timeouts so the cascade test suite runs in <1s total.
		QueryTimeout:         200 * time.Millisecond,
		PollManifest:         20 * time.Millisecond,
		PollLayer:            50 * time.Millisecond,
		TransientCooldownCap: 30 * time.Second,
	})
}

// buildResolverWithReplicas mirrors buildResolver but sets
// PrefetchPullerReplicas so prefetch fan-out can be exercised.
func buildResolverWithReplicas(t *testing.T, coord ifaces.Coordinator, disco coldstart.Discovery, self ifaces.NodeID, members []ifaces.Node, replicas int) *coldstart.Resolver {
	t.Helper()

	mems := fakes.NewMembers(self, members...)
	now := time.Now
	infl := inflight.New(inflight.DefaultStalls(), now)

	return coldstart.New(coldstart.Options{
		Members:                mems,
		Discovery:              disco,
		Coord:                  coord,
		Inflight:               infl,
		Now:                    now,
		HrwK:                   3,
		HrwScope:               hrw.ScopeCluster,
		PrefetchPullerReplicas: replicas,
		QueryTimeout:           200 * time.Millisecond,
		PollManifest:           20 * time.Millisecond,
		PollLayer:              50 * time.Millisecond,
		TransientCooldownCap:   30 * time.Second,
	})
}

func buildResolverWithFraction(t *testing.T, coord ifaces.Coordinator, disco coldstart.Discovery, self ifaces.NodeID, members []ifaces.Node, fraction float64) *coldstart.Resolver {
	t.Helper()

	mems := fakes.NewMembers(self, members...)
	now := time.Now
	infl := inflight.New(inflight.DefaultStalls(), now)

	return coldstart.New(coldstart.Options{
		Members:                mems,
		Discovery:              disco,
		Coord:                  coord,
		Inflight:               infl,
		Now:                    now,
		HrwK:                   3,
		HrwScope:               hrw.ScopeCluster,
		PrefetchPullerReplicas: 8,
		PrefetchPullerFraction: fraction,
		QueryTimeout:           200 * time.Millisecond,
		PollManifest:           20 * time.Millisecond,
		PollLayer:              50 * time.Millisecond,
		TransientCooldownCap:   30 * time.Second,
	})
}

// fixture: 4 named nodes; HRW top-K for the test digest needs to span
// all four so the test can pin who the responder is.
func clusterNodes() []ifaces.Node {
	return []ifaces.Node{
		{ID: "n0", Addr: "n0:5001"},
		{ID: "n1", Addr: "n1:5001"},
		{ID: "n2", Addr: "n2:5001"},
		{ID: "n3", Addr: "n3:5001"},
	}
}

func TestRule2_CacheHit(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('a', 64))
	nodes := clusterNodes()

	// Compute who actually lands in top-K for this digest so the test
	// can program the right node's intent.
	top := hrw.TopK(nodes, d, 3)
	if len(top) == 0 {
		t.Fatal("top-K empty")
	}

	cacheHolder := top[0].Node.ID

	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		cacheHolder: {HasCached: true},
	}}
	disco := &stubDisco{}
	hits := 0
	metrics := coldstart.MetricsHooks{
		OnTopKProbeHit:  func() { hits++ },
		OnDhtFalseEmpty: func() { /* counted via local int below */ },
	}

	var falseEmptyHits int

	metrics.OnDhtFalseEmpty = func() { falseEmptyHits++ }

	r := buildResolver(t, coord, disco, "self", nodes, metrics, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(res.Providers) == 0 {
		t.Fatal("Providers empty")
	}

	if res.Providers[0].NodeID != cacheHolder {
		t.Errorf("first provider = %s; want %s", res.Providers[0].NodeID, cacheHolder)
	}

	if hits != 1 {
		t.Errorf("OnTopKProbeHit calls = %d; want 1", hits)
	}

	if falseEmptyHits != 1 {
		t.Errorf("OnDhtFalseEmpty calls = %d; want 1", falseEmptyHits)
	}
}

func TestRule1_FailureShortCircuitBeatsCacheHit(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('b', 64))
	nodes := clusterNodes()

	top := hrw.TopK(nodes, d, 3)
	if len(top) < 2 {
		t.Fatal("need at least 2 nodes in top-K")
	}

	// One node has cache, another reports recently_failed (auth).
	// Rule 1 must beat rule 2.
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {HasCached: true},
		top[1].Node.ID: {RecentlyFailed: true, FailureClass: ifaces.FailureAuth},
	}}
	disco := &stubDisco{}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if !errors.Is(err, coldstart.ErrFailureShortCircuit) {
		t.Fatalf("err = %v; want ErrFailureShortCircuit", err)
	}
}

func TestRule1_DelegatedAuthorizationBypassesCredentialSpecificFailure(t *testing.T) {
	for _, class := range []ifaces.FailureClass{ifaces.FailureAuth, ifaces.FailureNotFound, ifaces.FailureRateLimited} {
		t.Run(string(class), func(t *testing.T) {
			d := digest.MustParse("sha256:" + rep('b', 64))
			nodes := clusterNodes()

			top := hrw.TopK(nodes, d, 3)
			if len(top) < 2 {
				t.Fatal("need at least 2 nodes in top-K")
			}

			cacheHolder := top[0].Node.ID
			coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
				cacheHolder:    {HasCached: true},
				top[1].Node.ID: {RecentlyFailed: true, FailureClass: class},
			}}
			r := buildResolver(t, coord, &stubDisco{}, "self", nodes, coldstart.MetricsHooks{}, time.Now)
			ctx := registryauth.WithAuthorization(context.Background(), "Bearer requester-token")

			res, err := r.Resolve(ctx, d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if len(res.Providers) == 0 || res.Providers[0].NodeID != cacheHolder {
				t.Fatalf("providers = %+v, want cache holder %s", res.Providers, cacheHolder)
			}
		})
	}
}

// TestRule1_ClusterWideTrustedClasses covers the design doc's requester rule:
// auth / not_found / rate_limited are trusted cluster-wide, so a
// single reachable node reporting any of them must short-circuit the
// cascade. Transient is handled separately by rule 4 (TestRule4_*).
func TestRule1_ClusterWideTrustedClasses(t *testing.T) {
	classes := []ifaces.FailureClass{
		ifaces.FailureAuth,
		ifaces.FailureNotFound,
		ifaces.FailureRateLimited,
	}
	for _, fc := range classes {
		fc := fc
		t.Run(string(fc), func(t *testing.T) {
			d := digest.MustParse("sha256:" + rep('b', 64))
			nodes := clusterNodes()
			top := hrw.TopK(nodes, d, 3)
			coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
				top[0].Node.ID: {RecentlyFailed: true, FailureClass: fc, CooldownUntil: time.Now().Add(time.Minute)},
				top[1].Node.ID: {},
				top[2].Node.ID: {},
			}}
			disco := &stubDisco{}
			r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

			_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
			if !errors.Is(err, coldstart.ErrFailureShortCircuit) {
				t.Fatalf("class=%s err = %v; want ErrFailureShortCircuit", fc, err)
			}
			// Crucially, no please_pull dialed: rule 1 forbids it
			// because rank-1 will get the same answer.
			if len(coord.pleasePullCalls) != 0 {
				t.Fatalf("class=%s: please_pull dialed %d times, want 0", fc, len(coord.pleasePullCalls))
			}
		})
	}
}

func TestRule3_InFlightThenDhtProvider(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('c', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	// One node reports in-flight with started_at = "1s ago".
	now := time.Now()
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {InFlight: true, StartedAt: now.Add(-1 * time.Second)},
	}}

	// First DHT poll empty, second returns a provider.
	disco := &stubDisco{providers: [][]ifaces.Provider{
		nil,
		{{NodeID: "pulled-by", Addr: "pulled-by:5001"}},
	}}

	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, func() time.Time { return now })

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(res.Providers) != 1 || res.Providers[0].NodeID != "pulled-by" {
		t.Errorf("Providers = %+v; want single 'pulled-by'", res.Providers)
	}
}

func TestRule3_InFlightStaleExcluded(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('d', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	// Manifest stall threshold = 5s. Report in-flight 10s ago - too
	// stale; should NOT trigger rule 3 (would fall through to rule 7).
	now := time.Now()
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {InFlight: true, StartedAt: now.Add(-10 * time.Second)},
		top[1].Node.ID: {},
		top[2].Node.ID: {},
	}}
	// With "neither cached nor in-flight" (after staleness filter), the
	// orchestrator will expand to top-2K under degraded DHT health, or
	// run cold-start on the first pass otherwise. Health=1.0 here; the
	// expansion is skipped, and rule 7 runs (please_pull + poll).
	disco := &stubDisco{
		health:    1.0,
		providers: [][]ifaces.Provider{nil, {{NodeID: "x", Addr: "x:5001"}}},
	}

	// the design doc: the stale puller exclusion must fire the takeover metric so
	// operators can observe rank-0 -> rank-1 routing.
	var takeoverKinds []string

	hooks := coldstart.MetricsHooks{
		OnDesignatedPullerTakeover: func(kindLabel string) {
			takeoverKinds = append(takeoverKinds, kindLabel)
		},
	}
	r := buildResolver(t, coord, disco, "self", nodes, hooks, func() time.Time { return now })

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	// Healthy DHT + all-neither must fire rule 7 cold-start at top-K
	// without an expansion pass (the design doc rule 7). The DHT poll returns
	// "x:5001" on the second FindProviders call.
	if err != nil {
		t.Fatalf("Resolve: %v (want success via rule 7 cold-start)", err)
	}

	if len(res.Providers) != 1 || res.Providers[0].Addr != "x:5001" {
		t.Fatalf("Providers = %+v; want single x:5001 from DHT poll", res.Providers)
	}

	if len(coord.pleasePullCalls) != 1 {
		t.Fatalf("please_pull dialed %d times; want 1", len(coord.pleasePullCalls))
	}

	if len(takeoverKinds) == 0 {
		t.Fatalf("OnDesignatedPullerTakeover never fired; want at least one (manifest)")
	}

	for _, k := range takeoverKinds {
		if k != "manifest" {
			t.Errorf("OnDesignatedPullerTakeover kind = %q; want \"manifest\"", k)
		}
	}
}

func TestRule4_TransientCooldown(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('e', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	now := time.Now()
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {RecentlyFailed: true, FailureClass: ifaces.FailureTransient, CooldownUntil: now.Add(20 * time.Second)},
	}}
	disco := &stubDisco{}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, func() time.Time { return now })

	_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("err = %v; want ErrCooldownActive", err)
	}
}

// TestRule4_HonorWindowSuppressesReprobe asserts the design doc: once the
// requester has observed a transient cooldown for a digest, the
// next Resolve within the honor window short-circuits without
// hitting any top-K node (i.e., no probe traffic).
func TestRule4_HonorWindowSuppressesReprobe(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('e', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	clock := time.Now()
	clockFn := func() time.Time { return clock }
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {RecentlyFailed: true, FailureClass: ifaces.FailureTransient, CooldownUntil: clock.Add(20 * time.Second)},
	}}
	disco := &stubDisco{}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, clockFn)

	// First call: observe transient, install honor window.
	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("first Resolve err = %v; want ErrCooldownActive", err)
	}

	firstCalls := coord.intentCalls
	if firstCalls == 0 {
		t.Fatalf("expected probe traffic on first Resolve, got 0 intent calls")
	}

	// Advance the clock a tick - still well inside the honor window.
	clock = clock.Add(1 * time.Second)

	// Second call: must short-circuit without re-probing.
	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("second Resolve err = %v; want ErrCooldownActive", err)
	}

	if coord.intentCalls != firstCalls {
		t.Fatalf("honor window did not suppress probe: intent calls went %d -> %d", firstCalls, coord.intentCalls)
	}
}

// TestRule4_HonorWindowExpires asserts that once the honor window
// has elapsed, the requester re-probes the top-K (the puller may
// have cleared its cooldown in the meantime).
func TestRule4_HonorWindowExpires(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('e', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	clock := time.Now()
	clockFn := func() time.Time { return clock }
	// Puller advertises a 20s cooldown; honor cap defaults to 30s so
	// the window is bounded by the puller's value (20s).
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {RecentlyFailed: true, FailureClass: ifaces.FailureTransient, CooldownUntil: clock.Add(20 * time.Second)},
	}}
	disco := &stubDisco{}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, clockFn)

	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("first Resolve err = %v; want ErrCooldownActive", err)
	}

	firstCalls := coord.intentCalls

	// Advance past the honor window (puller's 20s).
	clock = clock.Add(25 * time.Second)

	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("second Resolve err = %v; want ErrCooldownActive (puller still reports transient)", err)
	}

	if coord.intentCalls == firstCalls {
		t.Fatalf("expected re-probe after honor window expired; intent calls still %d", coord.intentCalls)
	}
}

// TestRule4_HonorWindowCapEnforced asserts that a puller advertising
// an unreasonably long cooldown (10 min) does not extend the
// requester's local honor window past TransientCooldownCap (30s
// default in buildResolver).
func TestRule4_HonorWindowCapEnforced(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('e', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	clock := time.Now()
	clockFn := func() time.Time { return clock }
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {RecentlyFailed: true, FailureClass: ifaces.FailureTransient, CooldownUntil: clock.Add(10 * time.Minute)},
	}}
	disco := &stubDisco{}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, clockFn)

	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("first Resolve err = %v; want ErrCooldownActive", err)
	}

	firstCalls := coord.intentCalls

	// Advance past the 30s cap but well inside the puller's 10min
	// cooldown - the cap should let the requester re-probe.
	clock = clock.Add(31 * time.Second)

	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("second Resolve err = %v; want ErrCooldownActive (puller still reports transient)", err)
	}

	if coord.intentCalls == firstCalls {
		t.Fatalf("cap not enforced: requester remained suppressed past TransientCooldownCap")
	}
}

func TestRule7_DegradedDhtExpansionThenColdStart(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('f', 64))
	nodes := []ifaces.Node{
		{ID: "n0", Addr: "n0:5001"},
		{ID: "n1", Addr: "n1:5001"},
		{ID: "n2", Addr: "n2:5001"},
		{ID: "n3", Addr: "n3:5001"},
		{ID: "n4", Addr: "n4:5001"},
		{ID: "n5", Addr: "n5:5001"},
		{ID: "n6", Addr: "n6:5001"},
		{ID: "n7", Addr: "n7:5001"},
	}
	// All nodes report "no cache, not in-flight" -> rule 7.
	intents := map[ifaces.NodeID]ifaces.PullIntent{}
	for _, n := range nodes {
		intents[n.ID] = ifaces.PullIntent{}
	}

	coord := &stubCoord{intents: intents}
	// DHT degraded so rule 6 expansion fires; then poll discovers the
	// provider after please_pull is sent.
	disco := &stubDisco{
		health:    0.4,
		providers: [][]ifaces.Provider{nil, {{NodeID: "the-puller", Addr: "the-puller:5001"}}},
	}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(res.Providers) != 1 || res.Providers[0].NodeID != "the-puller" {
		t.Errorf("Providers = %+v; want single 'the-puller'", res.Providers)
	}

	if len(coord.pleasePullCalls) != 1 {
		t.Errorf("pleasePullCalls = %d; want 1", len(coord.pleasePullCalls))
	}

	if len(coord.pleasePullRegs) != 1 || coord.pleasePullRegs[0] != "reg.example.com" {
		t.Errorf("pleasePullRegs = %v; want [reg.example.com]", coord.pleasePullRegs)
	}

	if len(coord.pleasePullRepos) != 1 || coord.pleasePullRepos[0] != "test/repo" {
		t.Errorf("pleasePullRepos = %v; want [test/repo]", coord.pleasePullRepos)
	}
}

// Rule 7 must refuse to send please_pull with empty registry/repository
// per the design doc (single-repo-per-batch). The orchestrator surfaces this as
// ErrExhausted so the mirror returns 5xx instead of silently never
// triggering the puller.
func TestRule7_EmptyRegistryRejected(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('e', 64))
	nodes := []ifaces.Node{
		{ID: "n0", Addr: "n0:5001"},
		{ID: "n1", Addr: "n1:5001"},
		{ID: "n2", Addr: "n2:5001"},
		{ID: "n3", Addr: "n3:5001"},
		{ID: "n4", Addr: "n4:5001"},
		{ID: "n5", Addr: "n5:5001"},
		{ID: "n6", Addr: "n6:5001"},
		{ID: "n7", Addr: "n7:5001"},
	}

	intents := map[ifaces.NodeID]ifaces.PullIntent{}
	for _, n := range nodes {
		intents[n.ID] = ifaces.PullIntent{}
	}

	coord := &stubCoord{intents: intents}
	// Degraded DHT so rule 6 expansion fires and the expanded pass
	// reaches rule 7. Without registry/repository the orchestrator
	// must refuse to send please_pull and surface ErrExhausted.
	disco := &stubDisco{
		health:    0.4,
		providers: [][]ifaces.Provider{nil, nil},
	}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "", "", 0)
	if !errors.Is(err, coldstart.ErrExhausted) {
		t.Fatalf("Resolve err = %v; want ErrExhausted (empty registry must short-circuit rule 7)", err)
	}

	if len(coord.pleasePullCalls) != 0 {
		t.Errorf("pleasePullCalls = %d; want 0 (must not dial puller with empty registry)", len(coord.pleasePullCalls))
	}
}

// Rule 7 cold-start MUST fire on the top-K pass when DHT is healthy
// and every reachable peer reports neither cached nor in-flight.
// Regression test for the earlier early-return that bypassed rule 7
// whenever expandLabel=="" - making the design's primary cold-start
// path unreachable on a healthy cluster.
func TestRule7_HealthyDhtFiresColdStartAtTopK(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('a', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)
	// All three top-K peers respond with empty intent (not cached,
	// not in-flight, not recently failed).
	coord := &stubCoord{intents: map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {},
		top[1].Node.ID: {},
		top[2].Node.ID: {},
	}}
	// Healthy DHT (1.0). Provider stack: first FindProviders returns
	// nil (which triggered the cold-start path); subsequent
	// pollDHT call returns "x:5001" once please_pull has completed.
	disco := &stubDisco{
		health:    1.0,
		providers: [][]ifaces.Provider{nil, {{NodeID: "x", Addr: "x:5001"}}},
	}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindBlob, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve err = %v; want success via rule 7 cold-start at top-K", err)
	}

	if len(res.Providers) != 1 || res.Providers[0].Addr != "x:5001" {
		t.Fatalf("Providers = %+v; want single x:5001 from DHT poll after please_pull", res.Providers)
	}

	if len(coord.pleasePullCalls) != 1 {
		t.Fatalf("please_pull dialed %d times; want exactly 1 (lowest-rank reachable)", len(coord.pleasePullCalls))
	}
}

// TestRule7_PleasePullAlreadyPulling asserts that an ALREADY_PULLING
// outcome is treated identically to STARTED - the cascade falls
// through to the DHT poll. This is the common race where the
// requester's rule-1 read saw the puller idle, but by the time
// please_pull lands the puller has already begun (e.g., a
// concurrent resolver beat us). Polling the DHT is correct: a
// provider record will land when the in-flight pull completes.
func TestRule7_PleasePullAlreadyPulling(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('a', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)
	coord := &stubCoord{
		intents: map[ifaces.NodeID]ifaces.PullIntent{
			top[0].Node.ID: {},
			top[1].Node.ID: {},
			top[2].Node.ID: {},
		},
		pleasePullOutcomes: map[ifaces.NodeID]map[digest.Digest]ifaces.PleasePullOutcome{
			top[0].Node.ID: {d: {Digest: d, Outcome: ifaces.PleasePullAlreadyPulling}},
		},
	}
	disco := &stubDisco{
		health:    1.0,
		providers: [][]ifaces.Provider{nil, {{NodeID: "x", Addr: "x:5001"}}},
	}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindBlob, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve err = %v; want success on ALREADY_PULLING (poll DHT)", err)
	}

	if len(res.Providers) != 1 || res.Providers[0].Addr != "x:5001" {
		t.Fatalf("Providers = %+v; want x:5001 from DHT poll", res.Providers)
	}

	if len(coord.pleasePullCalls) != 1 {
		t.Fatalf("please_pull dialed %d times; want 1", len(coord.pleasePullCalls))
	}
}

// TestRule7_PleasePullRecentlyFailedTrusted asserts that when the
// puller's please_pull response reports RECENTLY_FAILED with a
// trusted failure class (auth / not_found / rate_limited), the
// resolver short-circuits with ErrFailureShortCircuit *without
// polling the DHT*. This is the "state changed between rule-1 and
// rule-7" race the seventh review called out: the puller's local
// negative cache transitioned mid-cascade, and the requester must
// surface the same cluster-wide rule the rule-1 path would have.
func TestRule7_PleasePullRecentlyFailedTrusted(t *testing.T) {
	classes := []ifaces.FailureClass{
		ifaces.FailureAuth,
		ifaces.FailureNotFound,
		ifaces.FailureRateLimited,
	}
	for _, fc := range classes {
		fc := fc
		t.Run(string(fc), func(t *testing.T) {
			d := digest.MustParse("sha256:" + rep('a', 64))
			nodes := clusterNodes()
			top := hrw.TopK(nodes, d, 3)
			now := time.Now()
			coord := &stubCoord{
				intents: map[ifaces.NodeID]ifaces.PullIntent{
					top[0].Node.ID: {},
					top[1].Node.ID: {},
					top[2].Node.ID: {},
				},
				pleasePullOutcomes: map[ifaces.NodeID]map[digest.Digest]ifaces.PleasePullOutcome{
					top[0].Node.ID: {d: {
						Digest:        d,
						Outcome:       ifaces.PleasePullRecentlyFailed,
						FailureClass:  fc,
						CooldownUntil: now.Add(time.Minute),
					}},
				},
			}
			disco := &stubDisco{health: 1.0}
			r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, func() time.Time { return now })

			_, err := r.Resolve(context.Background(), d, ifaces.KindBlob, "reg.example.com", "test/repo", 0)
			if !errors.Is(err, coldstart.ErrFailureShortCircuit) {
				t.Fatalf("class=%s err = %v; want ErrFailureShortCircuit", fc, err)
			}

			if len(coord.pleasePullCalls) != 1 {
				t.Fatalf("class=%s please_pull dialed %d times; want 1", fc, len(coord.pleasePullCalls))
			}
		})
	}
}

// TestRule7_PleasePullRecentlyFailedTransient asserts that when the
// puller reports RECENTLY_FAILED with class=transient, the resolver
// installs the honor window (so subsequent Resolves short-circuit)
// and returns ErrCooldownActive *without polling the DHT*. The
// honor-window install must use outcome.CooldownUntil - bounded by
// TransientCooldownCap, identical to rule 4.
func TestRule7_PleasePullRecentlyFailedTransient(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('a', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)
	clock := time.Now()
	clockFn := func() time.Time { return clock }
	coord := &stubCoord{
		intents: map[ifaces.NodeID]ifaces.PullIntent{
			top[0].Node.ID: {},
			top[1].Node.ID: {},
			top[2].Node.ID: {},
		},
		pleasePullOutcomes: map[ifaces.NodeID]map[digest.Digest]ifaces.PleasePullOutcome{
			top[0].Node.ID: {d: {
				Digest:        d,
				Outcome:       ifaces.PleasePullRecentlyFailed,
				FailureClass:  ifaces.FailureTransient,
				CooldownUntil: clock.Add(20 * time.Second),
			}},
		},
	}
	disco := &stubDisco{health: 1.0}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, clockFn)

	// First Resolve observes the transient outcome and installs
	// the honor window.
	if _, err := r.Resolve(context.Background(), d, ifaces.KindBlob, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("first Resolve err = %v; want ErrCooldownActive", err)
	}

	if len(coord.pleasePullCalls) != 1 {
		t.Fatalf("please_pull dialed %d times; want 1 (first Resolve)", len(coord.pleasePullCalls))
	}

	firstIntentCalls := coord.intentCalls

	// Second Resolve within the honor window: must short-circuit
	// without any probe traffic or another please_pull.
	if _, err := r.Resolve(context.Background(), d, ifaces.KindBlob, "reg.example.com", "test/repo", 0); !errors.Is(err, coldstart.ErrCooldownActive) {
		t.Fatalf("second Resolve err = %v; want ErrCooldownActive (honor window)", err)
	}

	if coord.intentCalls != firstIntentCalls {
		t.Errorf("second Resolve issued %d new pull_intent_query calls; want 0 (honor window)",
			coord.intentCalls-firstIntentCalls)
	}

	if len(coord.pleasePullCalls) != 1 {
		t.Errorf("second Resolve issued additional please_pull (total %d); want still 1",
			len(coord.pleasePullCalls))
	}
}

// TestRule7_PleasePullUnspecifiedExhausts asserts that an empty or
// UNSPECIFIED outcome from the puller maps to ErrExhausted without
// polling the DHT. Polling would burn the per-digest stall budget
// waiting for a provider record that may never appear, because the
// puller never committed to starting a pull.
func TestRule7_PleasePullUnspecifiedExhausts(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('a', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)
	coord := &stubCoord{
		intents: map[ifaces.NodeID]ifaces.PullIntent{
			top[0].Node.ID: {},
			top[1].Node.ID: {},
			top[2].Node.ID: {},
		},
		pleasePullOutcomes: map[ifaces.NodeID]map[digest.Digest]ifaces.PleasePullOutcome{
			top[0].Node.ID: {d: {Digest: d, Outcome: ifaces.PleasePullUnspecified}},
		},
	}
	// If the resolver polled the DHT, this stack would eventually
	// return "x:5001"; the assertion that providers is empty proves
	// the poll never ran.
	disco := &stubDisco{
		health:    1.0,
		providers: [][]ifaces.Provider{{{NodeID: "x", Addr: "x:5001"}}},
	}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindBlob, "reg.example.com", "test/repo", 0)
	if !errors.Is(err, coldstart.ErrExhausted) {
		t.Fatalf("Resolve err = %v; want ErrExhausted on UNSPECIFIED", err)
	}

	if res != nil {
		t.Errorf("Resolution = %+v; want nil on UNSPECIFIED (no DHT poll)", res)
	}
}

func TestRule5_NoReachableExpands(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('1', 64))
	nodes := []ifaces.Node{
		{ID: "n0", Addr: ""},
		{ID: "n1", Addr: ""},
		{ID: "n2", Addr: ""},
		{ID: "n3", Addr: ""},
		{ID: "n4", Addr: ""},
		{ID: "n5", Addr: ""},
		{ID: "n6", Addr: ""},
		{ID: "n7", Addr: ""},
	}
	top3 := hrw.TopK(nodes, d, 3)
	top6 := hrw.TopK(nodes, d, 6)
	// Top-3 all error out (simulating unreachable); top-6 includes 3
	// extra nodes that succeed with empty intent (rule 7 path).
	intentErrs := map[ifaces.NodeID]error{}
	for _, s := range top3 {
		intentErrs[s.Node.ID] = errors.New("unreachable")
	}

	intents := map[ifaces.NodeID]ifaces.PullIntent{}

	for _, s := range top6 {
		if _, blocked := intentErrs[s.Node.ID]; !blocked {
			intents[s.Node.ID] = ifaces.PullIntent{}
		}
	}

	coord := &stubCoord{intents: intents, intentErrs: intentErrs}
	disco := &stubDisco{
		health:    1.0,
		providers: [][]ifaces.Provider{nil, {{NodeID: "p", Addr: "p:5001"}}},
	}
	r := buildResolver(t, coord, disco, "self", nodes, coldstart.MetricsHooks{}, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(res.Providers) == 0 {
		t.Fatal("Providers empty")
	}
}

func TestPollDHTTimeoutReturnsExhausted(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('2', 64))
	nodes := clusterNodes()
	// Force rule 7 (degraded health) but DHT never returns a provider
	// so the poll loop must time out at the per-the design doc threshold.
	intents := map[ifaces.NodeID]ifaces.PullIntent{}
	for _, n := range nodes {
		intents[n.ID] = ifaces.PullIntent{}
	}

	coord := &stubCoord{intents: intents}
	disco := &stubDisco{
		health:    0.3, // expand
		providers: [][]ifaces.Provider{nil, nil, nil, nil},
	}

	// Pin "now" to a clock we control; the inflight Stalls resolver
	// will give us a 5s manifest threshold. We shrink it by configuring
	// a tiny PollManifest so the ticker fires often but never finds a
	// provider; the deadline based on inflight.DefaultStalls is 5s
	// of wall-clock time, which is too long for a unit test. Use a
	// fake clock that advances quickly.
	now := time.Now()
	r := coldstart.New(coldstart.Options{
		Members:      fakes.NewMembers("self", nodes...),
		Discovery:    disco,
		Coord:        coord,
		Inflight:     inflight.New(inflight.Stalls{ManifestConfig: 100 * time.Millisecond, LayerFloor: time.Second, LayerBytesPerSec: 50 << 20, LayerMultiplier: 3}, func() time.Time { return now }),
		Now:          func() time.Time { return now },
		HrwK:         3,
		QueryTimeout: 50 * time.Millisecond,
		PollManifest: 20 * time.Millisecond,
		PollLayer:    100 * time.Millisecond,
	})

	_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if !errors.Is(err, coldstart.ErrExhausted) {
		t.Fatalf("err = %v; want ErrExhausted", err)
	}
}

func TestRankMismatchEmitsMetric(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('3', 64))
	nodes := clusterNodes()
	top := hrw.TopK(nodes, d, 3)

	// Responder at rank 0 reports rank 99 instead of its actual rank
	// - should trigger OnRankMismatch. The other top-K nodes report
	// their honest ranks so they don't generate false-positive
	// mismatches.
	intents := map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {HasCached: true, RecipientRank: 99},
		top[1].Node.ID: {RecipientRank: 1},
		top[2].Node.ID: {RecipientRank: 2},
	}
	coord := &stubCoord{intents: intents}
	disco := &stubDisco{}

	var mismatches []ifaces.NodeID

	metrics := coldstart.MetricsHooks{
		OnRankMismatch: func(_ string, id ifaces.NodeID) {
			mismatches = append(mismatches, id)
		},
	}
	r := buildResolver(t, coord, disco, "self", nodes, metrics, time.Now)
	_, _ = r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0) //nolint:errcheck // best-effort

	if len(mismatches) != 1 || mismatches[0] != top[0].Node.ID {
		t.Errorf("mismatches = %v; want [%s]", mismatches, top[0].Node.ID)
	}
}

func rep(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}

	return string(b)
}

// TestTopKExpansionFactor_AllUnreachable asserts that Options.TopKExpansionFactor
// is honored: with factor=3 and HrwK=3 the expanded pass probes 9
// candidates, not the default 2*K=6. Uses the rule-5 path (all
// unreachable) so the result is deterministic and never touches
// pollDHT.
func TestTopKExpansionFactor_AllUnreachable(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('4', 64))

	// 12-node cluster + self; all peer nodes "unreachable" so every
	// PullIntentQuery returns an error and the cascade emits rule 5
	// (errNoReachable) on every pass.
	nodes := make([]ifaces.Node, 12)
	for i := range nodes {
		nodes[i] = ifaces.Node{ID: ifaces.NodeID("n" + string(rune('0'+i))), Addr: "x:5001"}
	}

	intentErrs := map[ifaces.NodeID]error{}
	for _, n := range nodes {
		intentErrs[n.ID] = errors.New("unreachable")
	}

	tests := []struct {
		name            string
		factor          int
		wantIntentCalls int // 3 (first pass) + HrwK*factor (expanded pass)
	}{
		{"default_factor_2", 0, 3 + 6},
		{"explicit_factor_2", 2, 3 + 6},
		{"factor_3", 3, 3 + 9},
		{"factor_4_capped_by_cluster", 4, 3 + 12}, // 12 nodes, K*4=12 -> all
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			coord := &stubCoord{intentErrs: intentErrs}
			disco := &stubDisco{health: 1.0}

			var expandReasons []string

			metrics := coldstart.MetricsHooks{
				OnTopKExpansion: func(reason string) {
					expandReasons = append(expandReasons, reason)
				},
			}

			r := coldstart.New(coldstart.Options{
				Members:             fakes.NewMembers("self", nodes...),
				Discovery:           disco,
				Coord:               coord,
				Inflight:            inflight.New(inflight.DefaultStalls(), nil),
				HrwK:                3,
				HrwScope:            hrw.ScopeCluster,
				TopKExpansionFactor: tt.factor,
				Metrics:             metrics,
				QueryTimeout:        100 * time.Millisecond,
				PollManifest:        20 * time.Millisecond,
				PollLayer:           50 * time.Millisecond,
			})

			_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
			if !errors.Is(err, coldstart.ErrExhausted) {
				t.Fatalf("Resolve err = %v; want ErrExhausted", err)
			}

			if coord.intentCalls != tt.wantIntentCalls {
				t.Errorf("intentCalls = %d; want %d (factor=%d, HrwK=3)",
					coord.intentCalls, tt.wantIntentCalls, tt.factor)
			}

			if len(expandReasons) != 1 || expandReasons[0] != "all_unreachable" {
				t.Errorf("OnTopKExpansion reasons = %v; want [all_unreachable]", expandReasons)
			}
		})
	}
}

// TestTopKExpansion_DegradedReason asserts OnTopKExpansion fires with
// reason="degraded_health" on the rule-6 path (rule 7 + DHT health
// in the Degraded band [0.3, 0.7)).
func TestTopKExpansion_DegradedReason(t *testing.T) {
	d := digest.MustParse("sha256:" + rep('5', 64))

	nodes := make([]ifaces.Node, 8)
	for i := range nodes {
		nodes[i] = ifaces.Node{ID: ifaces.NodeID("n" + string(rune('0'+i))), Addr: "x:5001"}
	}

	intents := map[ifaces.NodeID]ifaces.PullIntent{}
	for _, n := range nodes {
		intents[n.ID] = ifaces.PullIntent{} // empty - rule 7
	}

	coord := &stubCoord{intents: intents}
	// Degraded DHT triggers rule 6 expansion; provider eventually
	// shows up so pollDHT terminates and the resolve succeeds.
	disco := &stubDisco{
		health:    0.4,
		providers: [][]ifaces.Provider{nil, {{NodeID: "puller", Addr: "puller:5001"}}},
	}

	var reasons []string

	metrics := coldstart.MetricsHooks{
		OnTopKExpansion: func(reason string) { reasons = append(reasons, reason) },
	}

	r := buildResolver(t, coord, disco, "self", nodes, metrics, time.Now)
	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(reasons) != 1 || reasons[0] != "degraded_health" {
		t.Errorf("OnTopKExpansion reasons = %v; want [degraded_health]", reasons)
	}
}

// ---------------------------------------------------------------------------
// Self-as-first-class-participant tests (sixth review, #1 priority).
//
// These cover the regression where the resolver excluded self from the
// HRW probe set and from the rule-7 reachable list, causing self-as-
// rank-0 cases to delegate please_pull to rank 1 and break the
// "one origin pull per digest" thundering-herd invariant.
// ---------------------------------------------------------------------------

// stubLocalIntent implements ifaces.LocalIntentProvider.
type stubLocalIntent struct {
	mu     sync.Mutex
	intent ifaces.PullIntent
	calls  int
}

func (s *stubLocalIntent) LocalPullIntent(_ context.Context, _ digest.Digest) ifaces.PullIntent {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++

	return s.intent
}

// stubLocalPull implements ifaces.LocalPullStarter and records every
// invocation so a test can prove rule 7 went local instead of
// dialing libp2p.
type stubLocalPull struct {
	mu       sync.Mutex
	registry []string
	repo     []string
	digests  [][]digest.Digest
	out      []ifaces.PleasePullOutcome
	err      error
}

func (s *stubLocalPull) StartLocalPull(_ context.Context, registry, repository string, _ ifaces.OriginRefKind, ds []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.registry = append(s.registry, registry)
	s.repo = append(s.repo, repository)
	dsCopy := make([]digest.Digest, len(ds))
	copy(dsCopy, ds)

	s.digests = append(s.digests, dsCopy)
	if s.err != nil {
		return nil, s.err
	}

	if s.out != nil {
		return s.out, nil
	}

	out := make([]ifaces.PleasePullOutcome, len(ds))
	for i, d := range ds {
		out[i] = ifaces.PleasePullOutcome{Digest: d, Outcome: ifaces.PleasePullStarted}
	}

	return out, nil
}

// findDigestWhereSelfIsRank0 generates digests until it finds one for
// which hrw.TopK(nodes, d, k) puts self at index 0. HRW is
// deterministic, so this terminates quickly for any non-degenerate
// cluster - the search just needs to find a (digest, nodeID) pair
// whose double-hash maximises among the cluster. We try byte values
// 'a'…'z' and '0'…'9' before giving up so the test never spins
// forever on a buggy hash.
func findDigestWhereSelfIsRank0(t *testing.T, nodes []ifaces.Node, self ifaces.NodeID, k int) digest.Digest {
	t.Helper()

	for _, c := range []byte("abcdef0123456789") {
		d := digest.MustParse("sha256:" + rep(c, 64))

		top := hrw.TopK(nodes, d, k)
		if len(top) > 0 && top[0].Node.ID == self {
			return d
		}
	}
	// Fall back: vary the first byte while keeping the rest stable
	// to widen the search space without leaving the hex alphabet.
	for i := 0; i < 256; i++ {
		hi := "0123456789abcdef"[i%16]
		lo := "0123456789abcdef"[(i/16)%16]
		body := rep('a', 62)
		d := digest.MustParse("sha256:" + string(hi) + string(lo) + body)

		top := hrw.TopK(nodes, d, k)
		if len(top) > 0 && top[0].Node.ID == self {
			return d
		}
	}

	t.Fatalf("no digest found where self=%s ranks 0 in top-K=%d", self, k)

	return digest.Digest{}
}

// programPeerIntentsByRank programs every node in top other than self
// with a PullIntent whose RecipientRank reflects its actual position
// in top. Tests use this so lowestRankReachable picks self (rank 0)
// unambiguously instead of tying with peers whose default rank=0
// from a zero-valued PullIntent.
func programPeerIntentsByRank(coord *stubCoord, top []hrw.Scored, self ifaces.NodeID) {
	if coord.intents == nil {
		coord.intents = map[ifaces.NodeID]ifaces.PullIntent{}
	}

	for i, s := range top {
		if s.Node.ID == self {
			continue
		}

		coord.intents[s.Node.ID] = ifaces.PullIntent{RecipientRank: int32(i)}
	}
}

// TestSelfIsRank0_UsesLocalPullNotRPC asserts that when self is the
// HRW-designated puller (rank 0 in top-K) and no peer reports cache /
// in-flight, rule 7 invokes the LocalPullStarter rather than dialing
// Coord.PleasePull(self, ...). This is the canonical regression
// case from sixth code review #1: pre-fix, the resolver excluded
// self and dispatched please_pull to rank 1.
func TestSelfIsRank0_UsesLocalPullNotRPC(t *testing.T) {
	nodes := clusterNodes()
	self := ifaces.NodeID("n2") // pick any, the helper finds a digest
	d := findDigestWhereSelfIsRank0(t, nodes, self, 3)
	top := hrw.TopK(nodes, d, 3)

	// Every other top-K member reports rule-7 (cold start) intent
	// with its true rank so lowestRankReachable picks self (rank 0)
	// unambiguously.
	coord := &stubCoord{}
	programPeerIntentsByRank(coord, top, self)
	// Discovery: DHT empty on first call (rule 2 false-empty cross
	// check), then non-empty after the local pull "lands" so pollDHT
	// terminates.
	disco := &stubDisco{
		providers: [][]ifaces.Provider{
			nil,
			{{NodeID: self, Addr: "local:5001"}},
		},
	}
	li := &stubLocalIntent{intent: ifaces.PullIntent{}} // empty - rule 7
	lp := &stubLocalPull{}

	mems := fakes.NewMembers(self, nodes...)
	infl := inflight.New(inflight.DefaultStalls(), time.Now)
	r := coldstart.New(coldstart.Options{
		Members:              mems,
		Discovery:            disco,
		Coord:                coord,
		Inflight:             infl,
		LocalIntent:          li,
		LocalPull:            lp,
		HrwK:                 3,
		HrwScope:             hrw.ScopeCluster,
		Now:                  time.Now,
		QueryTimeout:         200 * time.Millisecond,
		PollManifest:         20 * time.Millisecond,
		PollLayer:            50 * time.Millisecond,
		TransientCooldownCap: 30 * time.Second,
	})

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(res.Providers) == 0 {
		t.Fatal("Providers empty after rule 7 self-pull")
	}
	// Invariant A: no remote PleasePull dial - rule 7 routed locally.
	if len(coord.pleasePullCalls) != 0 {
		t.Errorf("Coord.PleasePull dialed %d times, want 0; ids=%v", len(coord.pleasePullCalls), coord.pleasePullCalls)
	}
	// Invariant B: local pump invoked exactly once with the right
	// digest and registry/repository.
	lp.mu.Lock()
	defer lp.mu.Unlock()

	if len(lp.digests) != 1 {
		t.Fatalf("StartLocalPull invocations = %d; want 1", len(lp.digests))
	}

	if len(lp.digests[0]) != 1 || lp.digests[0][0] != d {
		t.Errorf("StartLocalPull digests = %v; want [%s]", lp.digests[0], d)
	}

	if lp.registry[0] != "reg.example.com" || lp.repo[0] != "test/repo" {
		t.Errorf("StartLocalPull addr = %s/%s; want reg.example.com/test/repo", lp.registry[0], lp.repo[0])
	}
	// Invariant C: LocalPullIntent was consulted at least once - the
	// synthetic self-response is what made rule 7 pick self.
	li.mu.Lock()
	calls := li.calls
	li.mu.Unlock()

	if calls < 1 {
		t.Errorf("LocalPullIntent calls = %d; want >= 1", calls)
	}
}

// TestSelfIsRank0_NoLocalPull_FallsBackToRPC asserts back-compat:
// when LocalPull is nil but LocalIntent is set, rule 7 falls back to
// Coord.PleasePull(self) rather than crashing. New deployments wire
// both; existing tests that set neither still bypass self entirely.
func TestSelfIsRank0_NoLocalPull_FallsBackToRPC(t *testing.T) {
	nodes := clusterNodes()
	self := ifaces.NodeID("n0")
	d := findDigestWhereSelfIsRank0(t, nodes, self, 3)
	top := hrw.TopK(nodes, d, 3)

	coord := &stubCoord{}
	programPeerIntentsByRank(coord, top, self)
	disco := &stubDisco{
		providers: [][]ifaces.Provider{
			nil,
			{{NodeID: self, Addr: "local:5001"}},
		},
	}
	li := &stubLocalIntent{intent: ifaces.PullIntent{}}

	mems := fakes.NewMembers(self, nodes...)
	infl := inflight.New(inflight.DefaultStalls(), time.Now)
	r := coldstart.New(coldstart.Options{
		Members:     mems,
		Discovery:   disco,
		Coord:       coord,
		Inflight:    infl,
		LocalIntent: li,
		// LocalPull intentionally nil.
		HrwK:                 3,
		HrwScope:             hrw.ScopeCluster,
		Now:                  time.Now,
		QueryTimeout:         200 * time.Millisecond,
		PollManifest:         20 * time.Millisecond,
		PollLayer:            50 * time.Millisecond,
		TransientCooldownCap: 30 * time.Second,
	})

	if _, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Coord.PleasePull(self, ...) is the fallback - exactly one call,
	// to self.
	if len(coord.pleasePullCalls) != 1 || coord.pleasePullCalls[0] != self {
		t.Errorf("Coord.PleasePull calls = %v; want [%s]", coord.pleasePullCalls, self)
	}
}

// TestConcurrentResolvers_OnlyDesignatedPullerInvoked is the cache-hit
// invariant test in cold-start unit form. Two parallel calls to
// Resolve on the SAME resolver with the SAME digest must converge on
// exactly one StartLocalPull invocation - the second caller's
// LocalPullIntent must see the in-flight entry from the first call
// and piggyback (rule 3) instead of triggering a second origin pull.
//
// The single-resolver framing is a unit-test stand-in for "two nodes
// each running their own resolver" - both nodes share the inflight
// view via the libp2p coord stream in production, and our
// LocalIntent (backed by the same inflight map across goroutines)
// faithfully reproduces that hand-off here.
func TestConcurrentResolvers_OnlyDesignatedPullerInvoked(t *testing.T) {
	nodes := clusterNodes()
	self := ifaces.NodeID("n1")
	d := findDigestWhereSelfIsRank0(t, nodes, self, 3)
	top := hrw.TopK(nodes, d, 3)

	coord := &stubCoord{}
	programPeerIntentsByRank(coord, top, self)
	disco := &stubDisco{
		providers: [][]ifaces.Provider{
			{{NodeID: self, Addr: "local:5001"}},
		},
	}

	mems := fakes.NewMembers(self, nodes...)
	infl := inflight.New(inflight.DefaultStalls(), time.Now)

	// LocalIntent reports in-flight if the inflight map has an entry,
	// otherwise reports empty. This is what coord.Server's
	// computeLocalIntent does in production (modulo cache.Has /
	// secondary.Has, irrelevant here).
	li := &liFromInflight{infl: infl}
	// LocalPull stub blocks on a gate AFTER inserting the inflight
	// entry; the test uses a "started" signal to wait until the
	// first resolver is actually parked in the pump before launching
	// the second, eliminating timing flakes under -race.
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	lp := &gatedLocalPull{infl: infl, gate: gate, started: started}

	r := coldstart.New(coldstart.Options{
		Members:              mems,
		Discovery:            disco,
		Coord:                coord,
		Inflight:             infl,
		LocalIntent:          li,
		LocalPull:            lp,
		HrwK:                 3,
		HrwScope:             hrw.ScopeCluster,
		Now:                  time.Now,
		QueryTimeout:         200 * time.Millisecond,
		PollManifest:         20 * time.Millisecond,
		PollLayer:            50 * time.Millisecond,
		TransientCooldownCap: 30 * time.Second,
	})

	var wg sync.WaitGroup

	errs := make([]error, 2)
	// Launch G1 first, wait until it reaches the pump (so the
	// inflight entry is live and observable), then launch G2.
	wg.Add(1)

	go func() {
		defer wg.Done()

		_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
		errs[0] = err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first resolver never reached StartLocalPull")
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		_, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
		errs[1] = err
	}()
	// Give G2 enough time to reach rule 3 + pollDHT, then unblock
	// G1's pump.
	time.Sleep(40 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("Resolve[%d]: %v", i, e)
		}
	}

	lp.mu.Lock()
	defer lp.mu.Unlock()
	// Exactly one local pull: the second resolver must have hit rule
	// 3 (in-flight) once it saw self's intent reflect the in-flight
	// entry inserted by the first.
	if lp.starts != 1 {
		t.Errorf("StartLocalPull invocations = %d; want 1 (F1 invariant: one origin pull per digest)", lp.starts)
	}

	if len(coord.pleasePullCalls) != 0 {
		t.Errorf("Coord.PleasePull dialed %d times, want 0", len(coord.pleasePullCalls))
	}
}

// liFromInflight is a LocalIntentProvider that reports in_flight
// whenever the shared inflight map has an entry - mirroring the
// production coord.Server.computeLocalIntent semantics.
type liFromInflight struct {
	infl *inflight.Map
}

func (l *liFromInflight) LocalPullIntent(_ context.Context, d digest.Digest) ifaces.PullIntent {
	if e, ok := l.infl.LookupForIntent(d); ok {
		return ifaces.PullIntent{InFlight: true, StartedAt: e.StartedAt}
	}

	return ifaces.PullIntent{}
}

// gatedLocalPull inserts an inflight entry, signals on started, then
// blocks on gate before returning Started. started lets the test
// know the resolver is parked in the pump so the second resolver
// can be launched deterministically.
type gatedLocalPull struct {
	infl    *inflight.Map
	gate    <-chan struct{}
	started chan<- struct{}
	mu      sync.Mutex
	starts  int
}

func (g *gatedLocalPull) StartLocalPull(ctx context.Context, _, _ string, _ ifaces.OriginRefKind, ds []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	g.mu.Lock()
	g.starts++
	g.mu.Unlock()

	out := make([]ifaces.PleasePullOutcome, len(ds))
	for i, d := range ds {
		// Insert the in-flight entry BEFORE returning so any
		// concurrent LocalPullIntent observes us. Map.Start returns
		// alreadyInFlight=true on the second hit; we don't release
		// the handle because production wouldn't either until the
		// pull actually completes.
		_, e, _ := g.infl.Start(d, ifaces.KindManifest, 0)
		out[i] = ifaces.PleasePullOutcome{Digest: d, Outcome: ifaces.PleasePullStarted, StartedAt: e.StartedAt}
	}

	if g.started != nil {
		// Non-blocking notify; only the first call matters.
		select {
		case g.started <- struct{}{}:
		default:
		}
	}

	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return out, nil
}

// TestLocalPullIntentObeysProbeDeadline asserts that the context
// handed to LocalIntentProvider.LocalPullIntent carries the same
// per-query deadline as the peer fan-out (QueryTimeout), not the
// outer Resolve context. Seventh code review #4: previously the
// resolver passed the outer ctx, which may have no deadline at all
// (mirror request contexts usually inherit from the agent's
// long-lived service context). LocalPullIntent may consult the
// secondary blob source (containerd content store) on cache miss;
// without the timeout a stall in that lookup would block the whole
// probe past the per-digest budget while peer responses have
// already arrived.
func TestLocalPullIntentObeysProbeDeadline(t *testing.T) {
	nodes := clusterNodes()
	self := ifaces.NodeID("n2")
	d := findDigestWhereSelfIsRank0(t, nodes, self, 3)
	top := hrw.TopK(nodes, d, 3)

	coord := &stubCoord{}
	programPeerIntentsByRank(coord, top, self)
	// rule 7 success path so Resolve doesn't fail before recording
	// the local-intent deadline.
	disco := &stubDisco{
		providers: [][]ifaces.Provider{
			nil,
			{{NodeID: self, Addr: "local:5001"}},
		},
	}

	li := &deadlineCapturingLocalIntent{}
	lp := &stubLocalPull{}

	mems := fakes.NewMembers(self, nodes...)
	infl := inflight.New(inflight.DefaultStalls(), time.Now)

	const queryTimeout = 200 * time.Millisecond

	r := coldstart.New(coldstart.Options{
		Members:      mems,
		Discovery:    disco,
		Coord:        coord,
		Inflight:     infl,
		HrwK:         3,
		HrwScope:     hrw.ScopeCluster,
		LocalIntent:  li,
		LocalPull:    lp,
		QueryTimeout: queryTimeout,
		PollManifest: 10 * time.Millisecond,
		PollLayer:    10 * time.Millisecond,
	})

	// Outer ctx has NO deadline - this is the production case for
	// most Resolve calls (the mirror handler's request ctx is
	// derived from the long-lived service ctx). The probeCtx wrap
	// is the only thing that can bound LocalPullIntent's view of
	// the budget.
	start := time.Now()
	_, _ = r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0) //nolint:errcheck // best-effort

	deadline, hasDeadline := li.lastDeadline()
	if !hasDeadline {
		t.Fatalf("LocalPullIntent ctx had no deadline; expected one derived from QueryTimeout (%v)", queryTimeout)
	}
	// The deadline must be within [QueryTimeout - small slack,
	// QueryTimeout + small slack] of the call start. We allow 250ms
	// of slack to absorb scheduler jitter; the assertion that
	// matters is "deadline is bounded by QueryTimeout, NOT by the
	// outer ctx (which has none)".
	got := deadline.Sub(start)
	if got < queryTimeout-50*time.Millisecond || got > queryTimeout+250*time.Millisecond {
		t.Errorf("LocalPullIntent ctx deadline = +%v from call start; want close to QueryTimeout=%v", got, queryTimeout)
	}
}

// deadlineCapturingLocalIntent records the deadline of every
// context passed to LocalPullIntent so tests can assert the
// resolver applied the probe-level timeout.
type deadlineCapturingLocalIntent struct {
	mu       sync.Mutex
	deadline time.Time
	hasDdl   bool
}

func (l *deadlineCapturingLocalIntent) LocalPullIntent(ctx context.Context, _ digest.Digest) ifaces.PullIntent {
	l.mu.Lock()
	defer l.mu.Unlock()

	if d, ok := ctx.Deadline(); ok {
		l.deadline = d
		l.hasDdl = true
	}

	return ifaces.PullIntent{}
}

func (l *deadlineCapturingLocalIntent) lastDeadline() (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.deadline, l.hasDdl
}

// TestPrefetchLayers_RoutesSelfToLocalPull asserts that when self is
// the HRW rank-0 designated puller for one or more digests in a
// prefetch batch AND the Resolver is configured with a
// LocalPullStarter, those digests are dispatched as a single
// StartLocalPull call (one batch, all self-digests in it) rather
// than silently skipped. Seventh code review #5: previously the
// self-bucket was always dropped; that was safe because the
// per-digest Resolve cascade still recovered via rule 7 when
// containerd actually asked, but it lost the latency benefit of
// speculative prefetch for layers assigned to self.
func TestPrefetchLayers_RoutesSelfToLocalPull(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n0")
	// 3 digests land on self, 2 on n1, 1 on n2 -> expect:
	// - 1 StartLocalPull(self) carrying 3 digests
	// - 2 PleasePull calls (one to n1, one to n2)
	targets := map[ifaces.NodeID]int{"n0": 3, "n1": 2, "n2": 1}
	digests := findManyDigestsForPullers(t, cluster, targets)

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0}
	lp := &stubLocalPull{}

	mems := fakes.NewMembers(self, cluster...)
	infl := inflight.New(inflight.DefaultStalls(), time.Now)

	var (
		pullerCount, digestCount int
		hookMu                   sync.Mutex
	)

	r := coldstart.New(coldstart.Options{
		Members:      mems,
		Discovery:    disco,
		Coord:        coord,
		Inflight:     infl,
		HrwK:         3,
		HrwScope:     hrw.ScopeCluster,
		LocalPull:    lp,
		QueryTimeout: 200 * time.Millisecond,
		PollManifest: 20 * time.Millisecond,
		PollLayer:    50 * time.Millisecond,
		Metrics: coldstart.MetricsHooks{
			OnPrefetchBatch: func(p, ds int) {
				hookMu.Lock()
				defer hookMu.Unlock()

				pullerCount = p
				digestCount = ds
			},
		},
	})

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	// Remote pullers: exactly 2 PleasePulls (n1, n2).
	coord.mu.Lock()
	gotRemote := len(coord.pleasePullCalls)
	gotPullerSet := append([]ifaces.NodeID{}, coord.pleasePullCalls...)
	coord.mu.Unlock()

	if gotRemote != 2 {
		t.Fatalf("PleasePull remote calls: got %d (%v), want 2 (only n1+n2)", gotRemote, gotPullerSet)
	}

	for _, id := range gotPullerSet {
		if id == self {
			t.Fatalf("PleasePull was dispatched to self=%s; should have gone through StartLocalPull", self)
		}
	}

	// Self-puller: exactly one StartLocalPull call carrying all 3
	// self-digests, and registry/repo plumbed through correctly.
	lp.mu.Lock()
	defer lp.mu.Unlock()

	if got := len(lp.registry); got != 1 {
		t.Fatalf("StartLocalPull calls: got %d, want 1 (single self-batch)", got)
	}

	if lp.registry[0] != "docker.io" {
		t.Errorf("StartLocalPull registry: got %q, want docker.io", lp.registry[0])
	}

	if lp.repo[0] != "library/nginx" {
		t.Errorf("StartLocalPull repository: got %q, want library/nginx", lp.repo[0])
	}

	if len(lp.digests) != 1 || len(lp.digests[0]) != 3 {
		t.Fatalf("StartLocalPull batch shape: got %v batches with sizes %v, want 1 batch of 3", len(lp.digests), digestBatchSizes(lp.digests))
	}

	// Metric: total pullers = remote (2) + self (1) = 3; total
	// digests = 6.
	hookMu.Lock()
	defer hookMu.Unlock()

	if pullerCount != 3 {
		t.Errorf("OnPrefetchBatch pullers: got %d, want 3 (n1+n2+self)", pullerCount)
	}

	if digestCount != 6 {
		t.Errorf("OnPrefetchBatch digests: got %d, want 6 (all 6 digests routed)", digestCount)
	}
}

func digestBatchSizes(batches [][]digest.Digest) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}

	return out
}

// TestPrefetchLayers_NilLocalPullStillSkipsSelf is a guardrail: when
// LocalPull is NOT configured (e.g. unit tests, or a deployment that
// hasn't wired the embedded coordinator yet), self-digests must
// continue to be silently skipped. This is the legacy behaviour
// preserved by the seventh-review #5 fix so that
// LocalPull-less callers keep working unchanged.
func TestPrefetchLayers_NilLocalPullStillSkipsSelf(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n0")
	targets := map[ifaces.NodeID]int{"n0": 4}
	digests := findManyDigestsForPullers(t, cluster, targets)

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0}

	mems := fakes.NewMembers(self, cluster...)
	infl := inflight.New(inflight.DefaultStalls(), time.Now)
	r := coldstart.New(coldstart.Options{
		Members:   mems,
		Discovery: disco,
		Coord:     coord,
		Inflight:  infl,
		HrwK:      3,
		HrwScope:  hrw.ScopeCluster,
		// LocalPull omitted on purpose.
		QueryTimeout: 200 * time.Millisecond,
		PollManifest: 20 * time.Millisecond,
		PollLayer:    50 * time.Millisecond,
	})

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.pleasePullCalls); got != 0 {
		t.Fatalf("PleasePull calls: got %d, want 0 (self-bucket skipped without LocalPull)", got)
	}
}

// ---------------------------------------------------------------------------
// Two-node cold-start concurrency test (seventh code review #6).
//
// Spec: Node A and Node B both Resolve the same digest concurrently.
// HRW rank 0 = A. Required invariants:
//
// (cache-hit) Only A originates the origin pull. B's Resolve must either
// observe A's in-flight entry (rule 3 piggyback) or route its
// rule-7 please_pull through Coord.PleasePull(A, ...), which
// in turn must dedupe through A's inflight map and NOT
// originate a second pull.
// (peer-fetch) Neither resolver originates a pull on B's local side.
//
// The doubles below build a "cluster of two" where Coord routes
// per-target: PullIntentQuery reads the target's inflight map;
// PleasePull invokes the target's LocalPullStarter (which atomically
// claims its own inflight). This is the minimum fidelity needed to
// exercise the inter-node ordering that the cache-hit invariant rests on.
// ---------------------------------------------------------------------------

// countingLocalPull implements LocalPullStarter. On each call it
// claims every requested digest in `infl` atomically, increments
// `starts` for fresh claims and `dupes` for already-claimed digests,
// and returns matching PleasePullOutcomes. This mirrors the
// production puller side: inflight.Start is the single source of
// truth for "did this call kick off a new origin pull?".
type countingLocalPull struct {
	mu     sync.Mutex
	infl   *inflight.Map
	starts int
	dupes  int
}

func (c *countingLocalPull) StartLocalPull(_ context.Context, _, _ string, kind ifaces.OriginRefKind, ds []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	out := make([]ifaces.PleasePullOutcome, len(ds))
	for i, d := range ds {
		_, e, already := c.infl.Start(d, kind, 0)
		c.mu.Lock()
		if already {
			c.dupes++
			out[i] = ifaces.PleasePullOutcome{
				Digest:    d,
				Outcome:   ifaces.PleasePullAlreadyPulling,
				StartedAt: e.StartedAt,
			}
		} else {
			c.starts++
			out[i] = ifaces.PleasePullOutcome{
				Digest:    d,
				Outcome:   ifaces.PleasePullStarted,
				StartedAt: e.StartedAt,
			}
		}
		c.mu.Unlock()
	}

	return out, nil
}

// inflightLocalIntent implements LocalIntentProvider against a
// per-node inflight map. Mirrors the production computeLocalIntent
// for the in-flight bit (HasCached / RecentlyFailed are zero - those
// branches aren't exercised by the cache-hit test).
type inflightLocalIntent struct {
	infl *inflight.Map
}

func (l *inflightLocalIntent) LocalPullIntent(_ context.Context, d digest.Digest) ifaces.PullIntent {
	if e, ok := l.infl.LookupForIntent(d); ok {
		return ifaces.PullIntent{InFlight: true, StartedAt: e.StartedAt}
	}

	return ifaces.PullIntent{}
}

// twoNodeCoord routes Coord RPCs between two resolvers. RPCs to a
// target node consult / mutate that target's stub state, simulating
// the inter-node coord plane without the libp2p hop. Concurrent-safe
// because both inflight.Map and countingLocalPull are themselves
// locked.
type twoNodeCoord struct {
	mu            sync.Mutex
	infl          map[ifaces.NodeID]*inflight.Map
	rank          map[ifaces.NodeID]int32
	lp            map[ifaces.NodeID]*countingLocalPull
	pleasePullsTo map[ifaces.NodeID]int
	intentQueries map[ifaces.NodeID]int
}

func (c *twoNodeCoord) PullIntentQuery(_ context.Context, target ifaces.NodeID, d digest.Digest) (ifaces.PullIntent, error) {
	c.mu.Lock()
	c.intentQueries[target]++
	infl := c.infl[target]
	rank := c.rank[target]
	c.mu.Unlock()

	intent := ifaces.PullIntent{RecipientRank: rank}
	if e, ok := infl.LookupForIntent(d); ok {
		intent.InFlight = true
		intent.StartedAt = e.StartedAt
	}

	return intent, nil
}

func (c *twoNodeCoord) PleasePull(ctx context.Context, target ifaces.NodeID, registry, repository string, kind ifaces.OriginRefKind, ds []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	c.mu.Lock()
	c.pleasePullsTo[target]++
	lp := c.lp[target]
	c.mu.Unlock()

	return lp.StartLocalPull(ctx, registry, repository, kind, ds)
}

// TestColdStart_TwoNode_OnlyHRW0OriginatesPull is the cache-hit invariant
// test from seventh code review #6.
//
// Setup: two-node cluster (A=n0, B=n1). Pick a digest whose HRW rank
// 0 is A. Spin up two real Resolvers, one per node, sharing a Coord
// router and a Discovery (which canned-publishes A as a provider so
// pollDHT terminates quickly). Both Resolve concurrently.
//
// Assertions:
//
// - aLP.starts == 1: A originates exactly one origin pull.
//
// - bLP.starts == 0: B's local pull starter is never touched -
// because every rule-7 self-puller decision routes its
// PleasePull through Coord.PleasePull(target_id_of_A), which in
// this test's wiring lands on aLP, not on bLP.
//
// - twoNodeCoord.pleasePullsTo[A] >= 0: B's resolve may issue
// please_pull to A (rule 7 with A as rank-0 reachable puller)
// OR observe rule 3 via PullIntentQuery (A already in-flight),
// depending on race. Both outcomes satisfy cache-hit because aLP
// dedupes via inflight.Start.
//
// - twoNodeCoord.pleasePullsTo[B] == 0: nothing ever asks B to
// pull from origin - A is rank 0 and reachable.
func TestColdStart_TwoNode_OnlyHRW0OriginatesPull(t *testing.T) {
	cluster := []ifaces.Node{
		{ID: "n0", Addr: "n0:5001"},
		{ID: "n1", Addr: "n1:5001"},
	}

	// Find a digest where HRW rank 0 = n0 ("A").
	var d digest.Digest

	for i := 0; i < 4096; i++ {
		cand := digest.MustParse("sha256:" + digestHex(i))
		if pickHRW0(cluster, cand) == "n0" {
			d = cand
			break
		}
	}

	if d.String() == "" {
		t.Fatalf("could not find a digest with HRW rank 0 = n0 in 4096 tries")
	}
	// Sanity: ranks must be {n0: 0, n1: 1}.
	top := hrw.TopK(cluster, d, 2)
	if top[0].Node.ID != "n0" || top[1].Node.ID != "n1" {
		t.Fatalf("unexpected HRW ranking: %v", top)
	}

	aInfl := inflight.New(inflight.DefaultStalls(), time.Now)
	bInfl := inflight.New(inflight.DefaultStalls(), time.Now)
	aLP := &countingLocalPull{infl: aInfl}
	bLP := &countingLocalPull{infl: bInfl}
	aLI := &inflightLocalIntent{infl: aInfl}
	bLI := &inflightLocalIntent{infl: bInfl}

	coord := &twoNodeCoord{
		infl:          map[ifaces.NodeID]*inflight.Map{"n0": aInfl, "n1": bInfl},
		rank:          map[ifaces.NodeID]int32{"n0": 0, "n1": 1},
		lp:            map[ifaces.NodeID]*countingLocalPull{"n0": aLP, "n1": bLP},
		pleasePullsTo: map[ifaces.NodeID]int{},
		intentQueries: map[ifaces.NodeID]int{},
	}

	// Shared DHT: A is already advertised as a provider so pollDHT
	// terminates on the first poll. The test focuses on the
	// please_pull / inflight side; the DHT plane is not the thing
	// under test here.
	disco := &stubDisco{
		health: 1.0,
		providers: [][]ifaces.Provider{
			{{NodeID: "n0", Addr: "n0:5001"}},
		},
	}

	aMems := fakes.NewMembers("n0", cluster...)
	bMems := fakes.NewMembers("n1", cluster...)

	mkResolver := func(self ifaces.NodeID, mems ifaces.Members, infl *inflight.Map, li ifaces.LocalIntentProvider, lp ifaces.LocalPullStarter) *coldstart.Resolver {
		t.Helper()

		_ = self //nolint:errcheck // best-effort

		return coldstart.New(coldstart.Options{
			Members:      mems,
			Discovery:    disco,
			Coord:        coord,
			Inflight:     infl,
			HrwK:         2,
			HrwScope:     hrw.ScopeCluster,
			LocalIntent:  li,
			LocalPull:    lp,
			QueryTimeout: 200 * time.Millisecond,
			PollManifest: 10 * time.Millisecond,
			PollLayer:    10 * time.Millisecond,
		})
	}

	rA := mkResolver("n0", aMems, aInfl, aLI, aLP)
	rB := mkResolver("n1", bMems, bInfl, bLI, bLP)

	// Resolve concurrently. Both calls use the SAME outer ctx and
	// race against each other - that is the production scenario.
	var wg sync.WaitGroup

	resolveErrs := make([]error, 2)
	resolutions := make([]*coldstart.Resolution, 2)

	for i, r := range []*coldstart.Resolver{rA, rB} {
		i, r := i, r

		wg.Add(1)

		go func() {
			defer wg.Done()

			res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "docker.io", "library/nginx", 0)
			resolutions[i] = res
			resolveErrs[i] = err
		}()
	}

	wg.Wait()

	for i, err := range resolveErrs {
		if err != nil {
			t.Errorf("Resolve[%d] error: %v", i, err)
		}
	}

	// cache-hit: A originated exactly one origin pull.
	aLP.mu.Lock()
	gotAStarts, gotADupes := aLP.starts, aLP.dupes
	aLP.mu.Unlock()

	if gotAStarts != 1 {
		t.Errorf("F1: A originated %d pulls; want exactly 1 (dupes=%d)", gotAStarts, gotADupes)
	}

	// peer-fetch: B never originates.
	bLP.mu.Lock()
	gotBStarts := bLP.starts
	bLP.mu.Unlock()

	if gotBStarts != 0 {
		t.Errorf("F2: B originated %d pulls; want 0 (HRW rank 0 = A, B must never originate)", gotBStarts)
	}

	// B's rule-7 path may or may not have fired please_pull to A
	// (depending on whether A's inflight insert raced ahead of B's
	// PullIntentQuery). Either outcome satisfies cache-hit because aLP
	// dedupes via inflight.Start. But please_pull MUST NEVER be
	// addressed to B - A is the only legitimate target.
	coord.mu.Lock()
	pleasePullsToA := coord.pleasePullsTo["n0"]
	pleasePullsToB := coord.pleasePullsTo["n1"]
	intentToA := coord.intentQueries["n0"]
	coord.mu.Unlock()

	if pleasePullsToB != 0 {
		t.Errorf("please_pull was dispatched to B %d times; want 0 (B is not the designated puller)", pleasePullsToB)
	}

	if intentToA < 1 {
		t.Errorf("pull_intent_query to A fired %d times; want >= 1 (B must probe A)", intentToA)
	}

	t.Logf("two-node summary: A.starts=%d A.dupes=%d B.starts=%d please_pulls_to_A=%d intent_queries_to_A=%d",
		gotAStarts, gotADupes, gotBStarts, pleasePullsToA, intentToA)

	// cache-hit second leg: B's resolution must point at A (either via
	// rule-3 piggyback or rule-7 + DHT poll), not at B itself or
	// at empty.
	if resolutions[1] == nil {
		t.Fatalf("B's Resolution is nil; F1 requires B to observe A as the source")
	}

	if len(resolutions[1].Providers) == 0 {
		t.Errorf("B's Resolution has no providers; want at least A as provider")
	} else if resolutions[1].Providers[0].NodeID != "n0" {
		t.Errorf("B's Resolution first provider = %s; want n0 (= A)", resolutions[1].Providers[0].NodeID)
	}
}

// TestPullerSelectionIgnoresResponderRank asserts that puller
// selection uses the *requester's* HRW ranking, not whatever rank
// the responder reports back. Eighth code review #2: a peer mid-
// rollout or with stale membership informer cache can return a
// PullIntent with a wrong / -1 / inflated RecipientRank; if the
// requester sorted reachable responses by that field, two requesters
// computing the same HRW top-K could disagree on the winner during
// the convergence window, duplicating origin pulls.
//
// Setup: 4-node cluster, self = n3. Pick a digest where the
// requester computes top = [A=rank0, B=rank1, C=rank2, D=rank3].
// Have the responder at the requester's rank 0 lie and report
// RecipientRank=99; have the responder at requester's rank 1 lie
// and report RecipientRank=-1; have rank 2 report rank 0. All
// three responders report rule 7 (cold start, no cache, no
// in-flight). LocalPull is absent.
//
// Assertions:
// - sendPleasePull is dispatched to *requester* rank 0 (the
// node the requester locally computed as rank 0), NOT to
// responder-rank-0 (which here is the rank-2 liar).
// - Resolution providers start with that same node.
func TestPullerSelectionIgnoresResponderRank(t *testing.T) {
	nodes := clusterNodes()
	self := ifaces.NodeID("n3")
	d := digest.MustParse("sha256:" + rep('7', 64))

	top := hrw.TopK(nodes, d, 3)
	requesterRank0 := top[0].Node.ID
	requesterRank2 := top[2].Node.ID
	// Pre-fix, lying about responder rank steered selection.
	intents := map[ifaces.NodeID]ifaces.PullIntent{
		top[0].Node.ID: {RecipientRank: 99}, // true rank 0, lies as 99 (would be deprioritised)
		top[1].Node.ID: {RecipientRank: -1}, // true rank 1, lies as unknown
		top[2].Node.ID: {RecipientRank: 0},  // true rank 2, lies as rank 0 (would WIN pre-fix)
	}
	coord := &stubCoord{intents: intents}
	// Disco returns the *correct* rank-0 node as a provider so
	// pollDHT terminates after sendPleasePull fires.
	disco := &stubDisco{
		health: 1.0,
		providers: [][]ifaces.Provider{
			{{NodeID: requesterRank0, Addr: string(requesterRank0) + ":5001"}},
		},
	}

	r := buildResolver(t, coord, disco, self, nodes, coldstart.MetricsHooks{}, time.Now)

	res, err := r.Resolve(context.Background(), d, ifaces.KindManifest, "reg.example.com", "test/repo", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Requester-computed rank 0 must have received the please_pull;
	// the rank-2 liar must NOT have.
	coord.mu.Lock()
	defer coord.mu.Unlock()

	if len(coord.pleasePullCalls) != 1 {
		t.Fatalf("PleasePull call count: got %d (%v); want 1", len(coord.pleasePullCalls), coord.pleasePullCalls)
	}

	got := coord.pleasePullCalls[0]
	if got != requesterRank0 {
		t.Fatalf("please_pull dispatched to %s; want %s (requester rank 0). The rank-2 liar=%s reported rank 0 but must NOT have won.",
			got, requesterRank0, requesterRank2)
	}

	if res == nil || len(res.Providers) == 0 || res.Providers[0].NodeID != requesterRank0 {
		t.Errorf("Resolution providers[0] = %v; want first = %s (requester rank 0)", res, requesterRank0)
	}
}

// TestKindLabel asserts eighth-review #4: the three OriginRefKind
// values map to distinct Prometheus label values so config-blob
// fetches don't smear into the layer bucket.
//
// Why this matters: cold-start metrics
// (gantry_coldstart_duration_seconds, gantry_hrw_rank_mismatch_total,
// gantry_designated_puller_takeover_total) are labelled by `kind`,
// and a typical image pull traverses manifest -> config -> layer*.
// If KindConfig collapses into "layer" the config fetch (a small,
// usually fast pull) drags the layer p99 down and hides regressions
// in actual layer transfers. KindConfig.String already returns
// "config" so the package-internal label MUST agree.
//
// The default branch covers KindBlob (the common case) and any
// future enum value - "layer" is the conservative fallback for
// "we don't have a metric bucket for that yet", not for KindConfig.
func TestKindLabel(t *testing.T) {
	cases := []struct {
		name string
		kind ifaces.OriginRefKind
		want string
	}{
		{"KindManifest -> manifest", ifaces.KindManifest, "manifest"},
		{"KindConfig -> config (eighth-review #4)", ifaces.KindConfig, "config"},
		{"KindBlob -> layer", ifaces.KindBlob, "layer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coldstart.KindLabelForTest(tc.kind); got != tc.want {
				t.Errorf("kindLabel(%v) = %q; want %q", tc.kind, got, tc.want)
			}
		})
	}
}
