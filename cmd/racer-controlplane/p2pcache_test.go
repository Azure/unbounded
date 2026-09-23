// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer"
)

func cacheFixture() *racerapi.P2PCache {
	return &racerapi.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "dataset", UID: "cache-uid", Generation: 1}, Spec: racerapi.P2PCacheSpec{CacheGeneration: 1, MaxCandidateAttempts: 3}}
}

func TestP2PCacheGenerationIdentityAndWithdrawal(t *testing.T) {
	node, pod, _ := fixtures()
	pod.UID, pod.Spec.ServiceAccountName = "pod-uid", "racer-dataplane"
	cache := cacheFixture()
	build := func(previous *generation, caches ...racerapi.P2PCache) *generation {
		t.Helper()

		g, err := buildCacheGeneration("default", previous, []corev1.Node{*node}, []corev1.Pod{*pod}, caches, racer.SocketRoot)
		if err != nil {
			t.Fatal(err)
		}

		return g
	}

	first := build(nil, *cache)
	if first.Volume.ID != string(cache.UID) || len(first.Owners) != 262144 || first.Volume.CacheSocket != "/dev/racer/dataset/cache" || first.Volume.OriginSocket != "/dev/racer/dataset/origin" {
		t.Fatalf("invalid cache generation: %+v", first.Volume)
	}

	empty := build(first)
	if len(empty.volumes()) != 0 || !empty.Withdrawn[string(cache.UID)] || empty.Nodes[node.Name].IP == "" {
		t.Fatal("withdrawal lost idle participant or retirement tracking")
	}

	cache.UID = "replacement-uid"

	replacement := build(empty, *cache)
	if replacement.Volume.ID == first.Volume.ID || replacement.Volume.CacheSocket != first.Volume.CacheSocket {
		t.Fatal("recreation did not separate identity from endpoint lifetime")
	}
}

func TestP2PCacheStatusMissingParticipantsAndFreshness(t *testing.T) {
	node, pod, _ := fixtures()
	node.Labels[corev1.LabelOSStable] = "linux"
	pod.UID, pod.Spec.ServiceAccountName = "pod-uid", "racer-dataplane"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	cache := cacheFixture()
	site := machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: machina.SiteSpec{Components: machina.SiteComponents{Racer: &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: ptr.To(true)}}}}}

	g, err := buildCacheGeneration("default", nil, []corev1.Node{*node}, []corev1.Pod{*pod}, []racerapi.P2PCache{*cache}, racer.SocketRoot)
	if err != nil {
		t.Fatal(err)
	}

	g.Revision = 1

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{}
	if err := server.install(index); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	roll := &rollout{revision: 1, phase: 4, acks: map[string]rolloutAck{g.Nodes[node.Name].ID: {boot: "boot", phase: 4, seen: now, healthy: true}}}
	server.rollouts = map[string]*rollout{"default": roll}
	r := &cacheStatusReconciler{server: server}

	status := r.status(cache, []machina.Site{site}, []corev1.Node{*node}, []corev1.Pod{*pod}, now)
	if status.Participants.Desired != 1 || status.Participants.Ready != 1 || !meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatalf("fresh participant not ready: %+v", status)
	}

	missing := node.DeepCopy()
	missing.Name, missing.UID = "starting", "starting-uid"

	status = r.status(cache, []machina.Site{site}, []corev1.Node{*node, *missing}, []corev1.Pod{*pod}, now)
	if status.Participants.Desired != 2 || status.Participants.Ready != 1 || meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatalf("starting participant omitted: %+v", status)
	}

	status = r.status(cache, []machina.Site{site}, []corev1.Node{*node}, []corev1.Pod{*pod}, now.Add(storageFreshness))
	if status.Participants.Ready != 0 {
		t.Fatal("stale activation remained ready")
	}

	ack := roll.acks[g.Nodes[node.Name].ID]
	ack.healthy = false
	roll.acks[g.Nodes[node.Name].ID] = ack

	status = r.status(cache, []machina.Site{site}, []corev1.Node{*node}, []corev1.Pod{*pod}, now)
	if status.Participants.Ready != 0 {
		t.Fatal("unhealthy worker remained ready despite fresh activation")
	}

	ack.healthy = true
	roll.acks[g.Nodes[node.Name].ID] = ack

	cache.Generation++

	status = r.status(cache, []machina.Site{site}, []corev1.Node{*node}, []corev1.Pod{*pod}, now)
	if status.Participants.Ready != 0 || status.ObservedGeneration != cache.Generation {
		t.Fatal("old activation accepted for updated resource")
	}
}

func TestP2PCacheStatusDurablePublishedGeneration(t *testing.T) {
	ctx := context.Background()
	node, pod, cache := fixtures()
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	extra := cache.DeepCopy()
	extra.Name, extra.UID = "z-extra", "extra-uid"
	kube := fakeKube(node, pod, cache, extra)

	topology := newTestReconciler(kube)
	if _, err := topology.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}); err != nil {
		t.Fatal(err)
	}

	// Reconcile indexes before incrementing the candidate revision, then commits
	// and publishes it. Exercise that path rather than synthesizing an index.
	index := topology.server.source.topologies[identityBytes("universe", "default")]

	roll, err := topology.server.rolloutFor(ctx, index)
	if err != nil {
		t.Fatal(err)
	}

	for phase := uint32(2); phase <= 4; phase++ {
		if err := topology.server.persistPhase(ctx, "default", roll, phase); err != nil {
			t.Fatal(err)
		}
	}

	statusController := &cacheStatusReconciler{client: kube, server: topology.server}
	check := func(t *testing.T, ready bool) {
		t.Helper()

		roll.acks[index.g.Nodes[node.Name].ID] = rolloutAck{boot: "boot", phase: 4, healthy: true, seen: time.Now()}

		if _, err := statusController.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}); err != nil {
			t.Fatal(err)
		}

		var actual racerapi.P2PCache
		if err := kube.Get(ctx, client.ObjectKeyFromObject(cache), &actual); err != nil {
			t.Fatal(err)
		}

		wantParticipants := int32(1)
		if roll.invalid {
			wantParticipants = 0
		}

		if actual.Status.Participants.Desired != 1 || actual.Status.Participants.Ready != wantParticipants || meta.IsStatusConditionTrue(actual.Status.Conditions, racerapi.ConditionReady) != ready {
			t.Fatalf("want Ready=%v with %d ready participants: %+v", ready, wantParticipants, actual.Status)
		}
	}
	check(t, true)
	check(t, true) // Reuse the published digest on subsequent status reconciliations.

	if roll.pointer.Name != stateName("default")+"-rollout" || roll.pointer.Data["manifest"] != "" {
		t.Fatal("fixture must use the production rollout object, not the commit pointer")
	}

	// A metadata-only commit-pointer update changes its RV, not publication identity.
	_, pointer, err := topology.store.load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	pointer.Annotations = map[string]string{"review": "metadata-only"}
	if err := kube.Update(ctx, pointer); err != nil {
		t.Fatal(err)
	}

	check(t, true)

	for _, test := range []struct {
		name string
		edit func(*generation)
	}{
		{"primary payload at same revision", func(g *generation) { g.Volume.OriginSocket = "/dev/racer/changed/origin" }},
		{"additional payload at same revision", func(g *generation) { g.Additional[0].Volume.Cache++ }},
		{"withdrawal at same revision", func(g *generation) { g.Withdrawn = map[string]bool{"other-cache": true} }},
		{"new revision", func(g *generation) { g.Revision++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, pointer, err := topology.store.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			test.edit(candidate)

			if err := topology.store.commit(ctx, candidate, pointer); err != nil {
				t.Fatal(err)
			}

			check(t, false)

			_, pointer, err = topology.store.load(ctx, "default")
			if err != nil {
				t.Fatal(err)
			}

			if err := topology.store.commit(ctx, index.g, pointer); err != nil {
				t.Fatal(err)
			}

			check(t, true)
		})
	}

	roll.invalid = true

	check(t, false)

	roll.invalid = false

	check(t, true)
}

func TestP2PCacheMultiSiteWithdrawal(t *testing.T) {
	cache := cacheFixture()
	server := &Server{rollouts: map[string]*rollout{}}
	r := &cacheStatusReconciler{server: server}
	now := time.Now()

	var (
		sites []machina.Site
		nodes []corev1.Node
		pods  []corev1.Pod
	)

	for _, name := range []string{"east", "west"} {
		site := machina.Site{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"region": name}}, Spec: machina.SiteSpec{Components: machina.SiteComponents{Racer: &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: ptr.To(true)}}}}}
		sites = append(sites, site)
		node, pod, _ := fixtures()
		node.Name, node.UID = name, types.UID(name)
		node.Labels[racer.SiteLabelKey], node.Labels[corev1.LabelOSStable] = name, "linux"
		pod.Name, pod.UID, pod.Spec.NodeName, pod.Spec.ServiceAccountName = name, types.UID(name+"-pod"), name, "racer-dataplane"
		pod.Labels[universeAnnotation] = name
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}

		nodes, pods = append(nodes, *node), append(pods, *pod)

		g, err := buildCacheGeneration(name, nil, []corev1.Node{*node}, []corev1.Pod{*pod}, []racerapi.P2PCache{*cache}, racer.SocketRoot)
		if err != nil {
			t.Fatal(err)
		}

		g.Revision = 1

		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		if err := server.install(index); err != nil {
			t.Fatal(err)
		}

		server.rollouts[name] = &rollout{revision: 1, phase: 4, acks: map[string]rolloutAck{g.Nodes[name].ID: {boot: name, phase: 4, healthy: true, seen: now}}}
	}

	status := r.status(cache, sites, nodes, pods, now)
	if status.Participants.Desired != 2 || status.Participants.Ready != 2 || !meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatalf("multi-Site activation: %+v", status)
	}
	// A Site label edit withdraws west without changing the cache generation.
	cache.Spec.SiteSelector.MatchLabels = map[string]string{"region": "east"}

	status = r.status(cache, sites, nodes, pods, now)
	if status.Participants.Desired != 1 || status.Participants.Ready != 1 || meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatalf("withdrawal ignored old publication: %+v", status)
	}

	key := identityBytes("universe", "west")
	previous := server.source.topologies[key].g

	t.Run("durable inventory survives missing publications", func(t *testing.T) {
		objects := []client.Object{cache.DeepCopy()}
		for i := range sites {
			objects = append(objects, sites[i].DeepCopy(), nodes[i].DeepCopy(), pods[i].DeepCopy())
		}

		kube := fakeKube(objects...)

		store := stateStore{client: kube, namespace: "state"}
		for _, topology := range server.source.topologies {
			if err := store.commit(context.Background(), topology.g, nil); err != nil {
				t.Fatal(err)
			}
		}

		fresh := &Server{controlStore: store, rollouts: map[string]*rollout{}}

		east, _, err := store.load(context.Background(), "east")
		if err != nil {
			t.Fatal(err)
		}

		index, err := indexGeneration(east)
		if err != nil {
			t.Fatal(err)
		}

		if err := fresh.install(index); err != nil {
			t.Fatal(err)
		}

		eastRoll, err := fresh.rolloutFor(context.Background(), index)
		if err != nil {
			t.Fatal(err)
		}

		if err := fresh.persistPhase(context.Background(), "east", eastRoll, 4); err != nil {
			t.Fatal(err)
		}

		eastRoll.acks = server.rollouts["east"].acks
		statusController := &cacheStatusReconciler{client: kube, server: fresh}
		check := func() {
			t.Helper()

			if _, err := statusController.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cache.Name}}); err != nil {
				t.Fatal(err)
			}

			var actual racerapi.P2PCache
			if err := kube.Get(context.Background(), types.NamespacedName{Name: cache.Name}, &actual); err != nil {
				t.Fatal(err)
			}

			if actual.Status.Participants.Ready != 1 || meta.IsStatusConditionTrue(actual.Status.Conditions, racerapi.ConditionReady) {
				t.Fatalf("unloaded west falsely converged: %+v", actual.Status)
			}
		}
		check()

		if err := fresh.install(server.source.topologies[key]); err != nil {
			t.Fatal(err)
		}
		// An ambiguous commit unpublishes west without removing its durable pointer.
		delete(fresh.source.topologies, key)
		check()
	})

	withdrawn, err := buildCacheGeneration("west", previous, nodes, pods[1:], nil, racer.SocketRoot)
	if err != nil {
		t.Fatal(err)
	}

	withdrawn.Revision++

	index, err := indexGeneration(withdrawn)
	if err != nil {
		t.Fatal(err)
	}

	if err := server.install(index); err != nil {
		t.Fatal(err)
	}

	roll := server.rollouts["west"]
	roll.revision, roll.phase = withdrawn.Revision, 3

	status = r.status(cache, sites, nodes, pods, now)
	if meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatal("withdrawal ready before retirement")
	}

	roll.phase = 4

	status = r.status(cache, sites, nodes, pods, now)
	if !meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatalf("retired withdrawal not converged: %+v", status)
	}

	prior, err := indexGeneration(previous)
	if err != nil {
		t.Fatal(err)
	}

	data, err := marshalSnapshot(prior.snapshot(previous.Nodes["west"].ID))
	if err != nil {
		t.Fatal(err)
	}

	wildcard := forwardDecision{Snapshot: data, PodUID: previous.Nodes["west"].PodUID}
	bound := wildcard
	bound.Boot = identity("boot", "west")
	setHistory := func(entries ...forwardDecision) {
		t.Helper()

		raw, err := encodeForwards(entries)
		if err != nil {
			t.Fatal(err)
		}

		roll.pointer = &corev1.ConfigMap{Data: map[string]string{"forwards": raw}}
		if _, err := roll.forwardHistory("west"); err != nil {
			t.Fatal("invalid test forward history", err)
		}
	}
	setHistory(wildcard, bound)

	status = r.status(cache, sites, nodes, pods, now)
	if meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatal("bound retirement obligation ignored")
	}

	setHistory(wildcard)

	status = r.status(cache, sites, nodes, pods, now)
	if !meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatal("retained wildcard prevented completed withdrawal")
	}

	roll.acks = map[string]rolloutAck{}

	status = r.status(cache, sites, nodes, pods, now)
	if meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionReady) {
		t.Fatal("controller restart must require fresh withdrawal acknowledgment")
	}
}

func TestP2PCacheSocketIdentityAndValidation(t *testing.T) {
	a, b := cacheFixture(), cacheFixture()
	a.Name, a.UID = "a", "a-uid"
	b.Name, b.UID = "b", "b-uid"
	build := func(previous *generation, caches ...racerapi.P2PCache) (*generation, error) {
		return buildCacheGeneration("default", previous, nil, nil, caches, racer.SocketRoot)
	}

	first, err := build(nil, *b, *a)
	if err != nil {
		t.Fatal(err)
	}

	ordered, err := build(nil, *a, *b)
	if err != nil || !reflect.DeepEqual(first, ordered) {
		t.Fatal("allocation depends on list order", err)
	}

	if first.Volume.CacheSocket != "/dev/racer/a/cache" || first.Additional[0].Volume.CacheSocket != "/dev/racer/b/cache" {
		t.Fatal("cache socket paths are not derived from sorted names")
	}

	withdrawn, err := build(first)
	if err != nil {
		t.Fatal(err)
	}

	a.UID = "recreated"

	returned, err := build(withdrawn, *a)
	if err != nil || returned.Volume.CacheSocket != first.Volume.CacheSocket || returned.Volume.ID == first.Volume.ID {
		t.Fatal("recreation lost socket identity or reused cache UID", err)
	}

	a.Name = "invalid/name"
	if _, err := build(returned, *a); err == nil {
		t.Fatal("invalid socket name accepted")
	}
}

func TestP2PCacheStatusPatchAndEmptySelections(t *testing.T) {
	ctx := context.Background()
	cache := cacheFixture()
	kube := fakeKube(cache)
	r := &cacheStatusReconciler{client: kube, server: &Server{}}

	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cache)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(ctx, request.NamespacedName, cache); err != nil {
		t.Fatal(err)
	}

	if !meta.IsStatusConditionTrue(cache.Status.Conditions, racerapi.ConditionAccepted) || meta.FindStatusCondition(cache.Status.Conditions, racerapi.ConditionReady).Reason != "NoParticipants" {
		t.Fatal(cache.Status)
	}

	version := cache.ResourceVersion

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(ctx, request.NamespacedName, cache); err != nil {
		t.Fatal(err)
	}

	if cache.ResourceVersion != version {
		t.Fatal("unchanged status patched again")
	}

	cache.Spec.SiteSelector.MatchLabels = map[string]string{"absent": "true"}

	cache.Generation++
	if err := kube.Update(ctx, cache); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(ctx, request.NamespacedName, cache); err != nil {
		t.Fatal(err)
	}

	if cache.Status.ObservedGeneration != cache.Generation || meta.FindStatusCondition(cache.Status.Conditions, racerapi.ConditionReady).Reason != "NoMatchingSites" {
		t.Fatal(cache.Status)
	}

	cache.Spec.SiteSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "bad", Operator: "Invalid"}}

	status := r.status(cache, nil, nil, nil, time.Now())
	if meta.IsStatusConditionTrue(status.Conditions, racerapi.ConditionAccepted) {
		t.Fatal("invalid selector accepted without Sites")
	}
}
