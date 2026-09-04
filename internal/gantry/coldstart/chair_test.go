// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package coldstart_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/chairs"
	"github.com/Azure/unbounded/internal/gantry/coldstart"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/inflight"
)

type chairSnapshotStub struct {
	mu        sync.Mutex
	snapshot  chairs.Snapshot
	refreshes []chairs.ID
}

func (s *chairSnapshotStub) Snapshot(_ context.Context, _ int64) (chairs.Snapshot, error) {
	return s.snapshot, nil
}

func (s *chairSnapshotStub) RefreshChair(_ context.Context, id chairs.ID) (chairs.Chair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refreshes = append(s.refreshes, id)

	for _, chair := range s.snapshot.Chairs {
		if chair.ID == id {
			chair.Generation++
			return chair, nil
		}
	}

	return chairs.Chair{}, errors.New("chair not found")
}

type chairCoordStub struct {
	mu         sync.Mutex
	calls      []ifaces.ChairAssignment
	fail       map[uint32]error
	staleFirst map[uint32]bool
	outcomes   map[uint32]ifaces.PleasePullOutcome
	onCall     func(ifaces.ChairAssignment)
}

func (s *chairCoordStub) PleasePullChair(_ context.Context, _ ifaces.NodeID, _, _ string, _ ifaces.OriginRefKind, digests []digest.Digest, assignment ifaces.ChairAssignment) ([]ifaces.PleasePullOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, assignment)
	if s.onCall != nil {
		s.onCall(assignment)
	}

	if err := s.fail[assignment.ChairID]; err != nil {
		return nil, err
	}

	status := ifaces.PleasePullStarted
	configured, hasConfigured := s.outcomes[assignment.ChairID]

	if s.staleFirst[assignment.ChairID] {
		delete(s.staleFirst, assignment.ChairID)

		status = ifaces.PleasePullStaleChair
	}

	out := make([]ifaces.PleasePullOutcome, 0, len(digests))
	for _, d := range digests {
		if hasConfigured {
			configured.Digest = d
			out = append(out, configured)
		} else {
			out = append(out, ifaces.PleasePullOutcome{Digest: d, Outcome: status})
		}
	}

	return out, nil
}

type backupDiscovery struct {
	ready atomic.Bool
}

func (d *backupDiscovery) FindProviders(context.Context, digest.Digest) ([]ifaces.Provider, error) {
	if d.ready.Load() {
		return []ifaces.Provider{{NodeID: "backup", Addr: "backup:5001"}}, nil
	}

	return nil, nil
}

func (*backupDiscovery) Health() float64 { return 1 }

func TestChairResolverUsesBackupsUntilEightSeedsAccept(t *testing.T) {
	d := digest.MustParse("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	snapshot := fullChairSnapshot(5)
	ranked := chairs.Rank(snapshot, d)

	coord := &chairCoordStub{fail: map[uint32]error{}}
	for _, chair := range ranked[:3] {
		coord.fail[uint32(chair.ID)] = errors.New("dial failed")
	}

	resolver := newTestChairResolver(&chairSnapshotStub{snapshot: snapshot}, coord, &stubDisco{
		providers: [][]ifaces.Provider{{{NodeID: "seed", Addr: "seed:5001"}}},
	})

	resolution, err := resolver.Resolve(context.Background(), d, ifaces.KindBlob, "registry.example.com", "repo/image", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolution.Providers) != 1 || resolution.Providers[0].NodeID != "seed" {
		t.Fatalf("providers = %+v", resolution.Providers)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if got := len(coord.calls); got != chairs.SeedCount+6 {
		t.Fatalf("chair calls = %d, want %d", got, chairs.SeedCount+6)
	}

	callCounts := make(map[uint32]int)
	for _, call := range coord.calls {
		callCounts[call.ChairID]++
	}

	for rankedIndex, chair := range ranked[:chairs.SeedCount+3] {
		want := 1
		if rankedIndex < 3 {
			want = 2
		}

		if got := callCounts[uint32(chair.ID)]; got != want {
			t.Fatalf("chair %s calls = %d, want %d", chair.ID.Name(), got, want)
		}

		delete(callCounts, uint32(chair.ID))
	}

	if len(callCounts) != 0 {
		t.Fatalf("unexpected chair calls: %v", callCounts)
	}
}

func TestChairResolverRefreshesStaleChairBeforeUsingBackup(t *testing.T) {
	d := digest.MustParse("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	snapshot := fullChairSnapshot(8)
	ranked := chairs.Rank(snapshot, d)
	cache := &chairSnapshotStub{snapshot: snapshot}
	coord := &chairCoordStub{staleFirst: map[uint32]bool{uint32(ranked[0].ID): true}}
	resolver := newTestChairResolver(cache, coord, &stubDisco{
		providers: [][]ifaces.Provider{{{NodeID: "seed", Addr: "seed:5001"}}},
	})

	if _, err := resolver.Resolve(context.Background(), d, ifaces.KindBlob, "registry.example.com", "repo/image", 0); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if len(cache.refreshes) != 1 || cache.refreshes[0] != ranked[0].ID {
		t.Fatalf("refreshes = %v, want [%s]", cache.refreshes, ranked[0].ID.Name())
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	if len(coord.calls) != chairs.SeedCount+1 {
		t.Fatalf("chair calls = %d, want %d", len(coord.calls), chairs.SeedCount+1)
	}

	var generations []int64

	for _, call := range coord.calls {
		if call.ChairID == uint32(ranked[0].ID) {
			generations = append(generations, call.Generation)
		}
	}

	if len(generations) != 2 || generations[1] != generations[0]+1 {
		t.Fatalf("retry generations = %v, want consecutive pair", generations)
	}
}

func TestChairResolverTrustedFailureShortCircuits(t *testing.T) {
	d := digest.MustParse("sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	snapshot := fullChairSnapshot(8)
	ranked := chairs.Rank(snapshot, d)
	coord := &chairCoordStub{outcomes: map[uint32]ifaces.PleasePullOutcome{
		uint32(ranked[0].ID): {
			Outcome:      ifaces.PleasePullRecentlyFailed,
			FailureClass: ifaces.FailureNotFound,
		},
	}}
	resolver := newTestChairResolver(&chairSnapshotStub{snapshot: snapshot}, coord, &stubDisco{})

	_, err := resolver.Resolve(context.Background(), d, ifaces.KindManifest, "registry.example.com", "repo/image", 0)
	if !errors.Is(err, coldstart.ErrFailureShortCircuit) {
		t.Fatalf("Resolve error = %v, want ErrFailureShortCircuit", err)
	}
}

func TestChairResolverUsesBackupAfterAcceptedPullNeverAdvertises(t *testing.T) {
	d := digest.MustParse("sha256:abababababababababababababababababababababababababababababababab")
	snapshot := fullChairSnapshot(8)
	ranked := chairs.Rank(snapshot, d)

	primary := make(map[uint32]struct{}, chairs.SeedCount)
	for _, chair := range ranked[:chairs.SeedCount] {
		primary[uint32(chair.ID)] = struct{}{}
	}

	discovery := &backupDiscovery{}
	coord := &chairCoordStub{onCall: func(assignment ifaces.ChairAssignment) {
		if _, isPrimary := primary[assignment.ChairID]; !isPrimary {
			discovery.ready.Store(true)
		}
	}}
	resolver := newTestChairResolver(&chairSnapshotStub{snapshot: snapshot}, coord, discovery)

	resolution, err := resolver.Resolve(context.Background(), d, ifaces.KindManifest, "registry.example.com", "repo/image", 0)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolution.Providers) != 1 || resolution.Providers[0].NodeID != "backup" {
		t.Fatalf("providers = %+v, want backup provider", resolution.Providers)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()

	usedBackup := false

	for _, call := range coord.calls {
		if _, isPrimary := primary[call.ChairID]; !isPrimary {
			usedBackup = true
			break
		}
	}

	if !usedBackup {
		t.Fatal("resolver did not activate an ordered backup after DHT timeout")
	}
}

func newTestChairResolver(cache coldstart.ChairSnapshotCache, coord ifaces.ChairCoordinator, discovery coldstart.Discovery) *coldstart.ChairResolver {
	return coldstart.NewChairResolver(coldstart.ChairOptions{
		Chairs:    cache,
		Discovery: discovery,
		Coord:     coord,
		Inflight: inflight.New(inflight.Stalls{
			ManifestConfig:   20 * time.Millisecond,
			LayerFloor:       20 * time.Millisecond,
			LayerBytesPerSec: 1,
			LayerMultiplier:  1,
		}, nil),
		SelfPeerID:   "self",
		CurrentEpoch: func() int64 { return 8 },
		QueryTimeout: time.Second,
		PollLayer:    time.Millisecond,
	})
}

func fullChairSnapshot(epoch int64) chairs.Snapshot {
	snapshot := chairs.Snapshot{Epoch: epoch}
	for index := range chairs.Count {
		snapshot.Chairs = append(snapshot.Chairs, chairs.Chair{
			ID: chairs.ID(index),
			Holder: chairs.Holder{
				PeerID:       ifaces.NodeID(fmt.Sprintf("peer-%02d", index)),
				P2PAddrs:     []string{fmt.Sprintf("/ip4/10.0.0.%d/tcp/4001/p2p/peer-%02d", index+1, index)},
				TransferAddr: fmt.Sprintf("10.0.0.%d:5001", index+1),
			},
			Generation:      1,
			AssignmentEpoch: epoch,
		})
	}

	return snapshot
}
