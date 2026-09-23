// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
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

		g, err := buildCacheGeneration("default", previous, []corev1.Node{*node}, []corev1.Pod{*pod}, caches, nil, racer.SocketRoot)
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
	if replacement.Volume.ID == first.Volume.ID || replacement.Volume.Port != first.Volume.Port || replacement.Volume.CacheSocket != first.Volume.CacheSocket {
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

	g, err := buildCacheGeneration("default", nil, []corev1.Node{*node}, []corev1.Pod{*pod}, []racerapi.P2PCache{*cache}, nil, racer.SocketRoot)
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

		g, err := buildCacheGeneration(name, nil, []corev1.Node{*node}, []corev1.Pod{*pod}, []racerapi.P2PCache{*cache}, nil, racer.SocketRoot)
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

	withdrawn, err := buildCacheGeneration("west", previous, nodes, pods[1:], nil, nil, racer.SocketRoot)
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
}

func TestP2PCachePeerPortHistoryAndReservations(t *testing.T) {
	a, b := cacheFixture(), cacheFixture()
	a.Name, a.UID = "a", "a-uid"
	b.Name, b.UID = "b", "b-uid"
	reserved := reservedPorts{10000: true}
	build := func(previous *generation, caches ...racerapi.P2PCache) (*generation, error) {
		return buildCacheGeneration("default", previous, nil, nil, caches, reserved, racer.SocketRoot)
	}

	first, err := build(nil, *b, *a)
	if err != nil {
		t.Fatal(err)
	}

	ordered, err := build(nil, *a, *b)
	if err != nil || !reflect.DeepEqual(first, ordered) {
		t.Fatal("allocation depends on list order", err)
	}

	if first.Ports["a"] != 10001 || first.Ports["b"] != 10002 {
		t.Fatal(first.Ports)
	}

	withdrawn, err := build(first)
	if err != nil {
		t.Fatal(err)
	}

	a.UID = "recreated"

	returned, err := build(withdrawn, *a)
	if err != nil || returned.Volume.Port != 10001 {
		t.Fatal("lost historical reservation", err)
	}

	reserved[10001] = true

	if _, err := build(returned, *a); err == nil {
		t.Fatal("management reservation collision accepted")
	}

	full := &generation{Ports: map[string]int32{}}
	for port := int32(10000); port <= 29999; port++ {
		full.Ports[fmt.Sprint(port)] = port
	}

	if _, err := build(full, *a); err == nil {
		t.Fatal("exhausted peer range accepted")
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
