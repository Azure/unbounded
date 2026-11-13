// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurepodcidr

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func assignedNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a", UID: "current-node",
			Labels: map[string]string{"kubernetes.azure.com/ebpf-dataplane": "cilium"},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.238.0.4"},
		}},
	}
}

func assignedConfig(node *corev1.Node) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": Resource.GroupVersion().String(), "kind": "NodeNetworkConfig",
		"status": map[string]any{"networkContainers": []any{
			map[string]any{
				"type": "overlay", "assignmentMode": "static", "nodeIP": "10.238.0.4",
				"primaryIP": "10.248.1.0/24", "subnetAddressSpace": "10.248.0.0/16",
			},
			map[string]any{
				"type": "overlay", "assignmentMode": "static", "nodeIP": "10.238.0.4",
				"primaryIP": "fd48::100/120", "subnetAddressSpace": "fd48::/48",
			},
		}},
	}}
	object.SetName(node.Name)
	object.SetNamespace("kube-system")
	object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}})

	return object
}

func TestFromNodeNetworkConfig(t *testing.T) {
	node := assignedNode()

	cidrs, err := FromNodeNetworkConfig(node, assignedConfig(node))
	if err != nil || !slices.Equal(cidrs, []string{"10.248.1.0/24", "fd48::100/120"}) {
		t.Fatalf("per-node dual-stack assignment: %v, %v", cidrs, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
	}{
		{"stale-owner", func(o *unstructured.Unstructured) { o.SetOwnerReferences(nil) }},
		{"wrong-name", func(o *unstructured.Unstructured) { o.SetName("other-node") }},
		{"wrong-namespace", func(o *unstructured.Unstructured) { o.SetNamespace("default") }},
		{"empty", func(o *unstructured.Unstructured) { delete(o.Object, "status") }},
		{"wrong-node-ip", func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]any)["networkContainers"].([]any)[0].(map[string]any)["nodeIP"] = "10.238.0.5"
		}},
		{"host-not-prefix", func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]any)["networkContainers"].([]any)[0].(map[string]any)["primaryIP"] = "10.248.1.4/24"
		}},
		{"wrong-value-type", func(o *unstructured.Unstructured) {
			o.Object["status"].(map[string]any)["networkContainers"].([]any)[0].(map[string]any)["primaryIP"] = int64(24)
		}},
		{"dynamic", func(o *unstructured.Unstructured) {
			for _, value := range o.Object["status"].(map[string]any)["networkContainers"].([]any) {
				value.(map[string]any)["assignmentMode"] = "dynamic"
			}
		}},
		{"non-overlay", func(o *unstructured.Unstructured) {
			for _, value := range o.Object["status"].(map[string]any)["networkContainers"].([]any) {
				value.(map[string]any)["type"] = "vnet"
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := assignedConfig(node)
			test.mutate(object)

			if cidrs, err := FromNodeNetworkConfig(node, object); err == nil || len(cidrs) > 0 {
				t.Fatalf("invalid assignment accepted: %v, %v", cidrs, err)
			}
		})
	}
}
