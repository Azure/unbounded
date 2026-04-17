// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
)

func toUnstructuredSiteWatch(t *testing.T, v interface{}) *unstructured.Unstructured {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	return &unstructured.Unstructured{Object: obj}
}

func newTestInformer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
}

// TestFindMySiteFromCRDsAndGatewayDetection tests FindMySiteFromCRDsAndGatewayDetection.
func TestFindMySiteFromCRDsAndGatewayDetection(t *testing.T) {
	sliceInformer := newTestInformer()
	gatewayPoolInformer := newTestInformer()

	slice := &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		SiteName:   "site-a",
		Nodes: []unboundednetv1alpha1.NodeInfo{
			{Name: "node-a", WireGuardPublicKey: "pub-a"},
		},
	}
	if err := sliceInformer.GetStore().Add(toUnstructuredSiteWatch(t, slice)); err != nil {
		t.Fatalf("add slice failed: %v", err)
	}

	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{
			{Name: "gw-a", WireGuardPublicKey: "pub-gw", SiteName: "site-gw"},
		}},
	}
	if err := gatewayPoolInformer.GetStore().Add(toUnstructuredSiteWatch(t, pool)); err != nil {
		t.Fatalf("add pool failed: %v", err)
	}

	if got := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, "pub-a"); got != "site-a" {
		t.Fatalf("unexpected site from slices: %q", got)
	}

	if got := findMySiteFromCRDs(sliceInformer, gatewayPoolInformer, "pub-gw"); got != "site-gw" {
		t.Fatalf("unexpected site from gateway pool: %q", got)
	}

	if !isGatewayNodeFromCRDs(gatewayPoolInformer, "pub-gw") {
		t.Fatalf("expected pub-gw to be detected as gateway node")
	}

	if isGatewayNodeFromCRDs(gatewayPoolInformer, "unknown") {
		t.Fatalf("did not expect unknown key to be detected as gateway node")
	}
}

// TestWaitForSiteMembershipImmediate tests WaitForSiteMembershipImmediate.
func TestWaitForSiteMembershipImmediate(t *testing.T) {
	sliceInformer := newTestInformer()
	gatewayPoolInformer := newTestInformer()

	slice := &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-a"},
		SiteName:   "site-a",
		Nodes: []unboundednetv1alpha1.NodeInfo{
			{Name: "node-a", WireGuardPublicKey: "pub-a"},
		},
	}
	if err := sliceInformer.GetStore().Add(toUnstructuredSiteWatch(t, slice)); err != nil {
		t.Fatalf("add slice failed: %v", err)
	}

	got, err := waitForSiteMembership(context.Background(), sliceInformer, gatewayPoolInformer, "pub-a")
	if err != nil {
		t.Fatalf("waitForSiteMembership returned error: %v", err)
	}

	if got != "site-a" {
		t.Fatalf("unexpected site membership result: %q", got)
	}
}

// TestGetManageCniPluginFromCRDs tests GetManageCniPluginFromCRDs.
func TestGetManageCniPluginFromCRDs(t *testing.T) {
	siteInformer := newTestInformer()

	trueVal := true
	falseVal := false

	sites := []*unboundednetv1alpha1.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-default"}, Spec: unboundednetv1alpha1.SiteSpec{ManageCniPlugin: nil}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-true"}, Spec: unboundednetv1alpha1.SiteSpec{ManageCniPlugin: &trueVal}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-false"}, Spec: unboundednetv1alpha1.SiteSpec{ManageCniPlugin: &falseVal}},
	}
	for _, s := range sites {
		if err := siteInformer.GetStore().Add(toUnstructuredSiteWatch(t, s)); err != nil {
			t.Fatalf("add site failed: %v", err)
		}
	}

	if !getManageCniPluginFromCRDs(siteInformer, "") {
		t.Fatalf("expected empty site default to true")
	}

	if !getManageCniPluginFromCRDs(siteInformer, "site-default") {
		t.Fatalf("expected nil ManageCniPlugin default to true")
	}

	if !getManageCniPluginFromCRDs(siteInformer, "site-true") {
		t.Fatalf("expected explicit true to be honored")
	}

	if getManageCniPluginFromCRDs(siteInformer, "site-false") {
		t.Fatalf("expected explicit false to be honored")
	}

	if !getManageCniPluginFromCRDs(siteInformer, "site-missing") {
		t.Fatalf("expected missing site default to true")
	}
}
