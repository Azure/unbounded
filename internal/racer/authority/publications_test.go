// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

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

func TestPublicationDeltaSelectionAndDisconnectedFallback(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()
	reconcileTopology(t, r, ctx)

	members := AcceptedMembers{}

	for i := range 100 {
		id := wire.NodeID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i))
		members[id] = wire.Member{Node: id, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{}}
	}

	install := func() *CommittedPublication {
		t.Helper()

		cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
		if err != nil {
			t.Fatal(err)
		}

		prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, members, nil)
		if err != nil {
			t.Fatal(err)
		}

		committed, err := r.CommitVersion(ctx, prepared)
		if err != nil {
			t.Fatal(err)
		}

		if err := r.Publications.Install(committed); err != nil {
			t.Fatal(err)
		}

		return committed
	}
	base := install()
	id := wire.NodeID("22222222-2222-4222-8222-000000000000")
	m := members[id]
	m.Shares = 9
	members[id] = m
	next := install()

	delta := next.ForBase(base.record.ContentHash)
	if len(delta.encoded) >= len(next.encoded) {
		t.Fatal("delta was not selected")
	}

	decoded, err := wire.DecodePublication(strings.NewReader(base.encoded))
	if err != nil {
		t.Fatal(err)
	}

	applied, err := wire.ApplyDelta(decoded, strings.NewReader(delta.encoded))
	if err != nil {
		t.Fatal(err)
	}

	hash, _, err := wire.ContentHashes(applied)
	if err != nil || hash != next.record.ContentHash {
		t.Fatalf("target mismatch: %v", err)
	}

	if next.ForBase("missing").encoded != next.encoded || next.ForBase("").encoded != next.encoded {
		t.Fatal("missing-base fallback failed")
	}
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
		if err != nil || current.encoded != installed.encoded || current.authority == installed.authority || installed.authority.Err() == nil {
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

	previous := p.record

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

	if _, err := r.Publications.Prepare(p.record, "rv", AcceptedMembers{testNodeUID: {Node: testOtherUID}}, nil); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("map identity: %v", err)
	}

	oversized := members[testNodeUID]

	oversized.RDMANICs = []wire.RDMANIC{{Device: strings.Repeat("a", wire.MaxPublicationBytes), Port: 1}}
	if _, err := r.Publications.Prepare(p.record, "rv", AcceptedMembers{testNodeUID: oversized}, nil); !errors.Is(err, wire.TooLarge) {
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
	members := AcceptedMembers{testNodeUID: {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:8082", RDMANICs: []wire.RDMANIC{{Device: "mlx5_0", Port: 1, NUMANode: &numa}}}}

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

	decoded, err := wire.DecodePublication(strings.NewReader(committed.encoded))
	if err != nil || len(decoded.Members) != 1 || *decoded.Members[0].RDMANICs[0].NUMANode != 1 {
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
		sequence := current.record.Sequence

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
			want := wire.Conflict
			if cursor > sequence {
				want = wire.Unavailable
			}

			if _, err := r.Publications.Wait(context.Background(), identity, &cursor); !errors.Is(err, want) {
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

		if _, _, err := current.writeContext(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("write after leadership: %v", err)
		}
	})
}

func TestPollImmediateReturnsWithoutAllocations(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()
	previous := reconcileTopology(t, r, ctx).record.Sequence
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
				sequence := p.record.Sequence
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
		r.Publications.maxAge = 2 * wire.PollWait
		p := reconcileTopology(t, r, context.Background())
		identity := pollIdentity(r.Config, testNodeUID)
		sequence := p.record.Sequence
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
		sequence := p.record.Sequence
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
		sequence := current.record.Sequence

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

	p := publicationResponse{encoded: strings.Repeat("x", 100_000)}
	if n, err := p.writeTo(ctx, shortPublicationWriter{}); !errors.Is(err, io.ErrShortWrite) || n != 1 {
		t.Fatalf("short write: %d, %v", n, err)
	}

	w := &boundedWriter{}
	if n, err := p.writeTo(ctx, w); err != nil || n != 100_000 || w.largest > 32*1024 || w.calls != 4 {
		t.Fatalf("unbounded write scratch: %d, %v, %+v", n, err, w)
	}

	w = &boundedWriter{cancel: cancel}
	if n, err := p.writeTo(ctx, w); !errors.Is(err, context.Canceled) || n != 32*1024 || w.calls != 1 {
		t.Fatalf("write ignored cancellation: %d, %v", n, err)
	}
}

type shortPublicationWriter struct{}

func (shortPublicationWriter) Write([]byte) (int, error) { return 1, nil }

func TestPublicationAdmissionRequiresOwner(t *testing.T) {
	for _, image := range []*CommittedPublication{nil, {}, {leadership: t.Context()}} {
		if _, _, err := image.writeContext(t.Context()); !errors.Is(err, wire.Unavailable) {
			t.Fatalf("ownerless write admitted: %v", err)
		}
	}
}

func TestPublicationInstalledStateNeverExceedsHighWater(t *testing.T) {
	for _, mutation := range []string{"cluster", "sequence", "membership", "same-sequence hash"} {
		t.Run(mutation, func(t *testing.T) {
			r := initializedTopology(t)

			image := reconcileTopology(t, r, t.Context())
			if r.Publications.observed != image.record {
				t.Fatal("install did not observe record")
			}

			newer := image.record
			newer.Sequence += 2

			newer.MembershipVersion++
			if err := r.Publications.confirm(newer); err != nil {
				t.Fatal(err)
			}

			next := *image
			next.record = newer

			switch mutation {
			case "cluster":
				next.record.Cluster = testNodeUID
			case "sequence":
				next.record.Sequence--
			case "membership":
				next.record.MembershipVersion--
			case "same-sequence hash":
				next.record.ContentHash = strings.Repeat("a", 64)
			}

			if err := r.Publications.Install(&next); !errors.Is(err, wire.Conflict) {
				t.Fatalf("high-water bypass: %v", err)
			}

			if r.Publications.current != image || r.Publications.observed != newer {
				t.Fatal("rejected install mutated state")
			}
		})
	}
}
