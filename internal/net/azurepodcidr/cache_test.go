// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package azurepodcidr

import (
	"errors"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

func TestOptionalInformerDiscovery(t *testing.T) {
	client := clientfake.NewClientset()
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), 0)

	informer, err := InformerIfAvailable(client.Discovery(), factory)
	if err != nil || informer != nil {
		t.Fatalf("absent optional API: %v, %v", informer, err)
	}

	client.Discovery().(*fake.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: Resource.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: Resource.Resource, Namespaced: true}},
	}}

	informer, err = InformerIfAvailable(client.Discovery(), factory)
	if err != nil || informer == nil || informer != factory.ForResource(Resource).Informer() {
		t.Fatalf("discovered informer: %v, %v", informer, err)
	}

	expected := errors.New("discovery unavailable")

	client.PrependReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, expected
	})

	if _, err := InformerIfAvailable(client.Discovery(), factory); !errors.Is(err, expected) {
		t.Fatalf("discovery failure treated as absent API: %v", err)
	}
}

func TestCachedAssignmentLifecycle(t *testing.T) {
	node := assignedNode()
	object := assignedConfig(node)
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, nil)

	if _, err := FromCache(node, informer); err == nil {
		t.Fatal("missing managed assignment accepted")
	}

	if err := informer.GetStore().Add(object); err != nil {
		t.Fatal(err)
	}

	cidrs, err := FromCache(node, informer)
	if err != nil || !slices.Equal(cidrs, []string{"10.248.1.0/24", "fd48::100/120"}) {
		t.Fatalf("cached assignment: %v, %v", cidrs, err)
	}

	node.UID = "recreated-node"
	if _, err := FromCache(node, informer); err == nil {
		t.Fatal("stale cached assignment accepted for recreated node")
	}

	node.Spec.PodCIDRs = []string{"10.246.2.0/24"}

	cidrs, err = FromCache(node, informer)
	if err != nil || !slices.Equal(cidrs, node.Spec.PodCIDRs) {
		t.Fatalf("explicit Node assignment did not take precedence: %v, %v", cidrs, err)
	}

	cidrs[0] = "10.246.3.0/24"

	if node.Spec.PodCIDRs[0] != "10.246.2.0/24" {
		t.Fatal("cached result aliases Node assignment")
	}

	node.Spec.PodCIDRs = nil
	node.UID = "current-node"

	if err := informer.GetStore().Delete(object); err != nil {
		t.Fatal(err)
	}

	if _, err := FromCache(node, informer); err == nil {
		t.Fatal("deleted managed assignment retained")
	}

	node.Labels = nil
	if cidrs, err := FromCache(node, informer); err != nil || len(cidrs) != 0 {
		t.Fatalf("Azure source used for unmanaged Node: %v, %v", cidrs, err)
	}
}
