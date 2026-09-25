// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	testNodeUID                = "11111111-1111-4111-8111-111111111111"
	testOtherUID               = "22222222-2222-4222-8222-222222222222"
	testDaemonSetUID types.UID = "33333333-3333-4333-8333-333333333333"
)

func memberNode() corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: testNodeUID}}
}

func memberPod(uid types.UID, created int64, ip string) corev1.Pod {
	controller := true

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "racer-" + string(uid), UID: uid, Namespace: "racer",
			CreationTimestamp: metav1.NewTime(time.Unix(created, 0)),
			OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", UID: testDaemonSetUID, Controller: &controller}},
		},
		Spec:   corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{PodIP: ip},
	}
}

func TestParseAnnotations(t *testing.T) {
	zero, maxNUMA := uint32(0), ^uint32(0)
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        MemberAttributes
	}{
		{"defaults", nil, MemberAttributes{Shares: 4, Rails: []wire.Rail{}, AlignmentEnabled: true}},
		{"explicit empty rails", map[string]string{wire.RailsAnnotation: "[]"}, MemberAttributes{Shares: 4, Rails: []wire.Rail{}, AlignmentEnabled: true}},
		{"bounds and sorting", map[string]string{
			wire.SharesAnnotation: "4294967295", wire.AlignmentAnnotation: "false",
			wire.RailsAnnotation: `[{"rail":65535,"fabric":"β<&>","numa_node":4294967295},{"rail":0,"fabric":"a","numa_node":0}]`,
		}, MemberAttributes{Shares: ^uint32(0), Rails: []wire.Rail{{Rail: 0, Fabric: "a", NUMANode: &zero}, {Rail: 65535, Fabric: "β<&>", NUMANode: &maxNUMA}}}},
		{
			"identical duplicates",
			map[string]string{wire.RailsAnnotation: `[{"rail":2,"fabric":"b","numa_node":0},{"rail":1,"fabric":"a"},{"rail":2,"fabric":"b","numa_node":0},{"rail":1,"fabric":"a"}]`},
			MemberAttributes{Shares: 4, Rails: []wire.Rail{{Rail: 1, Fabric: "a"}, {Rail: 2, Fabric: "b", NUMANode: &zero}}, AlignmentEnabled: true},
		},
		{
			"unknown fields ignored",
			map[string]string{wire.RailsAnnotation: `[{"rail":0,"fabric":"a","Rail":1,"future":{"x":true}}]`},
			MemberAttributes{Shares: 4, Rails: []wire.Rail{{Fabric: "a"}}, AlignmentEnabled: true},
		},
		{"decimal shares", map[string]string{wire.SharesAnnotation: "0008"}, MemberAttributes{Shares: 8, Rails: []wire.Rail{}, AlignmentEnabled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := memberNode()
			node.Annotations = tc.annotations

			got, err := ParseAnnotations(&node)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestParseAnnotationsRejectsInvalidUpdates(t *testing.T) {
	for field, values := range map[string][]string{
		wire.SharesAnnotation:    {"", "0", "-1", "+1", "4294967296", "1.0", "1e2", "0x10", " 4", "4 ", "٤"},
		wire.AlignmentAnnotation: {"", "True", "FALSE", "1", "0", " true", "false "},
		wire.RailsAnnotation: {
			"", "null", "{}", "[null]", "[{}]", `[{"rail":0}]`, `[{"fabric":"a"}]`,
			`[{"rail":65536,"fabric":"a"}]`, `[{"rail":-1,"fabric":"a"}]`, `[{"rail":1.0,"fabric":"a"}]`,
			`[{"rail":"0","fabric":"a"}]`, `[{"rail":0,"fabric":""}]`, `[{"rail":0,"fabric":"a\n"}]`,
			`[{"rail":0,"fabric":"a\r"}]`, `[{"rail":0,"fabric":"a\u0000"}]`,
			`[{"rail":0,"fabric":"a","numa_node":null}]`, `[{"rail":0,"fabric":"a","numa_node":4294967296}]`,
			`[{"rail":0,"fabric":"a","numa_node":-1}]`, `[{"rail":0,"fabric":"a","numa_node":1e0}]`,
			`[{"rail":0,"fabric":"a","rail":1}]`, `[{"rail":0,"fabric":"a","\u0072ail":1}]`,
			`[{"rail":0,"fabric":"a","future":{"x":1,"x":2}}]`,
			`[{"Rail":0,"fabric":"a"}]`, `[{"rail":0,"fabric":"\ud800"}]`,
			"[{\"rail\":0,\"fabric\":\"\xff\"}]", "[] []",
			`[{"rail":0,"fabric":"a"},{"rail":0,"fabric":"b"}]`,
			`[{"rail":0,"fabric":"a"},{"rail":0,"fabric":"a","numa_node":0}]`,
			`[{"rail":0,"fabric":"a","numa_node":1},{"rail":0,"fabric":"a","numa_node":2}]`,
			`[{"rail":0,"fabric":"a","future":` + strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64) + "}]",
		},
	} {
		for _, value := range values {
			t.Run(field+"/"+value, func(t *testing.T) {
				node := memberNode()
				node.Annotations = map[string]string{field: value}

				got, err := ParseAnnotations(&node)
				if !errors.Is(err, wire.InvalidRequest) || !reflect.DeepEqual(got, MemberAttributes{}) {
					t.Fatalf("invalid update produced %#v, %v", got, err)
				}
			})
		}
	}

	if _, err := ParseAnnotations(nil); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("nil node: %v", err)
	}

	node := memberNode()

	node.Annotations = map[string]string{wire.RailsAnnotation: strings.Repeat(" ", 256*1024+1)}
	if _, err := ParseAnnotations(&node); !errors.Is(err, wire.TooLarge) {
		t.Fatalf("oversized rails: %v", err)
	}
}

func TestSelectEndpoint(t *testing.T) {
	pods := []corev1.Pod{
		memberPod("a", 1, "192.0.2.1"), memberPod("z", 2, "2001:db8::1"), memberPod("b", 2, "192.0.2.2"),
	}
	// The newest Pod need not be Ready; a ready older Pod is not preferred.
	pods[0].Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	for range 2 {
		got, err := SelectEndpoint(pods, testDaemonSetUID, "node-a", 7443)
		if err != nil || got != "[2001:db8::1]:7443" {
			t.Fatalf("got %q, %v", got, err)
		}

		slices.Reverse(pods)
	}

	for name, mutate := range map[string]func(*corev1.Pod){
		"terminating":      func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{} },
		"other node":       func(p *corev1.Pod) { p.Spec.NodeName = "node-b" },
		"unassigned":       func(p *corev1.Pod) { p.Spec.NodeName = "" },
		"missing uid":      func(p *corev1.Pod) { p.UID = "" },
		"no owner":         func(p *corev1.Pod) { p.OwnerReferences = nil },
		"other owner":      func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"not controller":   func(p *corev1.Pod) { p.OwnerReferences[0].Controller = nil },
		"false controller": func(p *corev1.Pod) { *p.OwnerReferences[0].Controller = false },
		"wrong kind":       func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "ReplicaSet" },
		"wrong api":        func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "other/v1" },
		"no ip":            func(p *corev1.Pod) { p.Status.PodIP = "" },
		"hostname":         func(p *corev1.Pod) { p.Status.PodIP = "example.com" },
		"ip with port":     func(p *corev1.Pod) { p.Status.PodIP = "192.0.2.9:7443" },
		"ip with zone":     func(p *corev1.Pod) { p.Status.PodIP = "fe80::1%eth0" },
	} {
		t.Run(name, func(t *testing.T) {
			ineligible := memberPod("new", 10, "192.0.2.9")
			mutate(&ineligible)

			if _, err := SelectEndpoint([]corev1.Pod{ineligible}, testDaemonSetUID, "node-a", 7443); !errors.Is(err, wire.Unavailable) {
				t.Fatalf("ineligible Pod admitted: %v", err)
			}

			got, err := SelectEndpoint([]corev1.Pod{ineligible, memberPod("old", 1, "192.0.2.1")}, testDaemonSetUID, "node-a", 7443)
			if err != nil || got != "192.0.2.1:7443" {
				t.Fatalf("eligible older Pod lost: %q, %v", got, err)
			}
		})
	}

	for _, tc := range []struct {
		uid  types.UID
		node string
		port uint16
	}{
		{testDaemonSetUID, "", 7443}, {testDaemonSetUID, "node-a", 0},
	} {
		if _, err := SelectEndpoint(pods, tc.uid, tc.node, tc.port); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("invalid endpoint configuration: %v", err)
		}
	}

	if _, err := SelectEndpoint(pods, "", "node-a", 7443); !errors.Is(err, wire.Unavailable) {
		t.Fatalf("missing workload must have no eligible endpoint: %v", err)
	}
}

func TestReconcileMembersColdStartAndRetention(t *testing.T) {
	node := memberNode()
	pod := memberPod("a", 1, "192.0.2.1")
	initial, diagnostics, err := ReconcileMembers([]corev1.Node{node}, []corev1.Pod{pod}, testDaemonSetUID, nil, 7443)

	want := wire.Member{Node: testNodeUID, Shares: 4, Rails: []wire.Rail{}, AlignmentEnabled: true, PeerEndpoint: "192.0.2.1:7443"}
	if err != nil || len(diagnostics) != 0 || !reflect.DeepEqual(initial[testNodeUID], want) {
		t.Fatalf("cold start: %#v, %v, %v", initial, diagnostics, err)
	}

	for _, tc := range []struct {
		name        string
		annotations map[string]string
		pods        []corev1.Pod
		shares      uint32
		endpoint    string
		diagnostics int
	}{
		{"pod gap", map[string]string{wire.SharesAnnotation: "8"}, nil, 8, "192.0.2.1:7443", 1},
		{"invalid annotations", map[string]string{wire.SharesAnnotation: "0"}, []corev1.Pod{memberPod("b", 2, "192.0.2.2")}, 4, "192.0.2.2:7443", 1},
		{"both missing", map[string]string{wire.SharesAnnotation: "0"}, nil, 4, "192.0.2.1:7443", 2},
		{"annotation unit", map[string]string{wire.SharesAnnotation: "8", wire.AlignmentAnnotation: "invalid"}, []corev1.Pod{pod}, 4, "192.0.2.1:7443", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := node.DeepCopy()
			node.Annotations = tc.annotations

			got, diagnostics, err := ReconcileMembers([]corev1.Node{*node}, tc.pods, testDaemonSetUID, initial, 7443)
			if err != nil || len(diagnostics) != tc.diagnostics || got[testNodeUID].Shares != tc.shares || got[testNodeUID].PeerEndpoint != tc.endpoint {
				t.Fatalf("warm reconcile: %#v, %v, %v", got, diagnostics, err)
			}

			if initial[testNodeUID].Shares != 4 || initial[testNodeUID].PeerEndpoint != "192.0.2.1:7443" {
				t.Fatal("mutated accepted input")
			}

			cold, diagnostics, err := ReconcileMembers([]corev1.Node{*node}, tc.pods, testDaemonSetUID, nil, 7443)
			if err != nil || len(cold) != 0 || len(diagnostics) != tc.diagnostics {
				t.Fatalf("cold reconcile: %#v, %v, %v", cold, diagnostics, err)
			}

			for _, d := range diagnostics {
				if d.Object != node.Name || d.Field == "" || d.Reason == "" {
					t.Fatalf("unactionable diagnostic: %#v", d)
				}
			}
		})
	}
}

func TestReconcileMembersIdentityAndRemoval(t *testing.T) {
	node := memberNode()
	pod := memberPod("a", 1, "192.0.2.1")

	accepted, _, err := ReconcileMembers([]corev1.Node{node}, []corev1.Pod{pod}, testDaemonSetUID, nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	for _, label := range []string{"", "false", "true"} {
		node.Labels = map[string]string{wire.ExclusionLabel: label}

		got, diagnostics, err := ReconcileMembers([]corev1.Node{node}, []corev1.Pod{pod}, testDaemonSetUID, accepted, 7443)
		if err != nil || len(got) != 0 || len(diagnostics) != 0 {
			t.Fatalf("exclusion: %v, %v, %v", got, diagnostics, err)
		}

		node.Labels = nil

		got, _, err = ReconcileMembers([]corev1.Node{node}, nil, testDaemonSetUID, got, 7443)
		if err != nil || len(got) != 0 {
			t.Fatalf("exclusion history survived: %v, %v", got, err)
		}
	}

	got, _, err := ReconcileMembers(nil, nil, testDaemonSetUID, accepted, 7443)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("deletion: %v, %v", got, err)
	}

	node.UID = testOtherUID

	got, _, err = ReconcileMembers([]corev1.Node{node}, nil, testDaemonSetUID, accepted, 7443)
	if err != nil || len(got) != 0 {
		t.Fatalf("same-name recreation inherited history: %v, %v", got, err)
	}

	got, _, err = ReconcileMembers([]corev1.Node{node}, []corev1.Pod{pod}, testDaemonSetUID, accepted, 7443)
	if err != nil || len(got) != 1 || got[testOtherUID].Node != testOtherUID {
		t.Fatalf("new UID not admitted: %v, %v", got, err)
	}
	// Node readiness and deletion timestamps do not change ownership while the
	// Node remains in the Kubernetes input and is not explicitly excluded.
	node.DeletionTimestamp = &metav1.Time{}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}

	got, _, err = ReconcileMembers([]corev1.Node{node}, []corev1.Pod{pod}, testDaemonSetUID, got, 7443)
	if err != nil || len(got) != 1 {
		t.Fatalf("readiness removed ownership: %v, %v", got, err)
	}
}

func TestReconcileMembersDefaultsAndIsolation(t *testing.T) {
	node := memberNode()
	node.Annotations = map[string]string{wire.SharesAnnotation: "8", wire.AlignmentAnnotation: "false", wire.RailsAnnotation: `[{"rail":0,"fabric":"a","numa_node":1}]`}
	pod := memberPod("a", 1, "192.0.2.1")

	accepted, _, err := ReconcileMembers([]corev1.Node{node}, []corev1.Pod{pod}, testDaemonSetUID, nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	node.Annotations[wire.SharesAnnotation] = "invalid"

	got, _, err := ReconcileMembers([]corev1.Node{node}, nil, testDaemonSetUID, accepted, 7443)
	if err != nil || !reflect.DeepEqual(got, accepted) {
		t.Fatalf("retention: %v, %v", got, err)
	}

	got[testNodeUID].Rails[0].Fabric = "changed"
	*got[testNodeUID].Rails[0].NUMANode = 9
	delete(got, testNodeUID)

	if accepted[testNodeUID].Rails[0].Fabric != "a" || *accepted[testNodeUID].Rails[0].NUMANode != 1 {
		t.Fatal("retention aliases accepted state")
	}

	node.Annotations = nil

	got, _, err = ReconcileMembers([]corev1.Node{node}, nil, testDaemonSetUID, accepted, 7443)
	if err != nil || got[testNodeUID].Shares != 4 || !got[testNodeUID].AlignmentEnabled || len(got[testNodeUID].Rails) != 0 {
		t.Fatalf("removed annotations did not default: %v, %v", got, err)
	}
}

func TestReconcileMembersRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes []corev1.Node
		uid   types.UID
		port  uint16
	}{
		{"missing port", nil, testDaemonSetUID, 0},
		{"duplicate node", []corev1.Node{memberNode(), memberNode()}, testDaemonSetUID, 7443},
		{"missing uid", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}, testDaemonSetUID, 7443},
		{"malformed uid", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "invalid"}}}, testDaemonSetUID, 7443},
		{"missing name", []corev1.Node{{ObjectMeta: metav1.ObjectMeta{UID: testNodeUID}}}, testDaemonSetUID, 7443},
		{"duplicate name", []corev1.Node{memberNode(), {ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: testOtherUID}}}, testDaemonSetUID, 7443},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := ReconcileMembers(tc.nodes, nil, tc.uid, nil, tc.port)
			if !errors.Is(err, wire.InvalidRequest) || got != nil {
				t.Fatalf("invalid inputs accepted: %v, %v", got, err)
			}
		})
	}
}

func TestReconcileCandidateHashesAndOrdering(t *testing.T) {
	nodeA, nodeB := memberNode(), memberNode()
	nodeB.Name, nodeB.UID = "node-b", testOtherUID
	nodeA.Annotations = map[string]string{wire.RailsAnnotation: `[{"rail":2,"fabric":"b"},{"rail":1,"fabric":"a"},{"rail":1,"fabric":"a"}]`}
	podA, podB := memberPod("a", 1, "192.0.2.1"), memberPod("b", 1, "192.0.2.2")
	podB.Spec.NodeName = "node-b"
	nodes, pods := []corev1.Node{nodeB, nodeA}, []corev1.Pod{podA, podB}
	nodesBefore := []corev1.Node{*nodeB.DeepCopy(), *nodeA.DeepCopy()}
	podsBefore := []corev1.Pod{*podA.DeepCopy(), *podB.DeepCopy()}

	members, diagnostics, err := ReconcileMembers(nodes, pods, testDaemonSetUID, nil, 7443)
	if err != nil || len(diagnostics) != 0 || !reflect.DeepEqual(nodes, nodesBefore) || !reflect.DeepEqual(pods, podsBefore) {
		t.Fatalf("candidate failed or mutated inputs: %v, %v", diagnostics, err)
	}

	candidate := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: testNodeUID, Members: []wire.Member{members[testOtherUID], members[testNodeUID]}}

	content, membership, err := wire.ContentHashes(candidate)
	if err != nil {
		t.Fatal(err)
	}

	slices.Reverse(nodes)
	slices.Reverse(pods)

	nodes[0].Annotations[wire.RailsAnnotation] = `[{"rail":1,"fabric":"a"},{"rail":2,"fabric":"b"}]`

	members, _, err = ReconcileMembers(nodes, pods, testDaemonSetUID, nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	candidate.Members = []wire.Member{members[testNodeUID], members[testOtherUID]}

	contentAgain, membershipAgain, err := wire.ContentHashes(candidate)
	if err != nil || contentAgain != content || membershipAgain != membership {
		t.Fatalf("order/dedup changed hashes: %v", err)
	}

	candidate.Caches, err = BuildCatalog([]racerv1.ClusterCache{catalogCache("cache-a", testNodeUID, nil)})
	if err != nil {
		t.Fatal(err)
	}

	contentAgain, membershipAgain, err = wire.ContentHashes(candidate)
	if err != nil || contentAgain == content || membershipAgain != membership {
		t.Fatalf("cache-only hash change: %v", err)
	}

	candidate.Members[0].PeerEndpoint = "192.0.2.9:7443"

	_, membershipAgain, err = wire.ContentHashes(candidate)
	if err != nil || membershipAgain == membership {
		t.Fatalf("endpoint must change membership hash: %v", err)
	}
}

func TestReconcileMembersLimit(t *testing.T) {
	nodes := make([]corev1.Node, wire.MaxMembers+1)

	accepted := make(AcceptedMembers, len(nodes))
	for i := range nodes {
		id := fmt.Sprintf("%08x-0000-0000-0000-000000000000", i)
		nodes[i] = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: id, UID: types.UID(id)}}
		accepted[wire.NodeID(id)] = wire.Member{Node: wire.NodeID(id), Shares: 4, PeerEndpoint: "192.0.2.1:7443", Rails: []wire.Rail{}, AlignmentEnabled: true}
	}

	got, _, err := ReconcileMembers(nodes, nil, testDaemonSetUID, accepted, 7443)
	if !errors.Is(err, wire.TooLarge) || got != nil {
		t.Fatalf("oversized membership: %d, %v", len(got), err)
	}
	// The bound is on admitted members, not all observed Nodes.
	nodes[0].Labels = map[string]string{wire.ExclusionLabel: ""}

	got, _, err = ReconcileMembers(nodes, nil, testDaemonSetUID, accepted, 7443)
	if err != nil || len(got) != wire.MaxMembers {
		t.Fatalf("membership at bound: %d, %v", len(got), err)
	}
}

func TestReconcileMembersMissingWorkloadAndRecovery(t *testing.T) {
	empty, diagnostics, err := ReconcileMembers(nil, nil, "", nil, 7443)
	if err != nil || empty == nil || len(empty) != 0 || len(diagnostics) != 0 {
		t.Fatalf("empty initial reconcile: %#v, %v, %v", empty, diagnostics, err)
	}

	node := memberNode()
	pods := []corev1.Pod{memberPod("a", 1, "192.0.2.1")}

	accepted, _, err := ReconcileMembers([]corev1.Node{node}, pods, testDaemonSetUID, nil, 7443)
	if err != nil {
		t.Fatal(err)
	}

	var got AcceptedMembers
	for _, uid := range []types.UID{"", "replacement-daemonset"} {
		got, diagnostics, err = ReconcileMembers([]corev1.Node{node}, pods, uid, accepted, 7443)
		if err != nil || !reflect.DeepEqual(got, accepted) || len(diagnostics) != 1 {
			t.Fatalf("workload gap: %v, %v, %v", got, diagnostics, err)
		}

		got, diagnostics, err = ReconcileMembers([]corev1.Node{node}, pods, uid, nil, 7443)
		if err != nil || len(got) != 0 || len(diagnostics) != 1 {
			t.Fatalf("cold workload gap: %v, %v, %v", got, diagnostics, err)
		}
	}

	node.Annotations = map[string]string{wire.SharesAnnotation: "invalid-sensitive-input"}

	cold, diagnostics, err := ReconcileMembers([]corev1.Node{node}, pods, testDaemonSetUID, nil, 7443)
	if err != nil || len(cold) != 0 || len(diagnostics) != 1 || strings.Contains(diagnostics[0].Reason, "invalid-sensitive-input") {
		t.Fatalf("cold invalid annotations: %v, %v, %v", cold, diagnostics, err)
	}

	node.Annotations[wire.SharesAnnotation] = "16"

	got, diagnostics, err = ReconcileMembers([]corev1.Node{node}, pods, testDaemonSetUID, cold, 7443)
	if err != nil || got[testNodeUID].Shares != 16 || len(diagnostics) != 0 {
		t.Fatalf("corrected inputs not admitted: %v, %v, %v", got, diagnostics, err)
	}
}
