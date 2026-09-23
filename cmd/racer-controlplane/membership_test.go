// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	eventhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pb "github.com/Azure/unbounded/api/racer"
	"github.com/Azure/unbounded/internal/racer"
)

func TestSiteDefaultEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]string
		active bool
	}{
		{"canonical", map[string]string{racer.SiteLabelKey: "default"}, true},
		{"deprecated", map[string]string{racer.DeprecatedSiteLabelKey: "default"}, true},
		{"canonical wins conflict", map[string]string{racer.SiteLabelKey: "default", racer.DeprecatedSiteLabelKey: "other"}, true},
		{"canonical empty", map[string]string{racer.SiteLabelKey: "", racer.DeprecatedSiteLabelKey: "default"}, false},
		{"no Site", nil, false},
		{"universe label is not Site", map[string]string{racer.UniverseKey: "default"}, false},
		{"excluded", map[string]string{racer.SiteLabelKey: "default", racer.ExcludeLabelKey: "true"}, false},
		{"exact exclusion only", map[string]string{racer.SiteLabelKey: "default", racer.ExcludeLabelKey: "True"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, p, s := fixtures()

			n.Labels = tc.labels
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}

			n.Labels[corev1.LabelOSStable] = "linux"
			n.Annotations = map[string]string{universeAnnotation: "default"}

			g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, s)
			if err != nil {
				t.Fatal(err)
			}

			if got := len(g.Owners) != 0; got != tc.active {
				t.Fatalf("active=%v, want %v: %+v", got, tc.active, g)
			}
		})
	}
}

func TestSiteExclusionReenrollmentAndReassignment(t *testing.T) {
	n, p, s := fixtures()
	p.UID = "old-process"
	build := func(name string, previous *generation, pods ...corev1.Pod) *generation {
		t.Helper()

		g, _, err := buildCacheFixture(name, previous, []corev1.Node{*n}, pods, s)
		if err != nil {
			t.Fatal(err)
		}

		g.Revision++

		return g
	}

	initial := build("default", nil, *p)
	if initial.Nodes[n.Name].PodUID != string(p.UID) || len(initial.Owners) == 0 {
		t.Fatal("initial enrollment missing")
	}

	n.Labels[racer.ExcludeLabelKey] = "true"
	// Bad fabric and an unowned terminating Pod must not poison exclusion.
	n.Annotations = map[string]string{racer.FabricAnnotationKey: "invalid fabric"}
	p.DeletionTimestamp = new(metav1.Now())
	p.OwnerReferences = nil

	excluded := build("default", initial, *p)
	if m := excluded.Nodes[n.Name]; m.IP != "" || m.PodUID != string(p.UID) || len(excluded.Owners) != 0 {
		t.Fatalf("exclusion lost removal authority: %+v", m)
	}

	index, err := indexGeneration(excluded)
	if err != nil {
		t.Fatal(err)
	}

	if snap := index.snapshot(initial.Nodes[n.Name].ID); snap == nil || len(snap.Volumes) != 0 || len(snap.Peers) != 0 {
		t.Fatal("excluded recipient must receive an empty snapshot")
	}

	delete(n.Labels, racer.ExcludeLabelKey)
	n.Annotations = nil
	_, p, _ = fixtures()
	p.UID = "replacement-process"

	reenrolled := build("default", excluded, *p)
	if m := reenrolled.Nodes[n.Name]; m.IP == "" || m.PodUID != string(p.UID) || m.ID != initial.Nodes[n.Name].ID {
		t.Fatalf("reenrollment did not retain Node identity: %+v", m)
	}

	n.Labels[racer.SiteLabelKey] = "other"

	old := build("default", reenrolled, *p)
	if m := old.Nodes[n.Name]; m.IP != "" || m.PodUID != string(p.UID) {
		t.Fatalf("Site reassignment lost old-universe drain: %+v", m)
	}

	next := build("other", nil, *p)
	if m := next.Nodes[n.Name]; m.IP != "" || m.PodUID != "" {
		t.Fatalf("old Pod authorized into new universe: %+v", m)
	}

	p = p.DeepCopy()
	p.Labels[universeAnnotation] = "other"
	p.UID = "new-site-process"

	next = build("other", next, *p)
	if next.Nodes[n.Name].PodUID != string(p.UID) || len(next.Owners) == 0 {
		t.Fatal("replacement in new Site did not enroll")
	}
}

func TestSiteRemovalAuthorityRequiresActualDeletion(t *testing.T) {
	ctx := context.Background()
	n, p, s := fixtures()
	p.UID = "live-pod"
	direct := fakeKube(n, p, s)
	r := newTestReconciler(direct)

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	// Finish the initial rollout before changing membership.
	g := r.loaded["default"]

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	roll, err := r.server.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.server.persistPhase(ctx, "default", roll, 4); err != nil {
		t.Fatal(err)
	}

	n.Labels[racer.ExcludeLabelKey] = "true"
	if err := direct.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	// Simulate selector/cache disappearance while the direct API retains the Pod.
	r.client = fakeKube(n, s)
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	if m := r.loaded["default"].Nodes[n.Name]; m.PodUID != string(p.UID) || m.IP != "" {
		t.Fatalf("cached absence revoked removal authority: %+v", m)
	}

	if err := direct.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	if r.loaded["default"].Nodes[n.Name].PodUID != "" {
		t.Fatal("actual deletion did not revoke authority")
	}
}

func TestSiteEventMappings(t *testing.T) {
	ctx := context.Background()
	n, p, _ := fixtures()
	old := n.DeepCopy()
	n.Labels[racer.SiteLabelKey] = "other"

	r := newTestReconciler(fakeKube(n))
	if !nodeChanged(old, n) {
		t.Fatal("Site move was filtered")
	}

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	mapping := eventhandler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		return universeRequests(objectUniverses(o)...)
	})
	mapping.Update(ctx, event.UpdateEvent{ObjectOld: old, ObjectNew: n}, queue)

	if queue.Len() != 2 {
		t.Fatalf("move queued %d universes, want both", queue.Len())
	}

	if got := r.podRequests(ctx, p); !reflect.DeepEqual(got, universeRequests("default", "other")) {
		t.Fatalf("Pod mapping lost old/new universes: %v", got)
	}

	if err := r.client.Delete(ctx, n); err != nil {
		t.Fatal(err)
	}

	if got := r.podRequests(ctx, p); !reflect.DeepEqual(got, universeRequests("default")) {
		t.Fatalf("deleted Node lost Pod universe: %v", got)
	}

	excluded := old.DeepCopy()

	excluded.Labels[racer.ExcludeLabelKey] = "true"
	if !nodeChanged(old, excluded) || !reflect.DeepEqual(objectUniverses(excluded), []string{"default"}) {
		t.Fatal("exclusion must reconcile and retain the Site index")
	}
}

func TestSiteDepartureDeliversV2RemovalUntilDeletion(t *testing.T) {
	for _, mode := range []string{"exclude", "unassign", "reassign"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()

			f := newCoordinationFixture(t, nil)
			for phase := uint32(0); phase <= 4; phase++ {
				f.call(t, phase, 200)
			}

			var node corev1.Node
			if err := f.api.Get(ctx, client.ObjectKey{Name: "node"}, &node); err != nil {
				t.Fatal(err)
			}

			switch mode {
			case "exclude":
				node.Labels[racer.ExcludeLabelKey] = "true"
			case "unassign":
				delete(node.Labels, racer.SiteLabelKey)
			case "reassign":
				node.Labels[racer.SiteLabelKey] = "other"
			}

			if err := f.api.Update(ctx, &node); err != nil {
				t.Fatal(err)
			}

			r := newTestReconciler(f.api)
			r.server = f.s

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}

			command := f.call(t, 4, 200)
			if command.Revision != 2 || command.Configuration == nil {
				t.Fatalf("missing removal command: %v", command)
			}

			var snapshot pb.Snapshot
			if err := proto.Unmarshal(command.Configuration.GetSigned().Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}

			if len(snapshot.Volumes) != 0 || len(snapshot.Peers) != 0 {
				t.Fatal("departing process received active membership")
			}

			// A controller restart must recover the historical authorization.
			f.s = &Server{controlStore: f.s.controlStore, signer: f.s.signer}
			r = newTestReconciler(f.api)

			r.server = f.s
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}

			for phase := command.Phase; phase <= 4; phase++ {
				f.call(t, phase, 200)
			}

			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod"}}
			if err := f.api.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}

			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}

			f.call(t, 4, 403)
		})
	}
}

func TestNodeUIDReplacementCannotAdoptHistoricalPod(t *testing.T) {
	n, p, s := fixtures()
	p.UID = "old-pod"

	g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, s)
	if err != nil {
		t.Fatal(err)
	}

	n.UID = "replacement-node"

	next, _, err := buildCacheFixture("default", g, []corev1.Node{*n}, []corev1.Pod{*p}, s)
	if err != nil {
		t.Fatal(err)
	}

	if m := next.Nodes[n.Name]; m.PodUID != "" || m.IP != "" {
		t.Fatalf("replacement Node adopted old Pod identity: %+v", m)
	}

	if m := next.Nodes["deleted/"+g.Nodes[n.Name].ID]; m.PodUID != string(p.UID) || m.IP != "" {
		t.Fatalf("old Node lost removal authority: %+v", m)
	}
}
