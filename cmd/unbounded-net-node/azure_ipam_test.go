// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/Azure/unbounded/internal/net/azurepodcidr"
)

func TestDelegatedAzureIPAMVerifiesNodeNetworkConfig(t *testing.T) {
	for _, test := range []struct {
		name       string
		ownerUID   string
		nodeCIDRs  []string
		wantCIDRs  []string
		wantFailed bool
		pending    bool
	}{
		{name: "verified-azure", ownerUID: "current-node", wantCIDRs: []string{"10.248.1.0/24", "fd48::100/120"}},
		{name: "delayed-azure", ownerUID: "current-node", pending: true, wantCIDRs: []string{"10.248.1.0/24", "fd48::100/120"}},
		{name: "stale-azure", ownerUID: "previous-node", wantFailed: true},
		{name: "node-precedence", ownerUID: "previous-node", nodeCIDRs: []string{"10.246.1.0/24"}, wantCIDRs: []string{"10.246.1.0/24"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-a", UID: "current-node",
					Labels: map[string]string{"kubernetes.azure.com/ebpf-dataplane": "cilium"},
				},
				Spec:   corev1.NodeSpec{PodCIDRs: test.nodeCIDRs},
				Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.238.0.4"}}},
			}
			object := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "acn.azure.com/v1alpha", "kind": "NodeNetworkConfig",
				"status": map[string]any{"networkContainers": []any{map[string]any{
					"type": "overlay", "assignmentMode": "static", "nodeIP": "10.238.0.4",
					"primaryIP": "10.248.1.0/24", "primaryIPV6": "fd48::100/120",
				}}},
			}}
			object.SetName(node.Name)
			object.SetNamespace("kube-system")
			object.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Node", Name: node.Name, UID: types.UID(test.ownerUID),
			}})
			client := fake.NewClientset(node)
			dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), object)
			pending := test.pending

			dynamicClient.PrependReactor("get", "nodenetworkconfigs", func(clienttesting.Action) (bool, runtime.Object, error) {
				if pending {
					pending = false
					return true, nil, apierrors.NewNotFound(azurepodcidr.Resource.GroupResource(), node.Name)
				}

				return false, nil, nil
			})

			health := &nodeHealthState{}
			health.markCNIUnmanaged()

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			acquireDelegatedIPAMAssignment(ctx, client, dynamicClient, &config{
				NodeName: node.Name, cniRetryInterval: time.Millisecond,
			}, health)

			_, err := health.getIPAMConfig()
			if test.wantFailed {
				if err == nil || len(health.verifiedIPAMAssignment()) != 0 {
					t.Fatal("stale NNC enabled allocation")
				}

				return
			}

			if err != nil || !slices.Equal(health.verifiedIPAMAssignment(), test.wantCIDRs) {
				t.Fatalf("verified assignment: %v, %v", health.verifiedIPAMAssignment(), err)
			}

			if len(test.nodeCIDRs) > 0 && len(dynamicClient.Actions()) != 0 {
				t.Fatal("explicit Node assignment unnecessarily read Azure NNC")
			}

			for _, action := range dynamicClient.Actions() {
				if action.GetVerb() != "get" {
					t.Fatalf("Azure allocation was mutated: %s", action.GetVerb())
				}
			}

			current, err := client.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			if err != nil || !slices.Equal(current.Spec.PodCIDRs, test.nodeCIDRs) {
				t.Fatalf("managed Node PodCIDRs changed: %v, %v", current, err)
			}
		})
	}
}
