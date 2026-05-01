// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

// testInformer is a minimal cache.SharedIndexInformer for tests that only need GetStore().
type testInformer struct {
	cache.SharedIndexInformer
	store cache.Store
}

func (f *testInformer) GetStore() cache.Store { return f.store }
func (f *testInformer) HasSynced() bool       { return true }

func toUnstructured(t *testing.T, obj interface{}) *unstructured.Unstructured {
	t.Helper()

	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}

	m := map[string]interface{}{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal object to map: %v", err)
	}

	return &unstructured.Unstructured{Object: m}
}

// TestParseSiteSliceAndGatewayPool tests ParseSiteSliceAndGatewayPool.
func TestParseSiteSliceAndGatewayPool(t *testing.T) {
	siteSrc := &unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: unboundednetv1alpha1.SiteSpec{
			NodeCidrs: []string{"10.0.0.0/16"},
		},
	}

	site, err := parseSite(toUnstructured(t, siteSrc))
	if err != nil {
		t.Fatalf("parseSite() error = %v", err)
	}

	if site.Name != "site-a" || len(site.Spec.NodeCidrs) != 1 {
		t.Fatalf("unexpected site parse result: %#v", site)
	}

	sliceSrc := &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		SiteName:   "site-a",
		SliceIndex: 1,
		Nodes: []unboundednetv1alpha1.NodeInfo{
			{Name: "node-1", InternalIPs: []string{"10.0.0.10"}},
		},
	}

	slice, err := parseSiteNodeSlice(toUnstructured(t, sliceSrc))
	if err != nil {
		t.Fatalf("parseSiteNodeSlice() error = %v", err)
	}

	if slice.SiteName != "site-a" || len(slice.Nodes) != 1 || slice.Nodes[0].Name != "node-1" {
		t.Fatalf("unexpected SiteNodeSlice parse result: %#v", slice)
	}

	poolSrc := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         "Internal",
			NodeSelector: map[string]string{"role": "gateway"},
			RoutedCidrs:  []string{"100.64.0.0/16"},
		},
	}

	pool, err := parseGatewayPool(toUnstructured(t, poolSrc))
	if err != nil {
		t.Fatalf("parseGatewayPool() error = %v", err)
	}

	if pool.Name != "pool-a" || pool.Spec.Type != "Internal" || len(pool.Spec.RoutedCidrs) != 1 {
		t.Fatalf("unexpected gateway pool parse result: %#v", pool)
	}
}

// TestNormalizeGatewayPoolType tests NormalizeGatewayPoolType.
func TestNormalizeGatewayPoolType(t *testing.T) {
	if got := normalizeGatewayPoolType(""); got != gatewayPoolTypeExternal {
		t.Fatalf("expected empty type to normalize to External, got %q", got)
	}

	if got := normalizeGatewayPoolType(gatewayPoolTypeInternal); got != gatewayPoolTypeInternal {
		t.Fatalf("expected Internal type to be preserved, got %q", got)
	}
}

// TestBuildGatewayPoolRoutedCIDRs tests BuildGatewayPoolRoutedCIDRs.
func TestBuildGatewayPoolRoutedCIDRs(t *testing.T) {
	fallback := []string{"100.64.0.0/16", "100.65.0.0/16"}
	if got := buildGatewayPoolRoutedCIDRs(nil, fallback); len(got) != 0 {
		t.Fatalf("expected nil pool to return empty result, got %#v", got)
	}

	pool := &unboundednetv1alpha1.GatewayPool{
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			RoutedCidrs: []string{"100.65.0.0/16", "100.66.0.0/16"},
		},
	}

	got := buildGatewayPoolRoutedCIDRs(pool, fallback)
	if len(got) != 3 {
		t.Fatalf("expected deduped merged CIDRs, got %#v", got)
	}
}

// TestBuildGatewayNodeRoutesForStatus tests BuildGatewayNodeRoutesForStatus.
func TestBuildGatewayNodeRoutesForStatus(t *testing.T) {
	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			RoutedCidrs: []string{"100.64.0.0/16", ""},
		},
	}
	site := &unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: unboundednetv1alpha1.SiteSpec{
			NodeCidrs: []string{"10.0.0.0/16"},
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
				{CidrBlocks: []string{"10.244.0.0/16", ""}},
			},
		},
	}

	routes := buildGatewayNodeRoutesForStatus(pool, site)
	if len(routes) != 3 {
		t.Fatalf("expected three non-empty routes, got %#v", routes)
	}

	if routes["100.64.0.0/16"].Type != "RoutedCidr" {
		t.Fatalf("expected routed CIDR type")
	}

	if routes["10.0.0.0/16"].Type != "NodeCidr" {
		t.Fatalf("expected node CIDR type")
	}

	if routes["10.244.0.0/16"].Type != "PodCidr" {
		t.Fatalf("expected pod CIDR type")
	}

	for cidr, route := range routes {
		if len(route.Paths) != 1 || len(route.Paths[0]) != 2 {
			t.Fatalf("expected full path for %s, got %#v", cidr, route.Paths)
		}

		if route.Paths[0][0].Type != "Site" || route.Paths[0][0].Name != "site-a" {
			t.Fatalf("unexpected first hop for %s: %#v", cidr, route.Paths[0])
		}

		if route.Paths[0][1].Type != "GatewayPool" || route.Paths[0][1].Name != "pool-a" {
			t.Fatalf("unexpected second hop for %s: %#v", cidr, route.Paths[0])
		}
	}
}

// TestResolveGatewayPoolSiteName tests ResolveGatewayPoolSiteName.
func TestResolveGatewayPoolSiteName(t *testing.T) {
	pool := &unboundednetv1alpha1.GatewayPool{}

	if got := resolveGatewayPoolSiteName("site-local", "pub-self", pool); got != "site-local" {
		t.Fatalf("expected mySiteName to win, got %q", got)
	}

	pool.Status.Nodes = []unboundednetv1alpha1.GatewayNodeInfo{{
		Name:               "gw-a",
		WireGuardPublicKey: "pub-self",
		SiteName:           "site-a",
	}}
	if got := resolveGatewayPoolSiteName("", "pub-self", pool); got != "site-a" {
		t.Fatalf("expected gateway pool node membership site fallback, got %q", got)
	}

	pool.Status.Nodes = nil
	if got := resolveGatewayPoolSiteName("", "pub-self", pool); got != "" {
		t.Fatalf("expected empty site when no fallback available, got %q", got)
	}
}

// TestSyncGatewayNodeRoutesStatusEarlyReturn tests SyncGatewayNodeRoutesStatusEarlyReturn.
func TestSyncGatewayNodeRoutesStatusEarlyReturn(t *testing.T) {
	if err := syncGatewayNodeRoutesStatus(context.TODO(), nil, nil, "node-a", nil); err != nil {
		t.Fatalf("expected nil client to no-op, got %v", err)
	}

	if err := syncGatewayNodeRoutesStatus(context.TODO(), nil, nil, "", nil); err != nil {
		t.Fatalf("expected empty node name to no-op, got %v", err)
	}
}

func fakeGatewayDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	for _, obj := range objects {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			if u.GetAPIVersion() == "" {
				u.SetAPIVersion("net.unbounded-cloud.io/v1alpha1")
			}

			if u.GetKind() == "" {
				u.SetKind("GatewayPoolNode")
			}
		}
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			gatewayNodeGVR: "GatewayPoolNodeList",
		},
		objects...,
	)
}

// TestPatchGatewayNodeStatusPaths tests PatchGatewayNodeStatusPaths.
func TestPatchGatewayNodeStatusPaths(t *testing.T) {
	base := toUnstructured(t, &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
	})
	patch := map[string]interface{}{
		"status": map[string]interface{}{
			"routes": map[string]interface{}{
				"10.0.0.0/16": map[string]interface{}{
					"type": "RoutedCidr",
				},
			},
		},
	}

	t.Run("status subresource succeeds", func(t *testing.T) {
		dyn := fakeGatewayDynamic(base.DeepCopy())
		if err := patchGatewayNodeStatus(context.Background(), dyn, "gw-a", patch); err != nil {
			t.Fatalf("patchGatewayNodeStatus() error = %v", err)
		}

		got, err := dyn.Resource(gatewayNodeGVR).Get(context.Background(), "gw-a", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("failed to get patched object: %v", err)
		}

		if _, ok, _ := unstructured.NestedMap(got.Object, "status", "routes"); !ok {
			t.Fatalf("expected status.routes to be patched")
		}
	})

	t.Run("fallback patch succeeds", func(t *testing.T) {
		dyn := fakeGatewayDynamic(base.DeepCopy())
		dyn.PrependReactor("patch", "gatewaypoolnodes", func(action ktesting.Action) (bool, runtime.Object, error) {
			if pa, ok := action.(ktesting.PatchAction); ok && pa.GetSubresource() == "status" {
				return true, nil, fmt.Errorf("status subresource unsupported")
			}

			return false, nil, nil
		})

		if err := patchGatewayNodeStatus(context.Background(), dyn, "gw-a", patch); err != nil {
			t.Fatalf("expected fallback patch success, got %v", err)
		}
	})

	t.Run("fallback not found returned", func(t *testing.T) {
		dyn := fakeGatewayDynamic()
		dyn.PrependReactor("patch", "gatewaypoolnodes", func(action ktesting.Action) (bool, runtime.Object, error) {
			if pa, ok := action.(ktesting.PatchAction); ok && pa.GetSubresource() == "status" {
				return true, nil, fmt.Errorf("status subresource unsupported")
			}

			return false, nil, nil
		})

		if err := patchGatewayNodeStatus(context.Background(), dyn, "missing-node", patch); err == nil {
			t.Fatalf("expected not-found fallback to be returned")
		}
	})

	t.Run("fallback error returned", func(t *testing.T) {
		dyn := fakeGatewayDynamic(base.DeepCopy())
		dyn.PrependReactor("patch", "gatewaypoolnodes", func(action ktesting.Action) (bool, runtime.Object, error) {
			pa, ok := action.(ktesting.PatchAction)
			if !ok {
				return false, nil, nil
			}

			if pa.GetSubresource() == "status" {
				return true, nil, fmt.Errorf("status subresource unsupported")
			}

			return true, nil, fmt.Errorf("fallback failed")
		})

		if err := patchGatewayNodeStatus(context.Background(), dyn, "gw-a", patch); err == nil {
			t.Fatalf("expected fallback error to be returned")
		}
	})
}

// TestSyncGatewayNodeRoutesStatus tests SyncGatewayNodeRoutesStatus.
func TestSyncGatewayNodeRoutesStatus(t *testing.T) {
	dyn := fakeGatewayDynamic(toUnstructured(t, &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
	}))

	routes := map[string]unboundednetv1alpha1.GatewayNodeRoute{
		"10.244.0.0/16": {Type: "RoutedCidr"},
	}
	if err := syncGatewayNodeRoutesStatus(context.Background(), dyn, nil, "gw-a", routes); err != nil {
		t.Fatalf("syncGatewayNodeRoutesStatus() error = %v", err)
	}

	got, err := dyn.Resource(gatewayNodeGVR).Get(context.Background(), "gw-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get gateway node: %v", err)
	}

	if _, ok, _ := unstructured.NestedString(got.Object, "status", "lastUpdated"); !ok {
		t.Fatalf("expected status.lastUpdated to be set")
	}

	if _, ok, _ := unstructured.NestedMap(got.Object, "status", "routes"); !ok {
		t.Fatalf("expected status.routes to be set")
	}
}

// TestSyncGatewayNodeRoutesStatusRemovesStaleRoutes verifies that CIDRs
// present in the existing GatewayPoolNode status but absent from the new
// routes map are explicitly removed (set to null in the MergePatch).
func TestSyncGatewayNodeRoutesStatusRemovesStaleRoutes(t *testing.T) {
	// Seed the GatewayPoolNode with two existing routes in its status.
	existing := toUnstructured(t, &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
	})

	err := unstructured.SetNestedField(existing.Object, map[string]interface{}{
		"10.0.0.0/16": map[string]interface{}{
			"type":   "NodeCidr",
			"source": "site-a",
		},
		"10.244.0.0/16": map[string]interface{}{
			"type":   "PodCidr",
			"source": "site-a",
		},
	}, "status", "routes")
	if err != nil {
		t.Fatalf("failed to set nested field: %v", err)
	}

	dyn := fakeGatewayDynamic(existing)

	// Build an informer cache containing the existing object so the stale route
	// detection can find routes to remove.
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = indexer.Add(existing)
	fakeInf := &testInformer{store: indexer}

	// Sync with a new routes map that only contains one of the two CIDRs.
	// The stale "10.0.0.0/16" route should be removed.
	newRoutes := map[string]unboundednetv1alpha1.GatewayNodeRoute{
		"10.244.0.0/16": {Type: "PodCidr"},
	}
	if err := syncGatewayNodeRoutesStatus(context.Background(), dyn, fakeInf, "gw-a", newRoutes); err != nil {
		t.Fatalf("syncGatewayNodeRoutesStatus() error = %v", err)
	}

	got, err := dyn.Resource(gatewayNodeGVR).Get(context.Background(), "gw-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get gateway node: %v", err)
	}

	routes, ok, _ := unstructured.NestedMap(got.Object, "status", "routes")
	if !ok {
		t.Fatalf("expected status.routes to be present")
	}

	// The surviving route must still be present.
	if _, ok := routes["10.244.0.0/16"]; !ok {
		t.Fatalf("expected 10.244.0.0/16 route to remain, got %v", routes)
	}

	// The stale route must have been removed (set to null in the patch).
	if _, ok := routes["10.0.0.0/16"]; ok {
		t.Fatalf("expected stale route 10.0.0.0/16 to be removed, but it still exists: %v", routes)
	}
}

// TestPatchGatewayNodeStatusFallbackNotFoundDetection tests PatchGatewayNodeStatusFallbackNotFoundDetection.
func TestPatchGatewayNodeStatusFallbackNotFoundDetection(t *testing.T) {
	dyn := fakeGatewayDynamic(toUnstructured(t, &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
	}))
	dyn.PrependReactor("patch", "gatewaypoolnodes", func(action ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(ktesting.PatchAction)
		if !ok {
			return false, nil, nil
		}

		if pa.GetSubresource() == "status" {
			return true, nil, fmt.Errorf("status subresource unsupported")
		}

		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: gatewayNodeGVR.Group, Resource: gatewayNodeGVR.Resource}, "gw-a")
	})

	if err := patchGatewayNodeStatus(context.Background(), dyn, "gw-a", map[string]interface{}{"status": map[string]interface{}{"routes": map[string]interface{}{}}}); err == nil {
		t.Fatalf("expected not found fallback to return an error")
	}
}
