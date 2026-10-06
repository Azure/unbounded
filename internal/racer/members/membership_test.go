// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	nodeID  = "11111111-1111-4111-8111-111111111111"
	otherID = "22222222-2222-4222-8222-222222222222"
)

func observedInput() members.Input {
	controller := true

	return members.Input{
		Nodes: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: nodeID}}},
		PodsByNode: map[string][]corev1.Pod{"node": {{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "racer", Name: "pod", UID: "pod-uid",
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "dataplane", UID: "workload-uid", Controller: &controller}},
			},
			Spec:   corev1.PodSpec{NodeName: "node"},
			Status: corev1.PodStatus{PodIP: "192.0.2.1"},
		}}},
		Ownership: members.WorkloadIdentities{
			Namespace: "racer", Workloads: [2]members.WorkloadIdentity{{Name: "dataplane", UID: "workload-uid"}},
		},
		PeerPort: 7443,
	}
}

func TestReconcileCandidateRequiresExplicitHistoryAdvance(t *testing.T) {
	input := observedInput()
	input.Nodes[0].Annotations = map[string]string{wire.SharesAnnotation: "8", wire.RDMANICsAnnotation: `[{"device":"nic","port":1,"rail":0,"numa_node":1}]`}
	accepted := members.History{}

	candidate, err := members.Reconcile(input, accepted)
	if err != nil || len(candidate.Diagnostics) != 0 || len(candidate.Members) != 1 || len(accepted) != 0 {
		t.Fatalf("candidate changed accepted history: %+v, %v, %v", candidate, accepted, err)
	}

	input.PodsByNode = nil
	input.Nodes[0].Annotations[wire.SharesAnnotation] = "invalid"

	unpublished, err := members.Reconcile(input, accepted)
	if err != nil || len(unpublished.Members) != 0 || len(unpublished.Diagnostics) != 2 {
		t.Fatalf("unpublished candidate survived a gap: %+v, %v", unpublished, err)
	}

	accepted = candidate.Members // Simulate a successful publication.

	retained, err := members.Reconcile(input, accepted)
	if err != nil || !reflect.DeepEqual(retained.Members, accepted) {
		t.Fatalf("accepted member did not survive gap: %+v, %v", retained, err)
	}

	retained.Members[nodeID].RDMANICs[0].Device = "changed"
	*retained.Members[nodeID].RDMANICs[0].NUMANode = 9
	delete(retained.Members, nodeID)

	if accepted[nodeID].RDMANICs[0].Device != "nic" || *accepted[nodeID].RDMANICs[0].NUMANode != 1 {
		t.Fatal("result aliases nested accepted history")
	}
}

func TestReconcileRecoveryIsUIDBoundAndSiteIsCurrent(t *testing.T) {
	input := observedInput()
	input.Nodes[0].Labels = map[string]string{machinav1.MachineSiteLabelKey: "old-site"}

	initial, err := members.Reconcile(input, nil)
	if err != nil {
		t.Fatal(err)
	}

	saved, err := json.Marshal(initial.Members[nodeID])
	if err != nil {
		t.Fatal(err)
	}

	input.PodsByNode = nil
	input.Nodes[0].Annotations = map[string]string{members.AdmittedMemberAnnotation: string(saved), wire.SharesAnnotation: "invalid"}
	input.Nodes[0].Labels = nil

	recovered, err := members.Reconcile(input, nil)
	if err != nil || len(recovered.Members) != 1 || recovered.Members[nodeID].Site != "" || recovered.Members[nodeID].PeerEndpoint != "192.0.2.1:7443" {
		t.Fatalf("recovery retained stale site or lost endpoint: %+v, %v", recovered, err)
	}

	input.Nodes[0].UID = otherID

	recreated, err := members.Reconcile(input, nil)
	if err != nil || len(recreated.Members) != 0 {
		t.Fatalf("recreated Node inherited history: %+v, %v", recreated, err)
	}

	input.Nodes[0].UID = nodeID
	input.Nodes[0].Annotations[members.AdmittedMemberAnnotation] = "malformed"

	invalid, err := members.Reconcile(input, nil)
	if err != nil || len(invalid.Members) != 0 {
		t.Fatalf("malformed recovery admitted: %+v, %v", invalid, err)
	}
}

func TestReconcileRejectsWholeInputAndOrdersDiagnostics(t *testing.T) {
	input := observedInput()
	input.Nodes = append(input.Nodes, input.Nodes[0])

	result, err := members.Reconcile(input, nil)
	if !errors.Is(err, wire.InvalidRequest) || result.Members != nil || result.Diagnostics != nil {
		t.Fatalf("partial result escaped: %+v, %v", result, err)
	}

	input.Nodes[1] = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: otherID}}
	input.PodsByNode = nil

	first, err := members.Reconcile(input, nil)
	if err != nil || len(first.Diagnostics) != 2 || first.Diagnostics[0].Object != "node" || first.Diagnostics[1].Object != "other" {
		t.Fatalf("unexpected diagnostics: %+v, %v", first, err)
	}

	slices.Reverse(input.Nodes)

	second, err := members.Reconcile(input, nil)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("order changed result: %+v, %v", second, err)
	}

	input.PeerPort = 0
	if _, err := members.Reconcile(input, nil); !errors.Is(err, wire.InvalidRequest) {
		t.Fatalf("zero port admitted: %v", err)
	}
}

func TestObservedOwnershipAndEndpoint(t *testing.T) {
	input := observedInput()

	pod := input.PodsByNode["node"][0]
	if !input.Ownership.Owns(&pod) || input.Ownership.Owns(nil) {
		t.Fatal("observed ownership mismatch")
	}

	for _, identity := range []members.WorkloadIdentity{{Name: "dataplane"}, {Name: "other", UID: "workload-uid"}, {Name: "dataplane", UID: "recreated"}} {
		ownership := input.Ownership

		ownership.Workloads[0] = identity
		if ownership.Owns(&pod) {
			t.Fatalf("unobserved identity admitted: %+v", identity)
		}
	}

	// Both configured workloads are eligible; equal timestamps break ties by UID.
	input.Ownership.Workloads[1] = members.WorkloadIdentity{Name: "podnet", UID: "podnet-uid"}
	newer := pod.DeepCopy()
	newer.UID = "z"
	newer.OwnerReferences[0].Name, newer.OwnerReferences[0].UID = "podnet", "podnet-uid"
	newer.Status.PodIP = "2001:db8::1"

	endpoint, err := members.SelectEndpoint([]corev1.Pod{*newer, pod}, input.Ownership, "node", 7443)
	if err != nil || endpoint != "[2001:db8::1]:7443" {
		t.Fatalf("mixed workload endpoint: %q, %v", endpoint, err)
	}

	newer.Status.Phase = corev1.PodSucceeded

	endpoint, err = members.SelectEndpoint([]corev1.Pod{*newer, pod}, input.Ownership, "node", 7443)
	if err != nil || endpoint != "192.0.2.1:7443" {
		t.Fatalf("terminal endpoint admitted: %q, %v", endpoint, err)
	}
}

func TestCatalogOrderingAndWholeCandidateValidation(t *testing.T) {
	volumes := []racerv1.ClusterVolume{
		{ObjectMeta: metav1.ObjectMeta{Name: "cache-b", UID: types.UID(otherID)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}},
		{ObjectMeta: metav1.ObjectMeta{Name: "cache-a", UID: types.UID(nodeID)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}},
	}

	catalog, err := members.BuildCatalog(volumes)
	if err != nil || len(catalog) != 2 || catalog[0].ID != nodeID || catalog[0].ClientSocket != "/run/racer/cache-a/client/socket" || volumes[0].Name != "cache-b" {
		t.Fatalf("catalog order or input mutation: %+v, %v", catalog, err)
	}

	volumes[1].Name = "../invalid"

	catalog, err = members.BuildCatalog(volumes)
	if !errors.Is(err, wire.InvalidRequest) || catalog != nil {
		t.Fatalf("partial catalog escaped: %+v, %v", catalog, err)
	}
}

func TestCatalogSkipsNonCacheVolumesBeforeValidation(t *testing.T) {
	valid := racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: types.UID(nodeID)}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}

	for _, volumeType := range []racerv1.ClusterVolumeType{"", "Future", "cache"} {
		t.Run(string(volumeType), func(t *testing.T) {
			invalid := racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "../invalid", UID: "invalid"}, Spec: racerv1.ClusterVolumeSpec{Type: volumeType}}
			duplicate := valid
			duplicate.Spec.Type = volumeType

			catalog, err := members.BuildCatalog([]racerv1.ClusterVolume{invalid, duplicate, valid, duplicate})
			if err != nil || len(catalog) != 1 || catalog[0].ID != nodeID {
				t.Fatalf("non-Cache volume affected catalog: %+v, %v", catalog, err)
			}

			catalog, err = members.BuildCatalog([]racerv1.ClusterVolume{invalid, duplicate, {}})
			if err != nil || catalog == nil || len(catalog) != 0 {
				t.Fatalf("non-Cache-only catalog: %+v, %v", catalog, err)
			}
		})
	}
}
