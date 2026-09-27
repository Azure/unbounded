// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
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

func TestPublicationCurrentAndSubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)
		p := r.Publications

		leader, cancel := context.WithCancel(t.Context())
		defer cancel()

		current, changed, err := p.CurrentAndSubscribe()
		if current != nil || !errors.Is(err, wire.Unavailable) {
			t.Fatalf("empty publication: %p, %v", current, err)
		}

		installed := reconcileTopology(t, r, leader)
		// Install between the snapshot and waiting must close the captured channel.
		<-changed

		current, changed, err = p.CurrentAndSubscribe()
		if current != installed || err != nil {
			t.Fatalf("installed publication: %p, %v", current, err)
		}

		go p.Suspend()

		<-changed

		current, changed, err = p.CurrentAndSubscribe()
		if current != nil || !errors.Is(err, wire.Unavailable) {
			t.Fatalf("suspended publication: %p, %v", current, err)
		}

		replayed := make(chan error, 1)

		go func() { replayed <- p.Install(installed) }()

		<-changed

		if err := <-replayed; err != nil {
			t.Fatal(err)
		}

		current, changed, err = p.CurrentAndSubscribe()
		if current != installed || err != nil {
			t.Fatalf("resumed publication: %p, %v", current, err)
		}

		select {
		case <-changed:
			t.Fatal("subscription returned an already-closed channel for unchanged state")
		default:
		}

		cancel()

		current, _, err = p.CurrentAndSubscribe()
		if current != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("lost leadership: %p, %v", current, err)
		}
	})
}

func TestPublicationBoundsOverflowAndInstallProof(t *testing.T) {
	r := initializedTopology(t)

	p := reconcileTopology(t, r, context.Background())
	for _, invalid := range []*CommittedPublication{nil, {}, {owner: r.Publications}} {
		if err := r.Publications.Install(invalid); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("forged install: %v", err)
		}
	}

	if err := NewPublications().Install(p); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("foreign install: %v", err)
	}

	cache, err := BuildCatalog([]racerv1.ClusterCache{catalogCache("cache", testNodeUID)})
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

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
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

func TestPollValidationAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)

		leader, loseLeadership := context.WithCancel(context.Background())
		defer loseLeadership()

		current := reconcileTopology(t, r, leader)
		identity := pollIdentity(r.Config, testNodeUID)
		sequence := current.Version().Sequence

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan error, 1)

		go func() { _, err := r.Publications.Wait(ctx, identity, &sequence); done <- err }()

		synctest.Wait()

		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("wait cancellation: %v", err)
		}

		if got, err := r.Publications.Wait(ctx, identity, nil); got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled immediate poll: %p, %v", got, err)
		}

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

		invalid := identity

		invalid.node = "not-a-uuid"
		if _, err := r.Publications.Wait(context.Background(), invalid, nil); !errors.Is(err, wire.Unauthenticated) {
			t.Fatalf("invalid identity: %v", err)
		}

		go func() { _, err := r.Publications.Wait(context.Background(), identity, &sequence); done <- err }()

		synctest.Wait()
		loseLeadership()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("leadership did not cancel wait: %v", err)
		}

		if _, err := r.Publications.Current(); !errors.Is(err, context.Canceled) {
			t.Fatalf("old leadership still serves: %v", err)
		}

		if got, err := r.Publications.Wait(context.Background(), identity, nil); got != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("immediate poll after leadership loss: %p, %v", got, err)
		}

		if _, err := current.WriteTo(io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("write after leadership: %v", err)
		}
	})
}

func TestPollImmediateReturnsWithoutAllocations(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()
	previous := reconcileTopology(t, r, ctx).Version().Sequence
	cache := catalogCache("cache", testNodeUID)

	if err := r.Create(ctx, &cache); err != nil {
		t.Fatal(err)
	}

	runKeys(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)

	current := reconcileTopology(t, r, ctx)
	identity := pollIdentity(r.Config, testNodeUID)

	for _, tc := range []struct {
		name  string
		after *wire.Sequence
	}{
		{name: "absent cursor"},
		{name: "older cursor", after: &previous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				got *CommittedPublication
				err error
			)

			allocations := testing.AllocsPerRun(100, func() {
				got, err = r.Publications.Wait(ctx, identity, tc.after)
			})

			if err != nil || got != current {
				t.Fatalf("immediate shared publication: %p, %v", got, err)
			}

			if allocations != 0 {
				t.Fatalf("immediate poll allocated: %v allocations per call", allocations)
			}
		})
	}
}

func TestPollCertificateExpiration(t *testing.T) {
	for _, name := range []string{"with context deadline", "without context deadline"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := initializedTopology(t)
				p := reconcileTopology(t, r, context.Background())
				identity := pollIdentity(r.Config, testNodeUID)
				identity.expires = time.Now().Add(time.Second)
				sequence := p.Version().Sequence
				ctx := context.Background()

				if name == "with context deadline" {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
				}

				got, err := r.Publications.Wait(ctx, identity, &sequence)
				if got != nil || !errors.Is(err, wire.Unauthenticated) || !time.Now().Equal(identity.expires) {
					t.Fatalf("expiration: %p, %v, time %v, expires %v", got, err, time.Now(), identity.expires)
				}
			})
		})
	}
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
	synctest.Test(t, func(t *testing.T) {
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

		synctest.Wait()

		cm, _, err := readVersion(ctx, r.APIReader, r.Config)
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
	})
}

func TestPollFanoutSharesOnePublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := initializedTopology(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		current := reconcileTopology(t, r, ctx)
		sequence := current.Version().Sequence

		const count = 256

		results := make(chan *CommittedPublication, count)
		errors := make(chan error, count)

		var wg sync.WaitGroup

		// Direct waiters share a node deliberately: only the HTTP server owns admission.
		for range count {
			identity := pollIdentity(r.Config, testNodeUID)

			wg.Go(func() { p, err := r.Publications.Wait(ctx, identity, &sequence); results <- p; errors <- err })
		}

		synctest.Wait()

		cache := catalogCache("cache", testNodeUID)
		if err := r.Create(ctx, &cache); err != nil {
			t.Fatal(err)
		}

		runKeys(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)

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
	})
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
