// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Keep the authoritative client separate from the cache and reject every Pod
// LIST, including filtered fallbacks, to enforce identity-bounded API work.
type podIdentityClient struct {
	client.Client
	t     *testing.T
	gets  []client.ObjectKey
	fault error
}

func (c *podIdentityClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		c.t.Fatal("authoritative identity check issued a Pod LIST")
	}

	return c.Client.List(ctx, list, opts...)
}

func (c *podIdentityClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		c.gets = append(c.gets, key)
		if c.fault != nil {
			return c.fault
		}
	}

	return c.Client.Get(ctx, key, obj, opts...)
}

func TestHistoricalPodIdentityChecks(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*corev1.Pod)
		missing bool
		fault   error
		release bool
	}{
		{name: "live"},
		{name: "unavailable", mutate: func(p *corev1.Pod) { p.Status = corev1.PodStatus{} }},
		{name: "selector-excluded", mutate: func(p *corev1.Pod) { p.Labels = nil }},
		{name: "terminating", mutate: func(p *corev1.Pod) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/retain"}
		}},
		{name: "not-found", missing: true, release: true},
		{name: "uid-replaced", mutate: func(p *corev1.Pod) { p.UID = "replacement" }, release: true},
		{name: "other-namespace", mutate: func(p *corev1.Pod) { p.Namespace = "other" }, release: true},
		{name: "forbidden", fault: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", errors.New("denied"))},
		{name: "timeout", fault: apierrors.NewTimeoutError("unavailable", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, p, _ := fixtures()
			p.UID = "old-uid"
			key := client.ObjectKeyFromObject(p)
			m := member{ID: "historical-node", PodUID: string(p.UID), PodNamespace: p.Namespace, PodName: p.Name}
			g := &generation{Nodes: map[string]member{"node": m, "duplicate": m}}

			if test.mutate != nil {
				test.mutate(p)
			}

			var objects []client.Object
			if !test.missing {
				objects = append(objects, p)
			}

			direct := &podIdentityClient{Client: fakeKube(objects...), t: t, fault: test.fault}
			// The cache is empty even when the authoritative Pod is still live.
			r := newTestReconciler(fakeKube())
			r.store.client = direct

			err := r.releaseDeletedPods(context.Background(), g)
			if !errors.Is(err, test.fault) {
				t.Fatalf("identity check error = %v, want %v", err, test.fault)
			}

			if !reflect.DeepEqual(direct.gets, []client.ObjectKey{key}) {
				t.Fatalf("expected one direct GET for shared Pod key, got %v", direct.gets)
			}

			want := m
			if test.release {
				want.PodUID, want.PodNamespace, want.PodName = "", "", ""
			}

			for name, got := range g.Nodes {
				if got != want {
					t.Fatalf("%s: got %+v, want %+v", name, got, want)
				}
			}
		})
	}
}

func TestHistoricalPodIdentitySkipsUnprovableAndActive(t *testing.T) {
	direct := &podIdentityClient{Client: fakeKube(), t: t}
	r := newTestReconciler(fakeKube())
	r.store.client = direct

	for _, m := range []member{
		{ID: "active", IP: "10.1.1.1", PodUID: "uid", PodNamespace: "ns", PodName: "pod"},
		{ID: "released", PodNamespace: "ns", PodName: "pod"},
		{ID: "legacy", PodUID: "uid"},
		{ID: "missing-namespace", PodUID: "uid", PodName: "pod"},
		{ID: "missing-name", PodUID: "uid", PodNamespace: "ns"},
	} {
		g := &generation{Nodes: map[string]member{"node": m}}
		if err := r.releaseDeletedPods(context.Background(), g); err != nil {
			t.Fatal(err)
		}

		if g.Nodes["node"] != m || len(direct.gets) != 0 {
			t.Fatalf("unexpected identity change or API call for %+v: %+v, %v", m, g.Nodes["node"], direct.gets)
		}
	}
}

func TestHistoricalLegacyPodIdentityBackfill(t *testing.T) {
	for _, observation := range []string{"missing", "different-uid", "unavailable", "node-replaced", "selected"} {
		t.Run(observation, func(t *testing.T) {
			ctx := context.Background()
			n, p, svc := fixtures()
			p.UID = "old-uid"

			g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, svc)
			if err != nil {
				t.Fatal(err)
			}

			g.Revision = 1

			m := g.Nodes[n.Name]
			if m.PodNamespace != p.Namespace || m.PodName != p.Name {
				t.Fatalf("selected Pod key not captured: %+v", m)
			}

			m.PodNamespace, m.PodName = "", ""
			g.Nodes[n.Name] = m

			store := stateStore{client: fakeKube(), namespace: "state"}
			if err := store.commit(ctx, g, nil); err != nil {
				t.Fatal(err)
			}

			legacy, pointer, err := store.load(ctx, "default")
			if err != nil || !reflect.DeepEqual(legacy, g) {
				t.Fatalf("legacy format-2 state failed to round trip: %v", err)
			}

			p.Status.Phase = corev1.PodUnknown
			pods := []corev1.Pod{*p}
			key := n.Name

			switch observation {
			case "missing":
				pods = nil
			case "different-uid":
				pods[0].UID = "replacement"
			case "node-replaced":
				n.UID = "replacement-node"
				key = "deleted/" + m.ID
			case "selected":
				pods[0].Status.Phase = corev1.PodRunning
			}

			next, _, err := buildCacheFixture("default", legacy, []corev1.Node{*n}, pods, svc)
			if err != nil {
				t.Fatal(err)
			}

			got := next.Nodes[key]

			backfilled := observation != "missing" && observation != "different-uid"
			if got.PodUID != m.PodUID || (got.PodNamespace == p.Namespace && got.PodName == p.Name) != backfilled {
				t.Fatalf("incorrect UID-based backfill: %+v", got)
			}

			if legacy.Nodes[n.Name] != m {
				t.Fatal("backfill mutated committed generation")
			}

			next.Revision++
			if err := store.commit(ctx, next, pointer); err != nil {
				t.Fatal(err)
			}

			reloaded, _, err := store.load(ctx, "default")
			if err != nil || !reflect.DeepEqual(reloaded, next) {
				t.Fatalf("backfilled identity lost on restart: %v", err)
			}

			// No Pod exists in the authoritative API. Only keyed historical
			// members can now be released; unresolved legacy state stays safe.
			direct := &podIdentityClient{Client: fakeKube(), t: t}
			r := newTestReconciler(fakeKube())

			r.store.client = direct
			if err := r.releaseDeletedPods(ctx, reloaded); err != nil {
				t.Fatal(err)
			}

			wantReleased := backfilled && observation != "selected"
			if (reloaded.Nodes[key].PodUID == "") != wantReleased {
				t.Fatalf("incorrect legacy release after restart: %+v", reloaded.Nodes[key])
			}
		})
	}
}

func TestHistoricalPodCacheDisappearance(t *testing.T) {
	ctx := context.Background()
	n, p, svc := fixtures()
	p.UID = "pod-uid"
	cache := fakeKube(n, p, svc)
	direct := &podIdentityClient{Client: fakeKube(p), t: t}
	r := newTestReconciler(cache)
	r.store.client = direct
	// Rollout liveness has a separate inventory path; share durable storage
	// while enforcing no Pod LIST on the reconciler's identity-check client.
	r.server.controlStore = stateStore{client: direct.Client, namespace: r.store.namespace}
	first := historicalReconcile(t, r)
	historicalRetire(t, r, first)

	if len(direct.gets) != 0 {
		t.Fatal("active recipient required a direct Pod check")
	}

	if err := cache.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Restart so the check relies on the persisted key, not in-memory state.
	r.loaded, r.pointers = map[string]*generation{}, map[string]*corev1.ConfigMap{}
	next := historicalReconcile(t, r)

	m := next.g.Nodes[n.Name]
	if m.IP != "" || m.PodUID != string(p.UID) || len(direct.gets) != 1 {
		t.Fatalf("cache disappearance lost live authority: %+v, checks %v", m, direct.gets)
	}

	historicalRetire(t, r, next)

	if err := direct.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}

	direct.fault = errors.New("API unavailable")
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); !errors.Is(err, direct.fault) {
		t.Fatalf("expected authoritative API error, got %v", err)
	}

	if r.loaded["default"] != next.g {
		t.Fatal("failed identity check changed committed view")
	}

	direct.fault = nil

	released := historicalReconcile(t, r)
	if released.g.Nodes[n.Name].PodUID != "" || released.g.Revision != next.g.Revision+1 {
		t.Fatal("authoritative deletion did not commit release")
	}
}
