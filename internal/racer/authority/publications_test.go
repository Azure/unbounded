// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
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

	cache, err := BuildCatalog([]racerv1.ClusterVolume{catalogVolume("cache", testNodeUID)})
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
	volume := catalogVolume("cache", testNodeUID)

	if err := r.Create(ctx, &volume); err != nil {
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

		volume := catalogVolume("cache", testNodeUID)
		if err := r.Create(ctx, &volume); err != nil {
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

func TestTopologyReplayRejectedBeforeMutation(t *testing.T) {
	for _, replay := range []string{"rollback", "conflicting sequence"} {
		for _, suspended := range []bool{false, true} {
			for _, operation := range []string{"publish", "commit"} {
				t.Run(fmt.Sprintf("%s/suspended=%t/%s", replay, suspended, operation), func(t *testing.T) {
					r := initializedTopology(t)
					first := reconcileTopology(t, r, t.Context())

					cm, _, err := readVersion(t.Context(), r.APIReader, r.Config)
					if err != nil {
						t.Fatal(err)
					}

					record := first.record
					record.Sequence = 10

					cm.Data = versionData(record)
					if err := r.Update(t.Context(), cm); err != nil {
						t.Fatal(err)
					}

					reconcileTopology(t, r, t.Context())

					if err := r.Get(t.Context(), client.ObjectKeyFromObject(cm), cm); err != nil {
						t.Fatal(err)
					}

					replayed := record
					if replay == "rollback" {
						replayed.Sequence--
					} else {
						replayed.ContentHash = strings.Repeat("a", 64)
					}

					cm.Data = versionData(replayed)
					if err := r.Update(t.Context(), cm); err != nil {
						t.Fatal(err)
					}

					before := cm.DeepCopy()

					if suspended {
						if err := r.Publications.confirm(replayed); !errors.Is(err, wire.Conflict) {
							t.Fatalf("replay was not rejected: %v", err)
						}
					}

					proposed := AcceptedMembers{testNodeUID: {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:8082"}}

					candidate, err := r.Publications.Prepare(replayed, cm.ResourceVersion, proposed, nil)
					if err != nil {
						t.Fatal(err)
					}

					writes := 0

					r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						writes++
						return c.Update(ctx, obj, opts...)
					}})
					if operation == "commit" {
						_, err = r.CommitVersion(t.Context(), candidate)
					} else {
						r.authority.publisher = r.engine()
						_, err = r.authority.PublishTopology(t.Context(), func(context.Context) (TopologyObservation, error) {
							node, pod := memberNode(), memberPod("pod", 1, "192.0.2.1")
							return TopologyObservation{Input: members.Input{Nodes: []corev1.Node{node}, PodsByNode: map[string][]corev1.Pod{node.Name: {pod}}, PeerPort: 8082, Ownership: members.WorkloadIdentities{Namespace: r.Config.Namespace, Workloads: [2]members.WorkloadIdentity{{Name: r.Config.DaemonSetName, UID: testDaemonSetUID}}}}}, nil
						})
					}

					if !errors.Is(err, wire.Conflict) || writes != 0 {
						t.Fatalf("replay must fail before mutation: err=%v writes=%d", err, writes)
					}

					if err := r.APIReader.Get(t.Context(), client.ObjectKeyFromObject(cm), cm); err != nil {
						t.Fatal(err)
					}

					if !reflect.DeepEqual(before.Data, cm.Data) || before.ResourceVersion != cm.ResourceVersion {
						t.Fatal("replay changed durable counters")
					}

					if r.Publications.observed != record || !r.Publications.suspended {
						t.Fatal("replay changed high-water state or retained serving authority")
					}
				})
			}
		}
	}
}

func TestTopologyReconcileAcceptsNewerDurableVersion(t *testing.T) {
	r := initializedTopology(t)
	reconcileTopology(t, r, t.Context())
	lagged := Assemble(r.Config, r.Client, r.APIReader).Topology
	reconcileTopology(t, lagged, t.Context())

	cm, record, err := readVersion(t.Context(), r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	record.Sequence += 2

	cm.Data = versionData(record)
	if err := r.Update(t.Context(), cm); err != nil {
		t.Fatal(err)
	}

	current := reconcileTopology(t, lagged, t.Context())
	if current.record != record || lagged.Publications.observed != record {
		t.Fatal("lagged replica failed to accept newer durable counters")
	}
}

func TestTopologyRejectsCredentialReplay(t *testing.T) {
	for _, replay := range []string{"rollback", "conflicting generation"} {
		for _, invalidated := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/invalidated=%t", replay, invalidated), func(t *testing.T) {
				r, _ := testKeyring(t)
				runKeys(t, r)
				original, oldBundle, _, _ := keyState(t, r)

				volume := catalogVolume("added", testOtherUID)
				if err := r.Create(t.Context(), &volume); err != nil {
					t.Fatal(err)
				}

				runKeys(t, r)
				a := Assemble(r.Config, r.Client, r.APIReader)
				runKeys(t, a.Keyring)
				image := reconcileTopology(t, a.Topology, t.Context())

				decoded, err := wire.DecodePublication(strings.NewReader(image.encoded))
				if err != nil || len(decoded.Caches) != 2 {
					t.Fatalf("fixture did not admit both caches: %v", err)
				}

				before, record, err := readVersion(t.Context(), r.APIReader, r.Config)
				if err != nil {
					t.Fatal(err)
				}

				trust := a.Server.Trust
				highWater, digest := trust.highWater, trust.digest

				guard, cancel, err := a.authority.TrustContext(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer cancel()

				secret, current, _, _ := keyState(t, r)
				secret.Data = original.DeepCopy().Data

				if replay == "conflicting generation" {
					oldBundle.Generation = current.Generation

					secret.Data["bundle.json"], err = wire.EncodeBundle(oldBundle)
					if err != nil {
						t.Fatal(err)
					}
				}

				if err := r.Update(t.Context(), secret); err != nil {
					t.Fatal(err)
				}

				if invalidated {
					if err := a.authority.Observe(t.Context()); !errors.Is(err, wire.Conflict) {
						t.Fatalf("observer did not reject replay: %v", err)
					}
				}

				writes := 0

				a.Topology.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes++
					return c.Update(ctx, obj, opts...)
				}})
				for range 2 {
					_, err := a.Topology.Reconcile(t.Context(), ctrl.Request{})
					if !errors.Is(err, wire.Conflict) || writes != 0 {
						t.Errorf("credential replay reached publication: err=%v writes=%d", err, writes)
					}
				}

				after, gotRecord, err := readVersion(t.Context(), r.APIReader, r.Config)
				if err != nil {
					t.Fatal(err)
				}

				if before.ResourceVersion != after.ResourceVersion || gotRecord != record || a.Topology.Publications.current != image {
					t.Error("replayed bundle changed committed catalog or counters")
				}

				if trustReady(trust) || trust.highWater != highWater || trust.digest != digest || !errors.Is(guard.Err(), context.Canceled) || !a.Topology.Publications.suspended {
					t.Error("replay did not revoke serving authority and retain replay protection")
				}
			})
		}
	}
}

func TestTopologyAcceptsNewerCredentialGeneration(t *testing.T) {
	for _, invalidated := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidated), func(t *testing.T) {
			r, _ := testKeyring(t)
			runKeys(t, r)
			a := Assemble(r.Config, r.Client, r.APIReader)
			runKeys(t, a.Keyring)
			reconcileTopology(t, a.Topology, t.Context())
			trust := a.Server.Trust
			highWater, digest, confirmed := trust.highWater, trust.digest, trust.confirmed

			volume := catalogVolume("added", testOtherUID)
			if err := r.Create(t.Context(), &volume); err != nil {
				t.Fatal(err)
			}

			runKeys(t, r)

			if r.Trust.highWater <= highWater {
				t.Fatal("fixture did not advance durable credentials")
			}

			if invalidated {
				trust.invalidate()
			}

			accepted, roots := trust.bundle, trust.roots
			image := reconcileTopology(t, a.Topology, t.Context())

			decoded, err := wire.DecodePublication(strings.NewReader(image.encoded))
			if err != nil || len(decoded.Caches) != 2 {
				t.Fatalf("newer credentials did not admit new cache: %v", err)
			}

			if trust.highWater != highWater || trust.digest != digest || trust.confirmed != confirmed || trust.bundle != accepted || trust.roots != roots || trustReady(trust) == invalidated {
				t.Fatal("topology installed or refreshed trust")
			}
		})
	}
}

func TestPrepareCanonicalEquivalence(t *testing.T) {
	numa := uint32(3)
	members := AcceptedMembers{
		testNodeUID:  {Node: testNodeUID, Shares: 4, PeerEndpoint: "192.0.2.1:7443", RDMANICs: []wire.RDMANIC{{Rail: 2, Device: "β<&>", Port: 1, NUMANode: &numa}, {Rail: 1, Device: "mlx5_0", Port: 1}}},
		testOtherUID: {Node: testOtherUID, Shares: 1, PeerEndpoint: "[2001:db8::1]:7443"},
	}
	caches := []wire.CacheDefinition{{ID: testNodeUID, Name: "cache", ClientSocket: "/run/racer/cache/client/socket", OriginSocket: "/run/racer/cache/origin/socket"}}
	v := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: testNodeUID}

	content, membership, err := wire.ContentHashes(v)
	if err != nil {
		t.Fatal(err)
	}

	previous := VersionRecord{Cluster: v.Cluster, Sequence: 1, MembershipVersion: 1, ContentHash: content, MembershipHash: membership}
	p := NewPublications()

	for _, tc := range []struct {
		name       string
		members    AcceptedMembers
		caches     []wire.CacheDefinition
		sequence   wire.Sequence
		membership wire.MembershipVersion
	}{
		{"unchanged empty", nil, nil, 1, 1},
		{"cache only", nil, caches, 2, 1},
		{"membership", members, caches, 3, 2},
		{"unchanged populated", members, caches, 3, 2},
		{"remove cache", members, nil, 4, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := p.Prepare(previous, "rv", tc.members, tc.caches)
			if err != nil {
				t.Fatal(err)
			}

			v.Sequence, v.MembershipVersion, v.Caches = tc.sequence, tc.membership, tc.caches

			v.Members = nil
			for _, member := range tc.members {
				v.Members = append(v.Members, member)
			}

			want, err := wire.EncodePublication(v)
			if err != nil {
				t.Fatal(err)
			}

			content, membership, err := wire.ContentHashes(v)
			if err != nil {
				t.Fatal(err)
			}

			wantRecord := VersionRecord{Cluster: v.Cluster, Sequence: tc.sequence, MembershipVersion: tc.membership, ContentHash: content, MembershipHash: membership}
			if prepared.record != wantRecord || prepared.encoded != string(want) || prepared.previous != previous || prepared.resourceVersion != "rv" || prepared.owner != p {
				t.Fatal("prepared bytes, hashes, counters, or commit metadata differ")
			}

			previous = prepared.record
		})
	}
}

func TestPrepareRejectsFinalCounterGrowth(t *testing.T) {
	v := wire.Publication{
		SchemaVersion: wire.SchemaVersion, Cluster: testNodeUID, Sequence: 9, MembershipVersion: 9,
		Members: []wire.Member{{Node: testNodeUID, Shares: 1, PeerEndpoint: "192.0.2.1:1", RDMANICs: []wire.RDMANIC{{Device: "x", Port: 1}}}},
	}

	b, err := wire.EncodePublication(v)
	if err != nil {
		t.Fatal(err)
	}

	v.Members[0].RDMANICs[0].Device += strings.Repeat("x", wire.MaxPublicationBytes-len(b))

	content, membership, err := wire.ContentHashes(v)
	if err != nil {
		t.Fatal(err)
	}

	previous := VersionRecord{Cluster: v.Cluster, Sequence: 9, MembershipVersion: 9, ContentHash: content, MembershipHash: membership}
	members := AcceptedMembers{testNodeUID: v.Members[0]}
	p := NewPublications()

	prepared, err := p.Prepare(previous, "rv", members, nil)
	if err != nil || len(prepared.encoded) != wire.MaxPublicationBytes {
		t.Fatalf("exact final bound: %v", err)
	}

	// Same-width input change grows both assigned counters from 9 to 10. The
	// counter-free hash still fits, but the final publication must be rejected.
	m := members[testNodeUID]
	m.Shares++

	members[testNodeUID] = m
	if prepared, err := p.Prepare(previous, "rv", members, nil); !errors.Is(err, wire.TooLarge) || prepared != nil {
		t.Fatalf("oversized final encoding accepted: %v", err)
	}
}

func stagedTopology(t *testing.T) *TopologyReconciler {
	t.Helper()
	r := testTopology(t)

	marker, err := readInstallation(t.Context(), r.APIReader, r.Config, true)
	if err != nil {
		t.Fatal(err)
	}

	marker.Data[markerInitializationProtocol] = stagedInitialization
	if err := r.Update(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	// The fake client does not assign server UIDs. Model the API's identity and
	// immutable ConfigMap data rules, including metadata updates remaining legal.
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			obj.SetUID(types.UID(uuid.NewString()))
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				old := &corev1.ConfigMap{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(cm), old); err != nil {
					return err
				}

				if old.Immutable != nil && *old.Immutable && (!reflect.DeepEqual(old.Data, cm.Data) || cm.Immutable == nil || !*cm.Immutable) {
					return errors.New("immutable data changed")
				}
			}

			return c.Update(ctx, obj, opts...)
		},
	})

	return r
}

// Each write boundary is tested both before persistence and with an uncertain
// successful response, including cancellation immediately after persistence.
func interruptInitialization(base client.WithWatch, boundary string, cancel context.CancelFunc) client.WithWatch {
	boom := errors.New("interrupted initialization")

	return interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if boundary == "before create" {
				return boom
			}

			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}

			if boundary == "after create" {
				return boom
			}

			if boundary == "cancel create" {
				cancel()
			}

			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if boundary == "before commit" {
				return boom
			}

			if err := c.Update(ctx, obj, opts...); err != nil {
				return err
			}

			if boundary == "after commit" {
				return boom
			}

			if boundary == "cancel commit" {
				cancel()
			}

			return nil
		},
	})
}

func TestStagedInitializationEveryBoundary(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		for _, boundary := range []string{"before create", "after create", "cancel create", "before commit", "after commit", "cancel commit"} {
			t.Run(fmt.Sprintf("credentials=%t/%s", credentials, boundary), func(t *testing.T) {
				r := stagedTopology(t)

				base := r.Client.(client.WithWatch)
				if credentials {
					if err := ensureInstalled(t.Context(), base, r.APIReader, r.Config); err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				writer := interruptInitialization(base, boundary, cancel)

				run := func(ctx context.Context, c client.WithWatch) error {
					if !credentials {
						return ensureInstalled(ctx, c, r.APIReader, r.Config)
					}

					_, err := Assemble(r.Config, c, r.APIReader).Keyring.Reconcile(ctx, ctrl.Request{})

					return err
				}
				if err := run(ctx, writer); err == nil {
					t.Fatal("interruption not observed")
				}

				var before client.Object = &corev1.ConfigMap{}

				name := r.Config.VersionConfigMapName
				if credentials {
					before, name = &corev1.Secret{}, r.Config.CredentialsSecretName
				}

				err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, before)
				if err != nil && !apierrors.IsNotFound(err) {
					t.Fatal(err)
				}

				if boundary != "after commit" && boundary != "cancel commit" {
					if credentials {
						if _, err := loadSigning(t.Context(), base, r.Config, Assemble(r.Config, base, base).Keyring.now()); err == nil {
							t.Fatal("uncommitted credentials usable")
						}
					} else if _, _, err := readVersion(t.Context(), base, r.Config); err == nil {
						t.Fatal("uncommitted version usable")
					}
				}

				if err := run(t.Context(), base); err != nil {
					t.Fatalf("restart: %v", err)
				}

				after := before.DeepCopyObject().(client.Object)
				if err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, after); err != nil {
					t.Fatal(err)
				}

				if before.GetUID() != "" && before.GetUID() != after.GetUID() {
					t.Fatal("recovery replaced staged material")
				}

				if secret, ok := before.(*corev1.Secret); ok && secret.UID != "" && !reflect.DeepEqual(secret.Data, after.(*corev1.Secret).Data) {
					t.Fatal("recovery changed exact secret material")
				}
			})
		}
	}
}

func TestStagedCommittedDeletionAndReplacementFailClosed(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			t.Run(fmt.Sprintf("credentials=%t/replace=%t", credentials, replace), func(t *testing.T) {
				r := stagedTopology(t)

				base := r.Client.(client.WithWatch)
				if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
					t.Fatal(err)
				}

				runKeys(t, Assemble(r.Config, base, base).Keyring)

				var obj client.Object = &corev1.ConfigMap{}

				name := r.Config.VersionConfigMapName
				if credentials {
					obj, name = &corev1.Secret{}, r.Config.CredentialsSecretName
				}

				if err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, obj); err != nil {
					t.Fatal(err)
				}

				if err := base.Delete(t.Context(), obj); err != nil {
					t.Fatal(err)
				}

				if replace {
					obj.SetResourceVersion("")

					if err := base.Create(t.Context(), obj); err != nil {
						t.Fatal(err)
					}
				}

				noWrites := interceptor.NewClient(base, interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						t.Fatal("recreated committed state")
						return nil
					},
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						t.Fatal("rewrote committed state")
						return nil
					},
				})
				if credentials {
					if _, err := Assemble(r.Config, noWrites, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err == nil {
						t.Fatal("lost credentials accepted")
					}
				} else if err := ensureInstalled(t.Context(), noWrites, base, r.Config); err == nil {
					t.Fatal("lost version accepted")
				}
			})
		}
	}
}

func TestStagedCompetingInstallers(t *testing.T) {
	r := stagedTopology(t)
	base := r.Client.(client.WithWatch)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	for range 8 {
		wg.Go(func() {
			if _, err := Assemble(r.Config, base, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()
	runKeys(t, Assemble(r.Config, base, base).Keyring)
}

func TestStagedDelayedCreateCannotResurrectAuthority(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		t.Run(fmt.Sprint(credentials), func(t *testing.T) {
			r := stagedTopology(t)

			base := r.Client.(client.WithWatch)
			if credentials {
				if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
					t.Fatal(err)
				}
			}

			writer := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				// Let a competitor fully commit, then lose its resource, while this
				// installer is paused after its NotFound but before its Create.
				if credentials {
					runKeys(t, Assemble(r.Config, base, base).Keyring)
				} else if err := ensureInstalled(ctx, base, base, r.Config); err != nil {
					t.Fatal(err)
				}

				if err := base.Delete(ctx, obj); err != nil {
					t.Fatal(err)
				}

				return c.Create(ctx, obj, opts...)
			}})
			if credentials {
				_, _ = Assemble(r.Config, writer, base).Keyring.Reconcile(t.Context(), ctrl.Request{})
				if _, err := Assemble(r.Config, base, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err == nil {
					t.Fatal("delayed Create restored credentials authority")
				}
			} else {
				if err := ensureInstalled(t.Context(), writer, base, r.Config); err == nil {
					t.Fatal("delayed Create restored version authority")
				}

				if _, _, err := readVersion(t.Context(), base, r.Config); err == nil {
					t.Fatal("orphan version accepted")
				}
			}
		})
	}
}

func TestStagedRejectsUnboundOrCorruptCandidates(t *testing.T) {
	for _, credentials := range []bool{false, true} {
		for _, corruption := range []string{"binding", "protocol", "immutable", "data"} {
			t.Run(fmt.Sprintf("credentials=%t/%s", credentials, corruption), func(t *testing.T) {
				r := stagedTopology(t)

				base := r.Client.(client.WithWatch)
				if credentials {
					if err := ensureInstalled(t.Context(), base, base, r.Config); err != nil {
						t.Fatal(err)
					}
				}

				writer := interruptInitialization(base, "after create", func() {})

				var obj client.Object = &corev1.ConfigMap{}

				name := r.Config.VersionConfigMapName
				if credentials {
					_, _ = Assemble(r.Config, writer, base).Keyring.Reconcile(t.Context(), ctrl.Request{})
					obj, name = &corev1.Secret{}, r.Config.CredentialsSecretName
				} else {
					_ = ensureInstalled(t.Context(), writer, base, r.Config)
				}

				if err := base.Get(t.Context(), client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, obj); err != nil {
					t.Fatal(err)
				}

				switch corruption {
				case "binding":
					obj.GetAnnotations()[installationUIDAnnotation] = "foreign"
				case "protocol":
					delete(obj.GetAnnotations(), initializationProtocol)
				case "immutable":
					immutable := true
					if credentials {
						obj.(*corev1.Secret).Immutable = &immutable
					} else {
						obj.(*corev1.ConfigMap).Immutable = &immutable
					}
				case "data":
					if credentials {
						delete(obj.(*corev1.Secret).Data, "issuer.json")
					} else {
						obj.(*corev1.ConfigMap).Data["sequence"] = "2"
					}
				}

				if err := base.Update(t.Context(), obj); err != nil {
					t.Fatal(err)
				}

				if credentials {
					if _, err := Assemble(r.Config, base, base).Keyring.Reconcile(t.Context(), ctrl.Request{}); err == nil {
						t.Fatal("invalid candidate committed")
					}

					version, _, err := readVersion(t.Context(), base, r.Config)
					if err != nil || version.Annotations[credentialClaim] != "" {
						t.Fatal("invalid candidate consumed claim")
					}
				} else {
					if err := ensureInstalled(t.Context(), base, base, r.Config); err == nil {
						t.Fatal("invalid candidate committed")
					}

					if _, err := readInstallation(t.Context(), base, r.Config, true); err != nil {
						t.Fatal("invalid candidate consumed marker")
					}
				}
			})
		}
	}
}

func integrationStagedInitialization(t *testing.T, c client.WithWatch) {
	for _, credentials := range []bool{false, true} {
		for i, boundary := range []string{"before create", "after create", "cancel create", "before commit", "after commit", "cancel commit"} {
			t.Run(fmt.Sprintf("credentials=%t/%s", credentials, boundary), func(t *testing.T) {
				a := integrationInstallation(t, c, fmt.Sprintf("staged-%t-%d", credentials, i))
				cfg := a.Topology.Config

				marker, err := readInstallation(t.Context(), c, cfg, true)
				if err != nil {
					t.Fatal(err)
				}

				marker.Data[markerInitializationProtocol] = stagedInitialization
				if err := c.Update(t.Context(), marker); err != nil {
					t.Fatal(err)
				}

				if credentials {
					if err := ensureInstalled(t.Context(), c, c, cfg); err != nil {
						t.Fatal(err)
					}
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				writer := interruptInitialization(c, boundary, cancel)
				if credentials {
					if _, err := Assemble(cfg, writer, c).Keyring.Reconcile(ctx, ctrl.Request{}); err == nil {
						t.Fatal("interruption not injected")
					}

					runKeys(t, Assemble(cfg, c, c).Keyring)
				} else {
					if err := ensureInstalled(ctx, writer, c, cfg); err == nil {
						t.Fatal("interruption not injected")
					}

					if err := ensureInstalled(t.Context(), c, c, cfg); err != nil {
						t.Fatal(err)
					}
				}

				marker, err = readInstallation(t.Context(), c, cfg, false)
				if err != nil {
					t.Fatal(err)
				}

				marker.Data[versionUID] = "replacement"
				if err := c.Update(t.Context(), marker); err == nil {
					t.Fatal("API allowed rewriting immutable binding")
				}
			})
		}
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("RACER_CLUSTER_ID", testOtherUID)
	t.Setenv("POD_NAMESPACE", "racer")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func testTopology(t *testing.T, objects ...client.Object) *TopologyReconciler {
	t.Helper()
	cfg := testConfig(t)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, racerv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation-uid"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh"}}
	objects = append(objects, marker)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithIndex(&corev1.Pod{}, podNodeIndex, podNodeKeys).Build()

	return Assemble(cfg, c, c).Topology
}

func initializedTopology(t *testing.T, objects ...client.Object) *TopologyReconciler {
	t.Helper()

	r := testTopology(t, objects...)
	if err := ensureInstalled(context.Background(), r.Client, r.APIReader, r.Config); err != nil {
		t.Fatal(err)
	}

	return r
}

func reconcileTopology(t *testing.T, r *TopologyReconciler, ctx context.Context) *CommittedPublication {
	t.Helper()

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter != 0 {
		t.Fatalf("reconcile: %v, %v", result, err)
	}

	p, err := r.Publications.Current()
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestInitializeCrashOrdering(t *testing.T) {
	for _, stage := range []string{"before marker", "marker response lost", "after marker", "create response lost", "success"} {
		t.Run(stage, func(t *testing.T) {
			r := testTopology(t)
			base := r.Client.(client.WithWatch)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			writes := []string{}
			boom := errors.New("simulated crash")
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					writes = append(writes, "consume")

					if stage == "before marker" {
						return boom
					}

					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}

					if stage == "marker response lost" {
						return boom
					}

					if stage == "after marker" {
						cancel()
					}

					return nil
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes = append(writes, "create")

					marker, err := readInstallation(ctx, r.APIReader, r.Config, false)
					if err != nil || marker.Immutable == nil || !*marker.Immutable {
						t.Fatalf("counter create before marker freeze: %v", err)
					}

					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}

					if stage == "create response lost" {
						return boom
					}

					return nil
				},
			})

			err := ensureInstalled(ctx, r.Client, r.APIReader, r.Config)
			if (err == nil) != (stage == "success") {
				t.Fatalf("initialize: %v", err)
			}

			wantWrites := []string{"consume"}
			if stage == "create response lost" || stage == "success" {
				wantWrites = append(wantWrites, "create")
			}

			if !reflect.DeepEqual(writes, wantWrites) {
				t.Fatalf("writes: %v", writes)
			}

			writes = nil

			if stage == "before marker" {
				r.Client = base
				if err := ensureInstalled(context.Background(), r.Client, r.APIReader, r.Config); err != nil {
					t.Fatalf("fresh marker cannot initialize: %v", err)
				}
			} else {
				restart, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
				defer stop()

				err := ensureInstalled(restart, r.Client, r.APIReader, r.Config)
				if (err == nil) != (stage == "success" || stage == "create response lost") {
					t.Fatalf("restart: %v", err)
				}

				if len(writes) != 0 {
					t.Fatalf("restart wrote state: %v", writes)
				}
			}

			_, _, err = readVersion(context.Background(), r.APIReader, r.Config)

			valid := stage == "before marker" || stage == "create response lost" || stage == "success"
			if (err == nil) != valid {
				t.Fatalf("recovery: %v", err)
			}
		})
	}
}

func TestInitializeConflictAndExistingState(t *testing.T) {
	r := testTopology(t)
	base := r.Client.(client.WithWatch)
	creates := 0

	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return apierrors.NewConflict(corev1.Resource("configmaps"), "marker", errors.New("concurrent initializer"))
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			creates++
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if err := ensureInstalled(ctx, r.Client, r.APIReader, r.Config); !errors.Is(err, context.DeadlineExceeded) || creates != 0 {
		t.Fatalf("marker conflict: %v, creates=%d", err, creates)
	}

	r.Client = base
	if err := base.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.VersionConfigMapName}}); err != nil {
		t.Fatal(err)
	}

	if err := ensureInstalled(context.Background(), r.Client, r.APIReader, r.Config); !errors.Is(err, wire.Conflict) {
		t.Fatalf("existing counters accepted: %v", err)
	}

	if _, err := readInstallation(context.Background(), r.APIReader, r.Config, true); err != nil {
		t.Fatalf("marker consumed despite existing counters: %v", err)
	}
}

func TestInitializeCanceledBeforeMarker(t *testing.T) {
	r := testTopology(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := ensureInstalled(ctx, r.Client, r.APIReader, r.Config); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled initialize: %v", err)
	}

	if _, err := readInstallation(context.Background(), r.APIReader, r.Config, true); err != nil {
		t.Fatalf("canceled initialize consumed marker: %v", err)
	}
}

func TestStalePreparedPublicationCannotCommit(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	p, err := r.Publications.Prepare(previous, cm.ResourceVersion, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Another writer changes only metadata, but even equal counters require the
	// exact read resource version. No install token can escape a stale candidate.
	cm.Labels = map[string]string{"changed": "true"}
	if err := r.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}

	if committed, err := r.CommitVersion(ctx, p); !apierrors.IsConflict(err) || committed != nil {
		t.Fatalf("stale candidate committed: %p, %v", committed, err)
	}
}

func TestTopologyNamespaceOwnershipAndMissingDaemonSet(t *testing.T) {
	node := memberNode()
	pod := memberPod("pod", 1, "192.0.2.1")
	pod.Namespace = "unrelated"
	r := initializedTopology(t, &node, &pod)
	ctx := context.Background()

	first := reconcileTopology(t, r, ctx)
	if len(r.Accepted) != 0 {
		t.Fatal("pod in unrelated namespace admitted")
	}

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName, UID: testDaemonSetUID}}
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if next := reconcileTopology(t, r, ctx); next != first {
		t.Fatal("foreign pod admitted by matching owner UID")
	}

	pod.Namespace = r.Config.Namespace

	pod.ResourceVersion = ""
	if err := r.Create(ctx, &pod); err != nil {
		t.Fatal(err)
	}

	member := reconcileTopology(t, r, ctx)
	if len(r.Accepted) != 1 {
		t.Fatal("managed endpoint not admitted")
	}

	if err := r.Delete(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if gap := reconcileTopology(t, r, ctx); gap != member {
		t.Fatal("missing workload discarded warm endpoint")
	}

	r = Assemble(r.Config, r.Client, r.APIReader).Topology
	if restarted := reconcileTopology(t, r, ctx); restarted.record != member.record || restarted.encoded != member.encoded {
		t.Fatal("cold restart changed admitted membership during workload gap")
	}

	if len(r.Accepted) != 1 {
		t.Fatal("cold restart lost persisted admitted identity")
	}
}

func TestRecoveryNeverRecreatesCounters(t *testing.T) {
	for _, mutation := range []string{"missing version", "missing marker", "corrupt", "wrong cluster", "wrong marker uid", "mutable marker", "fresh marker"} {
		t.Run(mutation, func(t *testing.T) {
			r := initializedTopology(t)
			ctx := context.Background()

			cm, _, err := readVersion(ctx, r.APIReader, r.Config)
			if err != nil {
				t.Fatal(err)
			}

			marker, err := readInstallation(ctx, r.APIReader, r.Config, false)
			if err != nil {
				t.Fatal(err)
			}

			switch mutation {
			case "missing version":
				err = r.Delete(ctx, cm)
			case "missing marker":
				err = r.Delete(ctx, marker)
			case "corrupt":
				cm.Data["sequence"] = "01"
				err = r.Update(ctx, cm)
			case "wrong cluster":
				cm.Data["cluster"] = testNodeUID
				err = r.Update(ctx, cm)
			case "wrong marker uid":
				cm.Annotations[installationUIDAnnotation] = "replacement"
				err = r.Update(ctx, cm)
			case "mutable marker":
				marker.Immutable = nil
				err = r.Update(ctx, marker)
			case "fresh marker":
				marker.Data["state"] = "fresh"
				err = r.Update(ctx, marker)
			}

			if err != nil {
				t.Fatal(err)
			}

			writes := 0

			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					writes++
					return nil
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return nil
				},
			})
			if _, err := r.Reconcile(ctx, ctrl.Request{}); err == nil || writes != 0 {
				t.Fatalf("unsafe recovery: %v, writes=%d", err, writes)
			}

			if _, err := r.Publications.Current(); err == nil {
				t.Fatal("served invalid recovery")
			}
		})
	}
}

func TestVersionCountersAndCrashAfterCommit(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()

	empty := reconcileTopology(t, r, ctx)
	if v := empty.record; v.Sequence != 1 || v.MembershipVersion != 1 {
		t.Fatalf("initial counters: %+v", v)
	}

	if same := reconcileTopology(t, r, ctx); same != empty {
		t.Fatal("unchanged install replaced shared allocation")
	}

	volume := catalogVolume("cache-a", testNodeUID)
	if err := r.Create(ctx, &volume); err != nil {
		t.Fatal(err)
	}

	runKeys(t, Assemble(r.Config, r.Client, r.APIReader).Keyring)

	catalog := reconcileTopology(t, r, ctx)
	if v := catalog.record; v.Sequence != 2 || v.MembershipVersion != 1 {
		t.Fatalf("catalog counters: %+v", v)
	}

	node, pod := memberNode(), memberPod("pod", 1, "192.0.2.1")

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: r.Config.DaemonSetName, Namespace: r.Config.Namespace, UID: testDaemonSetUID}}
	for _, obj := range []client.Object{&node, &pod, ds} {
		if err := r.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	member := reconcileTopology(t, r, ctx)
	if v := member.record; v.Sequence != 3 || v.MembershipVersion != 2 {
		t.Fatalf("member counters: %+v", v)
	}

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.CommitVersion(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	// Simulated crash before install: no candidate bytes or history were exposed.
	if current, _ := r.Publications.Current(); current != member {
		t.Fatal("commit installed prematurely")
	}

	if len(r.Accepted) != 1 {
		t.Fatal("commit replaced history")
	}

	r = Assemble(r.Config, r.Client, r.APIReader).Topology

	recovered := reconcileTopology(t, r, ctx)
	if v := recovered.record; v.Sequence != 5 || v.MembershipVersion != 4 {
		t.Fatalf("recovery reused an unserved counter: %+v", v)
	}
}

func TestCASConflictRetriesFreshInputsAndKeepsHistory(t *testing.T) {
	node, pod := memberNode(), memberPod("pod", 1, "192.0.2.1")
	r := initializedTopology(t, &node, &pod, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Namespace: "racer", UID: testDaemonSetUID}})
	ctx := context.Background()
	initial := reconcileTopology(t, r, ctx)
	base := r.Client.(client.WithWatch)
	updates := 0
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		updates++
		if updates == 1 {
			n := &corev1.Node{}
			if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, n); err != nil {
				return err
			}

			n.Annotations = map[string]string{wire.SharesAnnotation: "9"}
			if err := c.Update(ctx, n); err != nil {
				return err
			}

			return apierrors.NewConflict(corev1.Resource("configmaps"), obj.GetName(), wire.Conflict)
		}

		return c.Update(ctx, obj, opts...)
	}})

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter <= 0 || updates != 1 {
		t.Fatalf("conflict retry: %v, %v, writes=%d", result, err, updates)
	}

	if current, _ := r.Publications.Current(); current != initial || r.Accepted[testNodeUID].Shares != 4 {
		t.Fatal("failed commit changed publication/history")
	}

	next := reconcileTopology(t, r, ctx)
	if r.Accepted[testNodeUID].Shares != 9 || next.record.Sequence != initial.record.Sequence+1 {
		t.Fatal("retry reused stale inputs")
	}
}

func TestCancellationBeforeWritesAndInstall(t *testing.T) {
	for _, stage := range []string{"before reconcile", "read", "conflict", "after commit"} {
		t.Run(stage, func(t *testing.T) {
			r := initializedTopology(t)
			base := r.Client.(client.WithWatch)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			writes := 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				writes++

				if stage == "conflict" {
					cancel()
					return apierrors.NewConflict(corev1.Resource("configmaps"), obj.GetName(), wire.Conflict)
				}

				err := c.Update(ctx, obj, opts...)

				cancel()

				return err
			}})

			if stage == "before reconcile" {
				cancel()
			}

			if stage == "read" {
				r.APIReader = interceptor.NewClient(base, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)

					cancel()

					return err
				}})
			}

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if !errors.Is(err, context.Canceled) || !errors.Is(err, reconcile.TerminalError(nil)) || result.RequeueAfter != 0 {
				t.Fatalf("canceled reconcile retried: %+v, %v", result, err)
			}

			if (stage == "before reconcile" || stage == "read") && writes != 0 {
				t.Fatal("write after cancellation")
			}

			if _, err := r.Publications.Current(); err == nil {
				t.Fatal("installed after cancellation")
			}
		})
	}
	// Also cover cancellation between returning a committed token and Install.
	r := initializedTopology(t)
	ctx, cancel := context.WithCancel(context.Background())

	cm, previous, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		t.Fatal(err)
	}

	p, err := r.Publications.Prepare(previous, cm.ResourceVersion, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	committed, err := r.CommitVersion(ctx, p)
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	if err := r.Publications.Install(committed); !errors.Is(err, context.Canceled) {
		t.Fatalf("late install: %v", err)
	}
}
