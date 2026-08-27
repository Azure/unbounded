// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
)

type blockingPrefetchCoord struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   chan struct{}
	release   chan struct{}
}

func (c *blockingPrefetchCoord) PullIntentQuery(context.Context, ifaces.NodeID, digest.Digest) (ifaces.PullIntent, error) {
	return ifaces.PullIntent{}, nil
}

func (c *blockingPrefetchCoord) PleasePull(ctx context.Context, _ ifaces.NodeID, _, _ string, _ ifaces.OriginRefKind, digests []digest.Digest) ([]ifaces.PleasePullOutcome, error) {
	c.mu.Lock()

	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()

	c.started <- struct{}{}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
	}

	c.mu.Lock()
	c.active--
	c.mu.Unlock()

	outcomes := make([]ifaces.PleasePullOutcome, len(digests))
	for index, d := range digests {
		outcomes[index] = ifaces.PleasePullOutcome{Digest: d, Outcome: ifaces.PleasePullStarted}
	}

	return outcomes, nil
}

// digestHex renders i as a 64-char sha256 hex body.
func digestHex(i int) string {
	const hex = "0123456789abcdef"

	out := []byte("0000000000000000000000000000000000000000000000000000000000000000")

	n := uint(i)
	for j := len(out) - 1; n > 0 && j >= 0; j-- {
		out[j] = hex[n%16]
		n /= 16
	}

	return string(out)
}

func TestPrefetchChildren_ReplicatesToTopNPullers(t *testing.T) {
	cluster := clusterNodes() // n0..n3
	self := ifaces.NodeID("n3")

	// Use the first three closest peers as pullers.
	// non-self nodes, so replicas=3 fans the layer out to n0, n1, n2.
	var (
		d     digest.Digest
		found bool
	)

	for i := 0; i < 8192; i++ {
		cand := digest.MustParse("sha256:" + digestHex(i))

		ids := make(map[ifaces.NodeID]struct{})
		for _, s := range topN(cluster, 3) {
			ids[s.ID] = struct{}{}
		}

		if _, self3 := ids[self]; len(ids) == 3 && !self3 {
			d = cand
			found = true

			break
		}
	}

	if !found {
		t.Fatal("could not find a digest whose top-3 pullers exclude self")
	}

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}
	r := buildResolverWithReplicas(t, coord, disco, self, cluster, 3, coldstart.MetricsHooks{})

	children := []coldstart.ChildDigest{{Digest: d, Kind: ifaces.KindBlob}}
	if err := r.PrefetchChildren(context.Background(), children, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchChildren: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	// One layer digest, replicated to the top-3 pullers => 3 distinct
	// please_pull RPCs (3 initial origin seeds instead of 1).
	seen := make(map[ifaces.NodeID]struct{})
	for _, id := range coord.pleasePullCalls {
		seen[id] = struct{}{}
	}

	if len(coord.pleasePullCalls) != 3 || len(seen) != 3 {
		t.Fatalf("PleasePull calls: got %v, want 3 distinct pullers (top-3 replication)", coord.pleasePullCalls)
	}
}

func TestPrefetchChildren_ReportsRemoteGroupOutcomes(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")

	var (
		d     digest.Digest
		found bool
	)

	for i := 0; i < 8192; i++ {
		candidate := digest.MustParse("sha256:" + digestHex(i))

		top := topN(cluster, 2)
		if len(top) == 2 && top[0].ID != self && top[1].ID != self {
			d = candidate
			found = true

			break
		}
	}

	if !found {
		t.Fatal("could not find digest with two remote pullers")
	}

	top := topN(cluster, 2)
	coord := &stubCoord{pleasePullErrs: map[ifaces.NodeID]error{
		top[0].ID: errors.New("dial failed"),
	}}

	var (
		mu       sync.Mutex
		outcomes []string
	)

	r := buildResolverWithReplicas(t, coord, &stubDisco{health: 1.0}, self, cluster, 2, coldstart.MetricsHooks{
		OnPrefetchGroup: func(target, outcome string) {
			mu.Lock()
			defer mu.Unlock()

			outcomes = append(outcomes, target+":"+outcome)
		},
	})

	err := r.PrefetchChildren(context.Background(), []coldstart.ChildDigest{{Digest: d, Kind: ifaces.KindBlob}}, "docker.io", "library/nginx")
	if !errors.Is(err, coldstart.ErrPrefetchPartial) {
		t.Fatalf("PrefetchChildren error = %v, want ErrPrefetchPartial", err)
	}

	mu.Lock()
	defer mu.Unlock()

	sort.Strings(outcomes)

	if got, want := fmt.Sprint(outcomes), "[remote:error remote:success]"; got != want {
		t.Fatalf("group outcomes = %s, want %s", got, want)
	}
}

func TestPrefetchChildren_BoundsRemoteGroupConcurrency(t *testing.T) {
	nodes := make([]ifaces.Node, 8)
	for index := range nodes {
		nodes[index] = ifaces.Node{ID: ifaces.NodeID(fmt.Sprintf("n%d", index))}
	}

	coord := &blockingPrefetchCoord{
		started: make(chan struct{}, len(nodes)),
		release: make(chan struct{}),
	}
	resolver := coldstart.New(coldstart.Options{
		Self:                        "requester",
		Discovery:                   &stubDisco{health: 1.0, closest: nodes},
		Coord:                       coord,
		Inflight:                    inflight.New(inflight.DefaultStalls(), time.Now),
		PrefetchPullerReplicas:      len(nodes),
		PrefetchMaxConcurrentGroups: 2,
		QueryTimeout:                time.Second,
	})

	d := digest.MustParse("sha256:" + digestHex(9000))
	done := make(chan error, 1)

	go func() {
		done <- resolver.PrefetchChildren(context.Background(), []coldstart.ChildDigest{{Digest: d, Kind: ifaces.KindBlob}}, "docker.io", "library/nginx")
	}()

	for range 2 {
		select {
		case <-coord.started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for bounded prefetch workers")
		}
	}

	select {
	case <-coord.started:
		t.Fatal("a third remote group started while the two slots were occupied")
	case <-time.After(50 * time.Millisecond):
	}

	close(coord.release)

	if err := <-done; err != nil {
		t.Fatalf("PrefetchChildren: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if coord.maxActive != 2 {
		t.Fatalf("max active groups = %d, want 2", coord.maxActive)
	}
}

func TestPrefetchChildren_SinglePullerByDefault(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	// One digest whose rank-0 puller is non-self.
	targets := map[ifaces.NodeID]int{"n0": 1}

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, disco, cluster, targets)
	// buildResolver leaves PrefetchPullerReplicas unset => defaults to 1.
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	children := []coldstart.ChildDigest{{Digest: digests[0], Kind: ifaces.KindBlob}}
	if err := r.PrefetchChildren(context.Background(), children, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchChildren: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.pleasePullCalls); got != 1 {
		t.Fatalf("PleasePull calls: got %d want 1 (default single puller): %v", got, coord.pleasePullCalls)
	}
}

func TestPrefetchLayers_GroupsByPuller(t *testing.T) {
	cluster := clusterNodes()
	// We are n3; everyone else is a candidate puller.
	self := ifaces.NodeID("n3")
	// Pick digests so 4 land on n0, 3 land on n1, 2 land on n2. None
	// should land on n3 (which is self).
	targets := map[ifaces.NodeID]int{"n0": 4, "n1": 3, "n2": 2}

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, disco, cluster, targets)
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got, want := len(coord.pleasePullCalls), 3; got != want {
		t.Fatalf("PleasePull call count: got %d want %d (calls=%v)",
			got, want, coord.pleasePullCalls)
	}

	got := append([]ifaces.NodeID{}, coord.pleasePullCalls...)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

	want := []ifaces.NodeID{"n0", "n1", "n2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("PleasePull[%d]: got %s want %s", i, got[i], want[i])
		}
	}
	// Every call must carry the same registry+repo.
	for i, reg := range coord.pleasePullRegs {
		if reg != "docker.io" {
			t.Fatalf("call[%d] registry: got %q want docker.io", i, reg)
		}

		if coord.pleasePullRepos[i] != "library/nginx" {
			t.Fatalf("call[%d] repository: got %q want library/nginx", i, coord.pleasePullRepos[i])
		}
	}
}

func TestPrefetchLayers_SkipsSelf(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n0")
	// All digests should land on n0 (self). Resulting RPC count: 0.
	targets := map[ifaces.NodeID]int{"n0": 5}

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, disco, cluster, targets)
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.pleasePullCalls); got != 0 {
		t.Fatalf("PleasePull calls: got %d (%v), want 0 (all digests route to self)",
			got, coord.pleasePullCalls)
	}
}

func TestPrefetchLayers_DedupesDigests(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}

	// One unique digest, repeated 5 times.
	d := routeDigests(t, disco, cluster, map[ifaces.NodeID]int{"n0": 1})[0]
	digests := []digest.Digest{d, d, d, d, d}
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "lib/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.pleasePullCalls); got != 1 {
		t.Fatalf("PleasePull calls: got %d want 1 (dedupe should collapse to single batch)", got)
	}
}

func TestPrefetchLayers_EmptyRegistryRejected(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	coord := &stubCoord{}
	disco := &stubDisco{closest: cluster}
	d := routeDigests(t, disco, cluster, map[ifaces.NodeID]int{"n0": 1})[0]
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	err := r.PrefetchLayers(context.Background(), []digest.Digest{d}, "", "lib/nginx")
	if err == nil {
		t.Fatalf("PrefetchLayers with empty registry: expected error, got nil")
	}

	if !errors.Is(err, coldstart.ErrPrefetchInvalid) {
		t.Fatalf("err is not ErrPrefetchInvalid: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.pleasePullCalls); got != 0 {
		t.Fatalf("invalid call must not issue any RPC, got %d calls", got)
	}
}

func TestPrefetchLayers_EmptyDigestListNoop(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	coord := &stubCoord{}
	disco := &stubDisco{closest: cluster}

	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)
	if err := r.PrefetchLayers(context.Background(), nil, "docker.io", "lib/nginx"); err != nil {
		t.Fatalf("empty digest list: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.pleasePullCalls); got != 0 {
		t.Fatalf("empty digest list: got %d calls, want 0", got)
	}
}

func TestPrefetchLayers_PartialFailureReported(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	targets := map[ifaces.NodeID]int{"n0": 1, "n1": 1}

	coord := &stubCoord{
		pleasePullErrs: map[ifaces.NodeID]error{
			"n0": errors.New("simulated transport failure"),
		},
	}
	disco := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, disco, cluster, targets)
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	err := r.PrefetchLayers(context.Background(), digests, "docker.io", "lib/nginx")
	if err == nil {
		t.Fatalf("expected partial-failure error, got nil")
	}

	if !errors.Is(err, coldstart.ErrPrefetchPartial) {
		t.Fatalf("err is not ErrPrefetchPartial: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	// Both pullers were contacted even though one failed.
	if got := len(coord.pleasePullCalls); got != 2 {
		t.Fatalf("PleasePull calls: got %d want 2", got)
	}
}

func TestPrefetchLayers_MetricsFireOnce(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	targets := map[ifaces.NodeID]int{"n0": 2, "n1": 2, "n2": 1}
	disco := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, disco, cluster, targets)

	var (
		batchMu                  sync.Mutex
		pullerCount, digestCount int
		calls                    int
	)

	metrics := coldstart.MetricsHooks{
		OnPrefetchBatch: func(pullers, ds int) {
			batchMu.Lock()
			defer batchMu.Unlock()

			calls++
			pullerCount = pullers
			digestCount = ds
		},
	}
	coord := &stubCoord{}
	r := buildResolver(t, coord, disco, self, cluster, metrics, time.Now)

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "lib/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	batchMu.Lock()
	defer batchMu.Unlock()

	if calls != 1 {
		t.Fatalf("OnPrefetchBatch call count: got %d want 1", calls)
	}

	if pullerCount != 3 {
		t.Fatalf("OnPrefetchBatch pullers: got %d want 3", pullerCount)
	}

	if digestCount != 5 {
		t.Fatalf("OnPrefetchBatch digests: got %d want 5", digestCount)
	}
}

// TestPrefetchChildren_SplitsByKindOnSamePuller is the load-bearing
// invariant for the observability fix: a single
// manifest serve typically produces ONE config + N layers, and when
// closest-peer selection sends both to the same puller, the "all digests
// in a batch MUST share kind" rule forces TWO PleasePull RPCs (one
// per kind) rather than one mixed batch. Without this split, the
// config bucket on p2p_origin_pull_total stays permanently zero
// because the wire would carry every child as KindBlob.
func TestPrefetchChildren_SplitsByKindOnSamePuller(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}

	// Two digests both route to n0 - one config, one blob.
	dgs := routeDigests(t, disco, cluster, map[ifaces.NodeID]int{"n0": 2})
	children := []coldstart.ChildDigest{
		{Digest: dgs[0], Kind: ifaces.KindConfig},
		{Digest: dgs[1], Kind: ifaces.KindBlob},
	}
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	if err := r.PrefetchChildren(context.Background(), children, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchChildren: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	// One puller, two kinds -> exactly two PleasePull RPCs, both to n0.
	if got, want := len(coord.pleasePullCalls), 2; got != want {
		t.Fatalf("PleasePull call count: got %d want %d (calls=%v)",
			got, want, coord.pleasePullCalls)
	}

	for i, id := range coord.pleasePullCalls {
		if id != "n0" {
			t.Errorf("call[%d] puller: got %s want n0", i, id)
		}
	}
	// Kinds-on-the-wire must include both KindConfig and KindBlob; no
	// downgrade to a single mixed batch.
	seen := map[ifaces.OriginRefKind]bool{}
	for _, k := range coord.pleasePullKinds {
		seen[k] = true
	}

	if !seen[ifaces.KindConfig] {
		t.Errorf("KindConfig not seen on wire; got %v", coord.pleasePullKinds)
	}

	if !seen[ifaces.KindBlob] {
		t.Errorf("KindBlob not seen on wire; got %v", coord.pleasePullKinds)
	}
	// Each RPC must carry exactly one digest (no mixing).
	for i, ds := range coord.pleasePullDgs {
		if len(ds) != 1 {
			t.Errorf("call[%d] batch size: got %d want 1 (kind splitting must not pack across kinds)", i, len(ds))
		}
	}
}

func TestPrefetchChildren_PropagatesDelegatedAuthorization(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}
	dgs := routeDigests(t, disco, cluster, map[ifaces.NodeID]int{"n0": 2})
	children := []coldstart.ChildDigest{
		{Digest: dgs[0], Kind: ifaces.KindConfig},
		{Digest: dgs[1], Kind: ifaces.KindBlob},
	}
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)
	ctx := registryauth.WithAuthorization(context.Background(), "Bearer requester-token")

	if err := r.PrefetchChildren(ctx, children, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchChildren: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if len(coord.pleasePullAuth) == 0 {
		t.Fatal("no PleasePull calls recorded")
	}

	for i, authorization := range coord.pleasePullAuth {
		if authorization != "Bearer requester-token" {
			t.Fatalf("PleasePull[%d] authorization = %q, want requester token", i, authorization)
		}
	}
}

func TestPrefetchChildren_RemoteDispatchUsesDeterministicCoordinators(t *testing.T) {
	cluster := clusterNodes()
	routing := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, routing, cluster, map[ifaces.NodeID]int{
		"n0": 1,
		"n1": 1,
		"n2": 1,
		"n3": 1,
	})

	children := make([]coldstart.ChildDigest, 0, len(digests))
	for _, d := range digests {
		children = append(children, coldstart.ChildDigest{Digest: d, Kind: ifaces.KindBlob})
	}

	callersWithRemoteDispatch := 0
	totalRemoteGroups := 0
	totalLocalGroups := 0
	manifestDigest := digest.MustParse("sha256:" + strings.Repeat("f", 64))

	for _, node := range cluster {
		coord := &stubCoord{}
		localPull := &stubLocalPull{}
		now := time.Now
		resolver := coldstart.New(coldstart.Options{
			Self:                        node.ID,
			Discovery:                   routing,
			Coord:                       coord,
			Inflight:                    inflight.New(inflight.DefaultStalls(), now),
			Now:                         now,
			LocalPull:                   localPull,
			PrefetchCoordinatorReplicas: 1,
			QueryTimeout:                200 * time.Millisecond,
		})

		if err := resolver.PrefetchManifestChildren(context.Background(), manifestDigest, children, "docker.io", "library/nginx"); err != nil {
			t.Fatalf("PrefetchChildren for %s: %v", node.ID, err)
		}

		coord.mu.Lock()
		groups := len(coord.pleasePullCalls)
		coord.mu.Unlock()

		if groups > 0 {
			callersWithRemoteDispatch++
			totalRemoteGroups += groups
		}

		localPull.mu.Lock()
		totalLocalGroups += len(localPull.digests)
		localPull.mu.Unlock()
	}

	if callersWithRemoteDispatch != 1 {
		t.Fatalf("callers with remote dispatch = %d, want 1", callersWithRemoteDispatch)
	}

	if totalRemoteGroups != len(cluster)-1 {
		t.Fatalf("remote groups = %d, want %d", totalRemoteGroups, len(cluster)-1)
	}

	if totalLocalGroups != len(cluster) {
		t.Fatalf("local groups = %d, want %d", totalLocalGroups, len(cluster))
	}
}

// TestPrefetchChildren_DistinctPullersBatchedPerKind covers the
// cross-product case: 2 kinds × 2 distinct pullers -> 4 PleasePull
// RPCs. Confirms the (puller, kind) grouping key works in both
// dimensions.
func TestPrefetchChildren_DistinctPullersBatchedPerKind(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")
	// Want 2 digests on n0 and 2 digests on n1 - 4 digests total.
	disco := &stubDisco{health: 1.0, closest: cluster}
	dgs := routeDigests(t, disco, cluster, map[ifaces.NodeID]int{"n0": 2, "n1": 2})

	// routeDigests fills targets in sorted node order, so the first two
	// digests route to n0 and the second two to n1.
	bySrc := map[ifaces.NodeID][]digest.Digest{
		"n0": {dgs[0], dgs[1]},
		"n1": {dgs[2], dgs[3]},
	}

	children := []coldstart.ChildDigest{
		{Digest: bySrc["n0"][0], Kind: ifaces.KindConfig},
		{Digest: bySrc["n0"][1], Kind: ifaces.KindBlob},
		{Digest: bySrc["n1"][0], Kind: ifaces.KindConfig},
		{Digest: bySrc["n1"][1], Kind: ifaces.KindBlob},
	}

	coord := &stubCoord{}
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	if err := r.PrefetchChildren(context.Background(), children, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchChildren: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got, want := len(coord.pleasePullCalls), 4; got != want {
		t.Fatalf("PleasePull call count: got %d want %d (kinds=%v pullers=%v)",
			got, want, coord.pleasePullKinds, coord.pleasePullCalls)
	}
	// Each (puller, kind) combination appears exactly once.
	got := map[string]int{}

	for i, id := range coord.pleasePullCalls {
		k := string(id) + "/" + coord.pleasePullKinds[i].String()
		got[k]++
	}

	for _, want := range []string{"n0/config", "n0/blob", "n1/config", "n1/blob"} {
		if got[want] != 1 {
			t.Errorf("expected exactly one %s call, got %d (full map: %v)", want, got[want], got)
		}
	}
}

// TestPrefetchLayers_BackCompatTagsAllAsKindBlob proves the
// PrefetchLayers wrapper preserves its historical "every digest is a
// blob" behaviour even though it now delegates to PrefetchChildren.
// Important for older callers (and for the implicit assumption
// PrefetchLayers' kind is KindBlob inside other tests).
func TestPrefetchLayers_BackCompatTagsAllAsKindBlob(t *testing.T) {
	cluster := clusterNodes()
	self := ifaces.NodeID("n3")

	coord := &stubCoord{}
	disco := &stubDisco{health: 1.0, closest: cluster}
	digests := routeDigests(t, disco, cluster, map[ifaces.NodeID]int{"n0": 2})
	r := buildResolver(t, coord, disco, self, cluster, coldstart.MetricsHooks{}, time.Now)

	if err := r.PrefetchLayers(context.Background(), digests, "docker.io", "library/nginx"); err != nil {
		t.Fatalf("PrefetchLayers: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got, want := len(coord.pleasePullCalls), 1; got != want {
		t.Fatalf("PleasePull call count: got %d want %d", got, want)
	}

	if coord.pleasePullKinds[0] != ifaces.KindBlob {
		t.Errorf("kind on wire: got %v want KindBlob (back-compat)", coord.pleasePullKinds[0])
	}
}
