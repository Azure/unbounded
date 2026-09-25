// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

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
	if err := r.InitializeVersion(context.Background()); err != nil {
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

					marker, err := r.installation(ctx, false)
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

			err := r.InitializeVersion(ctx)
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

			r.Client = base
			if stage == "before marker" {
				if err := r.InitializeVersion(context.Background()); err != nil {
					t.Fatalf("fresh marker cannot initialize: %v", err)
				}
			} else if err := r.InitializeVersion(context.Background()); err == nil {
				t.Fatal("consumed marker reused")
			}

			_, _, err = r.readVersion(context.Background())

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
	if err := r.InitializeVersion(context.Background()); !apierrors.IsConflict(err) || creates != 0 {
		t.Fatalf("marker conflict: %v, creates=%d", err, creates)
	}

	r.Client = base
	if err := base.Create(context.Background(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.Config.Namespace, Name: r.Config.VersionConfigMapName}}); err != nil {
		t.Fatal(err)
	}

	if err := r.InitializeVersion(context.Background()); !errors.Is(err, wire.Conflict) {
		t.Fatalf("existing counters accepted: %v", err)
	}

	if _, err := r.installation(context.Background(), true); err != nil {
		t.Fatalf("marker consumed despite existing counters: %v", err)
	}
}

func TestInitializeCanceledBeforeMarker(t *testing.T) {
	r := testTopology(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := r.InitializeVersion(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled initialize: %v", err)
	}

	if _, err := r.installation(context.Background(), true); err != nil {
		t.Fatalf("canceled initialize consumed marker: %v", err)
	}
}

func TestStalePreparedPublicationCannotCommit(t *testing.T) {
	r := initializedTopology(t)
	ctx := context.Background()

	cm, previous, err := r.readVersion(ctx)
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
	reconcileTopology(t, r, ctx)

	if len(r.Accepted) != 0 {
		t.Fatal("cold restart inherited endpoint without workload")
	}
}

func TestRecoveryNeverRecreatesCounters(t *testing.T) {
	for _, mutation := range []string{"missing version", "missing marker", "corrupt", "wrong cluster", "wrong marker uid", "mutable marker", "fresh marker"} {
		t.Run(mutation, func(t *testing.T) {
			r := initializedTopology(t)
			ctx := context.Background()

			cm, _, err := r.readVersion(ctx)
			if err != nil {
				t.Fatal(err)
			}

			marker, err := r.installation(ctx, false)
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
	if v := empty.Version(); v.Sequence != 1 || v.MembershipVersion != 1 {
		t.Fatalf("initial counters: %+v", v)
	}

	if same := reconcileTopology(t, r, ctx); same != empty {
		t.Fatal("unchanged install replaced shared allocation")
	}

	cache := catalogCache("cache-a", testNodeUID, nil)
	if err := r.Create(ctx, &cache); err != nil {
		t.Fatal(err)
	}

	catalog := reconcileTopology(t, r, ctx)
	if v := catalog.Version(); v.Sequence != 2 || v.MembershipVersion != 1 {
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
	if v := member.Version(); v.Sequence != 3 || v.MembershipVersion != 2 {
		t.Fatalf("member counters: %+v", v)
	}

	cm, previous, err := r.readVersion(ctx)
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
	if v := recovered.Version(); v.Sequence != 5 || v.MembershipVersion != 4 {
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
	if r.Accepted[testNodeUID].Shares != 9 || next.Version().Sequence != initial.Version().Sequence+1 {
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

	cm, previous, err := r.readVersion(ctx)
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
