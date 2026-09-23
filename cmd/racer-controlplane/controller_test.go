// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

// Shared Kubernetes fixtures and reconciliation behavior.

func fixtures() (*corev1.Node, *corev1.Pod, *racerapi.P2PCache) {
	controller := true
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid", Labels: map[string]string{racer.SiteLabelKey: "default", corev1.LabelOSStable: "linux"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "pod-uid", Labels: map[string]string{dataplaneLabel: "true", universeAnnotation: "default"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "racer", UID: "ds", Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "node", ServiceAccountName: "racer-dataplane"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.1.1.1"}}
	s := &racerapi.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "volume", UID: "cache-uid", Generation: 1}, Spec: racerapi.P2PCacheSpec{CacheGeneration: 1, MaxCandidateAttempts: 3}}

	return n, p, s
}

func fakeKube(objects ...client.Object) client.Client {
	found := false

	for _, o := range objects {
		if s, ok := o.(*machina.Site); ok && s.Name == "default" {
			found = true
		}
	}

	if !found {
		enabled := true
		objects = append(objects, &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: machina.SiteSpec{Components: machina.SiteComponents{Racer: &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: &enabled}}}}})
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = machina.AddToScheme(scheme)
	_ = racerapi.AddToScheme(scheme)

	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&racerapi.P2PCache{}).WithObjects(objects...).WithIndex(&corev1.Node{}, universeIndex, objectUniverses).Build()
}

func buildGeneration(name string, previous *generation, nodes []corev1.Node, pods []corev1.Pod, caches []racerapi.P2PCache) (*generation, *racerapi.P2PCache, error) {
	g, err := buildCacheGeneration(name, previous, nodes, pods, caches, nil, racer.SocketRoot)
	return g, nil, err
}

func newTestReconciler(c client.Client) *reconciler {
	return &reconciler{client: c, store: stateStore{client: c, namespace: "state"}, server: &Server{controlStore: stateStore{client: c, namespace: "state"}}, loaded: map[string]*generation{}, pointers: map[string]*corev1.ConfigMap{}, podNamespace: "ns"}
}

func buildCacheFixture(name string, previous *generation, nodes []corev1.Node, pods []corev1.Pod, caches ...*racerapi.P2PCache) (*generation, *racerapi.P2PCache, error) {
	var items []racerapi.P2PCache
	for _, cache := range caches {
		items = append(items, *cache)
	}

	return buildGeneration(name, previous, nodes, pods, items)
}

func TestReconcileRestartRemovalAndCacheSockets(t *testing.T) {
	ctx := context.Background()
	n, p, s := fixtures()
	c := fakeKube(n, p, s)
	r := newTestReconciler(c)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	step := func() {
		t.Helper()

		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}

		g := r.loaded["default"]

		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		for _, node := range g.Nodes {
			if snapshot := index.snapshot(node.ID); snapshot.Epoch != g.Revision {
				t.Fatalf("snapshot epoch = %d, want committed generation %d", snapshot.Epoch, g.Revision)
			}
		}
	}
	step()

	first := r.loaded["default"]
	if first.Revision != 1 || len(first.Owners) != int(racer.SlotCount) {
		t.Fatal("initial generation missing")
	}

	var updated racerapi.P2PCache
	if err := c.Get(ctx, client.ObjectKeyFromObject(s), &updated); err != nil {
		t.Fatal(err)
	}

	if first.Volume.CacheSocket != "/dev/racer/volume/cache" || first.Volume.OriginSocket != "/dev/racer/volume/origin" {
		t.Fatal("node-local sockets missing")
	}

	step()

	if r.loaded["default"].Revision != 1 {
		t.Fatal("no-op reconcile advanced revision")
	}

	r = newTestReconciler(c)

	step()

	if r.loaded["default"].Revision != 1 {
		t.Fatal("restart changed generation")
	}

	if err := c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}

	step()

	g := r.loaded["default"]
	if g.Revision != 2 || len(g.Owners) != 0 {
		t.Fatal("Pod removal did not remove listeners")
	}

	index, _ := indexGeneration(g)
	if len(index.snapshot(identity("node", "node-uid")).Volumes) != 0 {
		t.Fatal("removed node retains volume")
	}

	if err := c.Delete(ctx, s); err != nil {
		t.Fatal(err)
	}

	step()

	if r.loaded["default"].Volume != nil {
		t.Fatal("P2PCache removal retained volume")
	}
}

func TestValidationPreservesLastGeneration(t *testing.T) {
	ctx := context.Background()
	n, p, s := fixtures()
	c := fakeKube(n, p, s)
	r := newTestReconciler(c)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}

	var updated racerapi.P2PCache

	_ = c.Get(ctx, client.ObjectKeyFromObject(s), &updated)

	updated.Spec.MaxCandidateAttempts = 9
	if err := c.Update(ctx, &updated); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("expected invalid cache configuration: %v", err)
	}

	if r.loaded["default"].Revision != 1 {
		t.Fatal("invalid generation committed")
	}
}

func TestNodeReplacementUniverseAndMultipleVolumes(t *testing.T) {
	n, p, s := fixtures()

	g, _, err := buildGeneration("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, []racerapi.P2PCache{*s})
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 1
	n.UID = "replacement"

	next, _, err := buildGeneration("default", g, []corev1.Node{*n}, []corev1.Pod{*p}, []racerapi.P2PCache{*s})
	if err != nil {
		t.Fatal(err)
	}

	if next.Nodes["deleted/"+identity("node", "node-uid")].IP != "" || len(next.Nodes) != 2 {
		t.Fatal("replacement lost tombstone")
	}

	n.Labels[racer.SiteLabelKey] = "other"
	if other, _, err := buildGeneration("default", g, []corev1.Node{*n}, []corev1.Pod{*p}, []racerapi.P2PCache{*s}); err != nil || len(other.Owners) != 0 {
		t.Fatal("cross-universe Pod contributed owners")
	}

	n.Labels[racer.SiteLabelKey] = "default"
	p.UID = "replacement-pod"
	second := s.DeepCopy()
	second.Name = "second"
	second.UID = "second-cache-uid"

	multi, _, err := buildGeneration("default", g, []corev1.Node{*n}, []corev1.Pod{*p}, []racerapi.P2PCache{*s, *second})
	if err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(multi)
	if err != nil {
		t.Fatal(err)
	}

	snapshot := index.snapshot(multi.Nodes[n.Name].ID)
	if len(snapshot.Volumes) != 2 || snapshot.Volumes[0].PeerListen == snapshot.Volumes[1].PeerListen || snapshot.Volumes[0].CacheSocket == snapshot.Volumes[1].CacheSocket || snapshot.Volumes[0].PeerEndpoints == nil || snapshot.Volumes[1].PeerEndpoints == nil {
		t.Fatal("missing isolated volume listeners/policies")
	}
}

func TestDurableStateRejectsLegacyFormat(t *testing.T) {
	ctx := context.Background()
	store := stateStore{client: fakeKube(), namespace: "state"}
	g := testGeneration(8, 1)

	g.Format = 0
	if err := store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.load(ctx, "default"); err == nil {
		t.Fatal("accepted incompatible persisted generation")
	}
}

func TestDurableStateChunksConflictAndCorruption(t *testing.T) {
	ctx := context.Background()
	c := fakeKube()
	store := stateStore{client: c, namespace: "state"}
	g := testGeneration(8000, 8000)
	g.Ports = map[string]int32{"ns/volume": 10000}

	g.SlotHistory = map[string]uint32{"ns/volume": 8000}
	if err := store.commit(ctx, g, nil); err != nil {
		t.Fatal(err)
	}

	loaded, pointer, err := store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(g, loaded) {
		t.Fatal("state round trip differs")
	}

	var m manifest

	_ = json.Unmarshal([]byte(pointer.Data["manifest"]), &m)
	if len(m.Chunks) < 2 {
		t.Fatal("test did not exercise chunking")
	}

	stale := pointer.DeepCopy()

	g.Revision = 2
	if err := store.commit(ctx, g, pointer); err != nil {
		t.Fatal(err)
	}

	g.Revision = 3
	if err := store.commit(ctx, g, stale); err == nil {
		t.Fatal("stale pointer overwrote committed state")
	}

	loaded, pointer, err = store.load(ctx, "default")
	if err != nil || loaded.Revision != 2 {
		t.Fatalf("failed write exposed partial generation: %v", err)
	}

	_ = json.Unmarshal([]byte(pointer.Data["manifest"]), &m)
	part := &corev1.ConfigMap{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: "state", Name: m.Chunks[0]}, part)

	part.BinaryData["state"] = []byte("corrupt")
	if err := c.Update(ctx, part); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.load(ctx, "default"); err == nil {
		t.Fatal("accepted corrupt persisted state")
	}
}

// Historical Pod authority, deletion proofs and durable history collection.

// These controller tests use synthetic worker acknowledgments, but exercise
// real admission, durable topology/history, and the signed HTTP authorization path.
func historicalReconcile(t *testing.T, r *reconciler) *topologyIndex {
	t.Helper()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatal(err)
	}

	index, err := indexGeneration(r.loaded["default"])
	if err != nil {
		t.Fatal(err)
	}

	return index
}

func historicalRetire(t *testing.T, r *reconciler, index *topologyIndex) {
	t.Helper()

	roll, err := r.server.rolloutFor(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.server.persistPhase(context.Background(), "default", roll, 4); err != nil {
		t.Fatal(err)
	}

	for _, m := range index.g.Nodes {
		if m.IP != "" {
			roll.acks[m.ID] = rolloutAck{phase: 4}
		}
	}
}

func TestHistoricalPodChurn(t *testing.T) {
	for _, sameName := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-node-name=%t", sameName), func(t *testing.T) {
			ctx := context.Background()
			n, p, svc := fixtures()
			p.UID = "pod-0"
			kube := fakeKube(n, p, svc)
			r := newTestReconciler(kube)
			r.server.controlStore = r.store
			ids := []string{}

			for i := 0; i < catchupLimit+17; i++ {
				if i > 0 {
					if err := kube.Delete(ctx, p); err != nil {
						t.Fatal(err)
					}

					if err := kube.Delete(ctx, n); err != nil {
						t.Fatal(err)
					}

					n, p, _ = fixtures()

					n.UID, p.UID = types.UID(fmt.Sprintf("node-%d", i)), types.UID(fmt.Sprintf("pod-%d", i))
					if !sameName {
						n.Name = string(n.UID)
						p.Spec.NodeName = n.Name
					}

					if err := kube.Create(ctx, n); err != nil {
						t.Fatal(err)
					}

					if err := kube.Create(ctx, p); err != nil {
						t.Fatal(err)
					}
				}

				index := historicalReconcile(t, r)

				ids = append(ids, identity("node", string(n.UID)))

				if index.g.Revision != uint64(i+1) || len(index.g.Nodes) != i+1 || len(index.g.Owners) != int(racer.SlotCount) {
					t.Fatalf("churn %d: revision=%d recipients=%d owners=%d", i, index.g.Revision, len(index.g.Nodes), len(index.g.Owners))
				}

				for _, id := range ids[:len(ids)-1] {
					m := index.g.Nodes[index.byID[id]]

					snap := index.snapshot(id)
					if m.ID != id || m.IP != "" || m.PodUID != "" || snap == nil || hex.EncodeToString(snap.Node) != id || snap.Revision != uint64(i+1) || len(snap.Volumes) != 0 || len(snap.Peers) != 0 {
						t.Fatalf("churn %d historical identity %s lost or still authorized: %+v", i, id, m)
					}
				}

				historicalRetire(t, r, index)
				roll := r.server.rollouts["default"]

				rem, err := removalHistory(roll.pointer.Data["removals"], "default", index.g.Revision)
				if err != nil || len(rem) != 0 {
					t.Fatalf("churn %d recreates deleted Pod obligations: %d %v", i, len(rem), err)
				}

				fwd, err := forwardHistory(roll.pointer.Data["forwards"], "default", index.g.Revision)
				if err != nil || len(fwd) != 0 {
					t.Fatalf("churn %d retains inaccessible forwards: %d %v", i, len(fwd), err)
				}

				if i%67 == 0 || i == catchupLimit+16 {
					// Restart from ConfigMaps, including beyond the former 512 boundary.
					r = newTestReconciler(kube)
					r.server.controlStore = r.store

					reloaded := historicalReconcile(t, r)
					if reloaded.g.Revision != index.g.Revision || len(reloaded.g.Nodes) != len(ids) {
						t.Fatal("restart lost durable continuity")
					}

					historicalRetire(t, r, reloaded)
				}
			}
		})
	}
}

func TestHistoricalLiveExcludedPod(t *testing.T) {
	for _, exclusion := range []string{"site-excluded", "labels-changed", "node-not-ready", "node-deleted", "node-replaced", "pod-unavailable"} {
		t.Run(exclusion, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordinationFixture(t, nil)
			r := newTestReconciler(f.api)
			r.server = f.s
			f.index = historicalReconcile(t, r)
			historicalRetire(t, r, f.index)

			n, p, svc := fixtures()

			switch exclusion {
			case "site-excluded":
				if err := f.api.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
					t.Fatal(err)
				}

				n.Labels[racer.ExcludeLabelKey] = "true"
				if err := f.api.Update(ctx, n); err != nil {
					t.Fatal(err)
				}
			case "labels-changed", "pod-unavailable":
				if err := f.api.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
					t.Fatal(err)
				}

				if exclusion == "labels-changed" {
					p.Labels = nil // The authoritative check must not filter dataplane labels.
					if err := f.api.Update(ctx, p); err != nil {
						t.Fatal(err)
					}
				} else {
					p.Status.Phase = corev1.PodUnknown
					if err := f.api.Status().Update(ctx, p); err != nil {
						t.Fatal(err)
					}
				}
			case "node-not-ready":
				if err := f.api.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
					t.Fatal(err)
				}

				n.Status.Conditions = nil
				if err := f.api.Status().Update(ctx, n); err != nil {
					t.Fatal(err)
				}
			case "node-deleted", "node-replaced":
				if err := f.api.Delete(ctx, n); err != nil {
					t.Fatal(err)
				}

				if exclusion == "node-replaced" {
					n.UID = "replacement-node"

					n.Status.Conditions = nil
					if err := f.api.Create(ctx, n); err != nil {
						t.Fatal(err)
					}
				}
			}

			f.index = historicalReconcile(t, r)

			m := f.index.g.Nodes[f.index.byID[f.node]]
			if m.IP != "" || m.PodUID != "pod-uid" {
				t.Fatalf("live exclusion revoked catch-up: %+v", m)
			}

			if exclusion == "node-replaced" && f.index.g.Nodes["node"].PodUID != "" {
				t.Fatal("new Node inherited old bootstrap authorization without selection")
			}

			boot := strings.Repeat("ab", 32)
			cmd := catchupRequest(t, f, boot, "", 0, 200)
			digest := hex.EncodeToString(cmd.SnapshotDigest)

			snapshot, err := marshalSnapshot(f.index.snapshot(f.node))
			if err != nil {
				t.Fatal(err)
			}

			catchupRequest(t, f, boot, digest, 2, 200)
			// Force a later candidate while the excluded process misses activation.
			if err := f.api.Get(ctx, client.ObjectKeyFromObject(svc), svc); err == nil {
				svc.Spec.CacheGeneration = 2
				if err := f.api.Update(ctx, svc); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.api.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
					t.Fatal(err)
				}

				n.Annotations = map[string]string{annotationPrefix + "fabric": "changed"}
				if err := f.api.Update(ctx, n); err != nil {
					t.Fatal(err)
				}
			}

			f.index = historicalReconcile(t, r)
			if f.index.g.Revision != 3 {
				t.Fatal("exclusion did not advance")
			}

			f.s = &Server{controlStore: f.s.controlStore, signer: f.s.signer}
			r = newTestReconciler(f.api)
			r.server = f.s
			f.index = historicalReconcile(t, r)

			old := catchupRequest(t, f, boot, digest, 2, 200)
			if old.Revision != 2 || old.Phase != 4 || hex.EncodeToString(old.SnapshotDigest) != digest {
				t.Fatal("live excluded recipient lost exact terminal decision")
			}

			rem, err := removalHistory(f.durable(t).Data["removals"], "default", 3)
			if err != nil || len(rem) != 2 || !bytes.Equal(rem[0].Snapshot, snapshot) {
				t.Fatal("historical bytes changed", err)
			}
			// Only actual Pod deletion releases this retained historical authority.
			if err := f.api.Delete(ctx, p); err != nil {
				t.Fatal(err)
			}

			f.index = historicalReconcile(t, r)
			catchupRequest(t, f, boot, digest, 2, 403)

			if m := f.index.g.Nodes[f.index.byID[f.node]]; m.ID != f.node || m.PodUID != "" {
				t.Fatal("deleted Pod retained authority or lost snapshot identity")
			}
		})
	}
}

type historicalGetFailure struct{ client.Client }

func (c historicalGetFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Pod); ok {
		return errors.New("uncached Pod identity unavailable")
	}

	return c.Client.Get(ctx, key, obj, opts...)
}

func TestHistoricalPodDeletionCommitSafety(t *testing.T) {
	for _, fault := range []string{"inventory-failure", "no-commit", "lost-commit"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			f := newCoordinationFixture(t, nil)
			f.generation(t, false)

			boot := strings.Repeat("ab", 32)
			cmd := catchupRequest(t, f, boot, "", 0, 200)
			digest := hex.EncodeToString(cmd.SnapshotDigest)

			if _, err := f.s.rolloutBusy(ctx, f.index); err != nil {
				t.Fatal(err)
			}

			before := f.durable(t).Data["removals"]

			_, p, svc := fixtures()
			if err := f.api.Delete(ctx, svc); err != nil {
				t.Fatal(err)
			}

			if err := f.api.Delete(ctx, p); err != nil {
				t.Fatal(err)
			}

			r := newTestReconciler(f.api)
			r.server = f.s

			switch fault {
			case "inventory-failure":
				r.store.client = historicalGetFailure{f.api}
			case "no-commit":
				r.store.client = &forwardCommitFailure{Client: f.api, armed: true}
			case "lost-commit":
				r.store.client = &reviewCommitLoss{Client: f.api, lose: true}
			}

			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err == nil {
				t.Fatal("fault not exercised")
			}

			if f.durable(t).Data["removals"] != before {
				t.Fatal("uncommitted proposal collected history")
			}

			g, _, err := f.s.controlStore.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			f.index, err = indexGeneration(g)
			if err != nil {
				t.Fatal(err)
			}

			f.s = &Server{controlStore: f.s.controlStore, signer: f.s.signer}
			if err := f.s.install(f.index); err != nil {
				t.Fatal(err)
			}

			code := 200
			if fault == "lost-commit" {
				code = 403
			}

			catchupRequest(t, f, boot, digest, 2, code)
			r = newTestReconciler(f.api)
			r.server = f.s

			f.index = historicalReconcile(t, r)
			if f.index.g.Revision != 3 || f.index.g.Nodes["node"].PodUID != "" {
				t.Fatal("deletion did not commit exactly one durable successor")
			}

			if _, err := f.s.rolloutFor(ctx, f.index); err != nil {
				t.Fatal(err)
			}

			rem, err := removalHistory(f.durable(t).Data["removals"], "default", 3)
			if err != nil || len(rem) != 0 {
				t.Fatal("committed deletion did not reclaim history", err)
			}

			catchupRequest(t, f, boot, digest, 2, 403)
		})
	}
}

func TestHistoricalDeletedPodForwardGC(t *testing.T) {
	for _, fault := range []string{"lost", "lost-read", "no-commit", "history-conflict"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			f, roll, _ := forwardFixture(t)

			before, err := forwardHistory(roll.pointer.Data["forwards"], "default", roll.revision)
			if err != nil || len(before) == 0 || before[0].Ref == nil {
				t.Fatal("missing durable forward chunk reference", err)
			}

			n, p, svc := fixtures()
			for _, o := range []client.Object{n, p, svc} {
				if err := f.api.Delete(ctx, o); err != nil {
					t.Fatal(err)
				}
			}

			r := newTestReconciler(f.api)
			r.server = f.s

			f.index = historicalReconcile(t, r)
			if m := f.index.g.Nodes[f.index.byID[f.node]]; m.ID != f.node || m.PodUID != "" {
				t.Fatal("deletion did not commit historical authorization release")
			}
			// Crash between topology commit and ledger GC, then inject ambiguous
			// history writes. No proposal can collect these references beforehand.
			f.s = &Server{controlStore: f.s.controlStore, signer: f.s.signer}
			if err := f.s.install(f.index); err != nil {
				t.Fatal(err)
			}

			f.api.fault = fault
			if _, err := f.s.rolloutFor(ctx, f.index); err == nil {
				t.Fatal("history fault did not fail closed")
			}

			for i := 0; i < 4; i++ {
				roll, err = f.s.rolloutFor(ctx, f.index)
				if err == nil {
					break
				}
			}

			if err != nil {
				t.Fatal(err)
			}

			kept, err := forwardHistory(roll.pointer.Data["forwards"], "default", roll.revision)
			if err != nil || len(kept) != 0 {
				t.Fatal("inaccessible forward obligations not durably collected", err)
			}

			catchupRequest(t, f, strings.Repeat("ab", 32), "", 0, 403)
		})
	}
}

func TestModelRejectsInvalidCaches(t *testing.T) {
	for _, attempts := range []int32{0, 9} {
		n, p, cache := fixtures()

		cache.Spec.MaxCandidateAttempts = attempts
		if _, _, err := buildCacheFixture("default", nil, []corev1.Node{*n}, []corev1.Pod{*p}, cache); err == nil {
			t.Fatal("invalid cache accepted")
		}
	}
}
