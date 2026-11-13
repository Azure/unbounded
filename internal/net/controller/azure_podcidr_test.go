// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	netv1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestAzureCIDRAdvertisementAndWithdrawal(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a", UID: "current-node",
			Labels:      map[string]string{"kubernetes.azure.com/ebpf-dataplane": "cilium"},
			Annotations: map[string]string{WireGuardPubKeyAnnotation: "pub"},
		},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.238.0.4"}}},
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"networkContainers": []any{map[string]any{
			"type": "overlay", "assignmentMode": "static", "nodeIP": "10.238.0.4",
			"primaryIP": "10.248.1.0/24", "primaryIPV6": "fd48::100/120",
		}}},
	}}
	object.SetName(node.Name)
	object.SetNamespace("kube-system")
	object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}})

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)

	if err := store.Add(object); err != nil {
		t.Fatal(err)
	}

	sc := &SiteController{azurePodCIDRInformer: &fakeInformer{store: store}}
	want := []string{"10.248.1.0/24", "fd48::100/120"}

	if info := sc.buildNodeInfo(node); !slices.Equal(info.PodCIDRs, want) {
		t.Fatalf("Azure assignment missing from SiteNodeSlice entry: %#v", info)
	}

	pool := &netv1.GatewayPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: netv1.SchemeGroupVersion.String(), Kind: "GatewayPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "azure"},
		Spec:       netv1.GatewayPoolSpec{Type: gatewayPoolTypeInternal},
	}
	poolObject := toGatewayPoolUnstructured(t, pool)

	poolStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := poolStore.Add(poolObject); err != nil {
		t.Fatal(err)
	}

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(node); err != nil {
		t.Fatal(err)
	}

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()

	gc := &GatewayPoolController{
		clientset: fake.NewClientset(node),
		dynamicClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			gatewayNodeGVR: "GatewayPoolNodeList",
		}, poolObject),
		azurePodCIDRInformer: sc.azurePodCIDRInformer,
		nodeLister:           corelisters.NewNodeLister(nodeIndexer),
		gatewayPoolInformer:  &fakeInformer{store: poolStore},
		gatewayNodeInformer:  &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc)},
		workqueue:            queue,
		gwPortAllocated:      make(map[int32]string),
		gwPortByNode:         make(map[string]int32),
	}

	if err := gc.syncPool(t.Context(), pool.Name); err != nil {
		t.Fatal(err)
	}

	current, err := gc.dynamicClient.Resource(gatewayPoolGVR).Get(t.Context(), pool.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	advertised, err := parseGatewayPool(current)
	if err != nil || len(advertised.Status.Nodes) != 1 {
		t.Fatalf("GatewayPool advertisement: %#v, %v", advertised, err)
	}

	if got := advertised.Status.Nodes[0]; !slices.Equal(got.PodCIDRs, want) ||
		!slices.Equal(got.HealthEndpoints, []string{"10.248.1.1", "fd48::101"}) {
		t.Fatalf("GatewayPool did not use Azure CIDRs: %#v", got)
	}

	if err := poolStore.Update(current); err != nil {
		t.Fatal(err)
	}

	object.SetOwnerReferences(nil)

	if err := store.Update(object); err != nil {
		t.Fatal(err)
	}

	if info := sc.buildNodeInfo(node); len(info.PodCIDRs) != 0 {
		t.Fatalf("stale NNC advertised: %#v", info)
	}

	if err := gc.syncPool(t.Context(), pool.Name); err != nil {
		t.Fatal(err)
	}

	current, err = gc.dynamicClient.Resource(gatewayPoolGVR).Get(t.Context(), pool.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	advertised, err = parseGatewayPool(current)
	if err != nil || len(advertised.Status.Nodes) != 0 {
		t.Fatalf("stale gateway assignment retained: %#v, %v", advertised, err)
	}
}

func TestAzureCIDREventsDirtyOnlyOwningSite(t *testing.T) {
	sc := newIPMatchedSliceTestController()
	defer sc.workqueue.ShutDown()

	node := sliceTestNode("10.1.0.5", map[string]string{"kubernetes.azure.com/ebpf-dataplane": "cilium"})

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(node); err != nil {
		t.Fatal(err)
	}

	sc.nodeLister = corelisters.NewNodeLister(indexer)
	object := &unstructured.Unstructured{}
	object.SetName(node.Name)
	object.SetNamespace("kube-system")

	for _, event := range []any{object, cache.DeletedFinalStateUnknown{Key: "kube-system/" + node.Name, Obj: object}} {
		sc.handleAzurePodCIDRChange(event)

		if got := sliceWorkSites(t, sc); !slices.Equal(got, []string{"site-a"}) {
			t.Fatalf("Azure CIDR event dirtied unrelated Sites: %v", got)
		}
	}

	object.SetNamespace("default")
	sc.handleAzurePodCIDRChange(object)

	if sc.slicesDirty.Load() {
		t.Fatal("foreign namespace dirtied SiteNodeSlices")
	}
}
