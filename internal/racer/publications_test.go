// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func pollIdentity(cfg Config, node wire.NodeID) NodeIdentity {
	return NodeIdentity{cluster: cfg.Cluster, node: node, expires: time.Now().Add(time.Hour)}
}

func awaitPolls(t *testing.T, p *Publications, count int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		n := len(p.polls)
		p.mu.Unlock()

		if n == count {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("poll admission did not reach %d", count)
}

func TestPublicationBoundsOverflowAndInstallProof(t *testing.T) {
	r := initializedTopology(t)

	p := reconcileTopology(t, r, context.Background())
	for _, invalid := range []*CommittedPublication{nil, {}, {owner: r.Publications}} {
		if err := r.Publications.Install(invalid); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("forged install: %v", err)
		}
	}

	if err := NewPublications(r.Config.Limits).Install(p); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("foreign install: %v", err)
	}

	cache, err := BuildCatalog([]racerv1.ClusterCache{catalogCache("cache", testNodeUID, nil)})
	if err != nil {
		t.Fatal(err)
	}

	previous := p.Version()

	previous.Sequence = ^wire.Sequence(0)
	if _, err := r.Publications.Prepare(previous, "rv", nil, cache); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("overflow: %v", err)
	}

	if _, err := r.Publications.Prepare(previous, "rv", nil, nil); err != nil {
		t.Fatalf("unchanged maximum counter rejected: %v", err)
	}

	previous.MembershipVersion = ^wire.MembershipVersion(0)

	members := AcceptedMembers{testNodeUID: {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:8082"}}
	if _, err := r.Publications.Prepare(previous, "rv", members, nil); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("membership overflow: %v", err)
	}

	if _, err := r.Publications.Prepare(p.Version(), "rv", AcceptedMembers{testNodeUID: {Node: testOtherUID}}, nil); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("map identity: %v", err)
	}

	oversized := members[testNodeUID]

	oversized.Rails = []wire.Rail{{Fabric: strings.Repeat("a", wire.MaxPublicationBytes)}}
	if _, err := r.Publications.Prepare(p.Version(), "rv", AcceptedMembers{testNodeUID: oversized}, nil); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("oversized candidate: %v", err)
	}
}

func TestPublicationDeepIsolationAndReplay(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()
	old := reconcileTopology(t, r, ctx)

	cm, previous, err := r.readVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	numa := uint32(1)
	members := AcceptedMembers{testNodeUID: {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:8082", Rails: []wire.Rail{{Fabric: "fabric", NUMANode: &numa}}}}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, members, nil)
	if err != nil {
		t.Fatal(err)
	}

	numa = 9

	delete(members, testNodeUID)

	committed, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Publications.Install(committed); err != nil {
		t.Fatal(err)
	}

	decoded, err := wire.DecodePublication(strings.NewReader(committed.Encoding()))
	if err != nil || len(decoded.Members) != 1 || *decoded.Members[0].Rails[0].NUMANode != 1 {
		t.Fatalf("mutable alias: %+v, %v", decoded, err)
	}

	if err := r.Publications.Install(old); !errors.Is(err, wire.Conflict) {
		t.Fatalf("rollback: %v", err)
	}

	conflicting := *committed

	conflicting.encoded = "different"
	if err := r.Publications.Install(&conflicting); !errors.Is(err, wire.Conflict) {
		t.Fatalf("conflicting replay: %v", err)
	}

	if err := r.Publications.Install(committed); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
}

func TestPollAdmissionAndCancellation(t *testing.T) {
	r := initializedTopology(t)

	leader, loseLeadership := context.WithCancel(context.Background())
	defer loseLeadership()

	current := reconcileTopology(t, r, leader)
	r.Publications.Limits.MaxPolls = 1
	identity := pollIdentity(r.Config, testNodeUID)
	sequence := current.Version().Sequence
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { _, err := r.Publications.Wait(ctx, identity, &sequence); done <- err }()

	awaitPolls(t, r.Publications, 1)

	for _, node := range []wire.NodeID{testNodeUID, testOtherUID} {
		if _, err := r.Publications.Wait(context.Background(), pollIdentity(r.Config, node), &sequence); !errors.Is(err, wire.Overloaded) {
			t.Fatalf("poll limit: %v", err)
		}
	}

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation: %v", err)
	}

	awaitPolls(t, r.Publications, 0)

	if got, err := r.Publications.Wait(context.Background(), identity, nil); err != nil || got != current {
		t.Fatalf("immediate shared snapshot: %p, %v", got, err)
	}

	for _, cursor := range []wire.Sequence{0, sequence + 1} {
		if _, err := r.Publications.Wait(context.Background(), identity, &cursor); !errors.Is(err, wire.Conflict) {
			t.Fatalf("future/zero cursor: %v", err)
		}
	}

	wrong := identity

	wrong.cluster = testNodeUID
	if _, err := r.Publications.Wait(context.Background(), wrong, nil); !errors.Is(err, wire.Forbidden) {
		t.Fatalf("wrong cluster: %v", err)
	}

	expired := identity

	expired.expires = time.Now().Add(-time.Second)
	if _, err := r.Publications.Wait(context.Background(), expired, nil); !errors.Is(err, wire.Unauthenticated) {
		t.Fatalf("expired identity: %v", err)
	}

	go func() { _, err := r.Publications.Wait(context.Background(), identity, &sequence); done <- err }()

	awaitPolls(t, r.Publications, 1)
	loseLeadership()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("leadership did not cancel wait: %v", err)
	}

	if _, err := r.Publications.Current(); !errors.Is(err, context.Canceled) {
		t.Fatalf("old leadership still serves: %v", err)
	}

	if _, err := current.WriteTo(io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("write after leadership: %v", err)
	}
}

func TestPollCertificateExpiration(t *testing.T) {
	r := initializedTopology(t)
	p := reconcileTopology(t, r, context.Background())
	identity := pollIdentity(r.Config, testNodeUID)
	identity.expires = time.Now().Add(20 * time.Millisecond)
	sequence := p.Version().Sequence

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := r.Publications.Wait(ctx, identity, &sequence); !errors.Is(err, wire.Unauthenticated) {
		t.Fatalf("expiration: %v", err)
	}

	awaitPolls(t, r.Publications, 0)
}

func TestPollNormalTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)
		p := reconcileTopology(t, r, context.Background())
		identity := pollIdentity(r.Config, testNodeUID)
		sequence := p.Version().Sequence
		start := time.Now()

		got, err := r.Publications.Wait(context.Background(), identity, &sequence)
		if err != nil || got != nil || time.Since(start) != wire.PollWait {
			t.Fatalf("normal timeout: %p, %v, %v", got, err, time.Since(start))
		}
	})
}

func TestDurableLossWithdrawsPublication(t *testing.T) {
	r := initializedTopology(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := reconcileTopology(t, r, ctx)
	sequence := p.Version().Sequence
	done := make(chan error, 1)

	go func() {
		_, err := r.Publications.Wait(ctx, pollIdentity(r.Config, testNodeUID), &sequence)
		done <- err
	}()

	awaitPolls(t, r.Publications, 1)

	cm, _, err := r.readVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(ctx, cm); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err == nil {
		t.Fatal("missing counter accepted")
	}

	if err := <-done; !errors.Is(err, wire.Unavailable) {
		t.Fatalf("waiting poll did not fail closed: %v", err)
	}

	if _, err := r.Publications.Current(); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("durable loss still serves: %v", err)
	}
}

func TestPollFanoutSharesOnePublication(t *testing.T) {
	r := initializedTopology(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	current := reconcileTopology(t, r, ctx)
	sequence := current.Version().Sequence

	const count = 256

	results := make(chan *CommittedPublication, count)
	errors := make(chan error, count)

	var wg sync.WaitGroup

	for i := range count {
		identity := pollIdentity(r.Config, wire.NodeID(fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)))

		wg.Go(func() { p, err := r.Publications.Wait(ctx, identity, &sequence); results <- p; errors <- err })
	}

	awaitPolls(t, r.Publications, count)

	cache := catalogCache("cache", testNodeUID, nil)
	if err := r.Create(ctx, &cache); err != nil {
		t.Fatal(err)
	}

	next := reconcileTopology(t, r, ctx)

	wg.Wait()

	for range count {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}

		if p := <-results; p != next {
			t.Fatalf("waiter copied or missed publication: %p != %p", p, next)
		}
	}

	awaitPolls(t, r.Publications, 0)
}

type boundedWriter struct {
	largest int
	calls   int
	cancel  context.CancelFunc
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.largest = max(w.largest, len(p))

	w.calls++
	if w.cancel != nil {
		w.cancel()
	}

	return len(p), nil
}

func TestPublicationWriteScratchAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &CommittedPublication{leadership: ctx, encoded: strings.Repeat("x", 100_000)}

	w := &boundedWriter{}
	if n, err := p.WriteTo(w); err != nil || n != 100_000 || w.largest > 32*1024 || w.calls != 4 {
		t.Fatalf("unbounded write scratch: %d, %v, %+v", n, err, w)
	}

	w = &boundedWriter{cancel: cancel}
	if n, err := p.WriteTo(w); !errors.Is(err, context.Canceled) || n != 32*1024 || w.calls != 1 {
		t.Fatalf("write ignored cancellation: %d, %v", n, err)
	}
}
