// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestDeletedNodeForcesTransitionCleanup(t *testing.T) {
	oldCleanup := cleanupNodeNetworkingOnShutdownFunc

	t.Cleanup(func() { cleanupNodeNetworkingOnShutdownFunc = oldCleanup })

	called := false
	cleanupNodeNetworkingOnShutdownFunc = func(_ *config, _ *wireGuardState, force bool) {
		called = true

		if !force {
			t.Error("Node deletion did not force networking cleanup")
		}
	}
	nodeClient := fake.NewClientset()
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gatewayNodeGVR.GroupVersion().String(), "kind": "GatewayPoolNode",
		"metadata": map[string]any{"name": "deleted"},
		"status":   map[string]any{"routes": map[string]any{"10.0.0.0/24": map[string]any{"type": "NodeCidr"}}},
	}})
	state := &wireGuardState{
		clientset: nodeClient, nodeName: "deleted", manageCniPlugin: true,
		adoptedAssignment: nodeAssignment{site: "old", gatewayPool: "pool-g", nodeUID: "old-uid"},
	}
	cleanupForAssignmentTransition(&config{}, state, client, nil)

	if !called {
		t.Fatal("network cleanup was not invoked")
	}

	for _, action := range nodeClient.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("mutated missing Node: %v", action)
		}
	}

	for _, action := range client.Actions() {
		if action.GetResource() != gatewayNodeGVR {
			t.Fatalf("looked up destination after Node deletion: %v", action)
		}
	}

	got, err := client.Resource(gatewayNodeGVR).Get(t.Context(), "deleted", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, found, err := unstructured.NestedMap(got.Object, "status", "routes"); err != nil || found {
		t.Fatalf("deleted Node still advertises routes: %v err=%v", got.Object, err)
	}
}

func TestDisableCNIForAssignmentTransition(t *testing.T) {
	for _, tc := range []struct {
		name         string
		managed      bool
		destination  string
		unmanaged    bool
		missing      bool
		renameFails  bool
		wantDisabled bool
		wantErr      bool
	}{
		{name: "managed to unmanaged", managed: true, destination: "new", unmanaged: true, wantDisabled: true},
		{name: "managed to managed", managed: true, destination: "new"},
		{name: "unmanaged source", destination: "new", unmanaged: true},
		{name: "same site", managed: true, destination: "old", unmanaged: true},
		{name: "no destination", managed: true},
		{name: "unknown destination", managed: true, destination: "new", missing: true, wantErr: true},
		{name: "rename failure", managed: true, destination: "new", unmanaged: true, renameFails: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config{CNIConfDir: t.TempDir(), CNIConfFile: "10-unbounded.conflist"}

			active := filepath.Join(cfg.CNIConfDir, cfg.CNIConfFile)
			if err := os.WriteFile(active, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}

			if tc.renameFails {
				// A directory at the snapshot path is not a replaceable managed file.
				if err := os.Mkdir(cniDisabledPath(cfg), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			objects := []runtime.Object{}

			if !tc.missing && tc.destination != "" {
				managed := !tc.unmanaged
				objects = append(objects, toUnstructuredSiteWatch(t, &unboundedv1alpha3.Site{
					TypeMeta:   metav1.TypeMeta{APIVersion: siteGVR.GroupVersion().String(), Kind: "Site"},
					ObjectMeta: metav1.ObjectMeta{Name: tc.destination},
					Spec:       unboundedv1alpha3.SiteSpec{ManageCniPlugin: &managed},
				}))
			}

			client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
			state := &wireGuardState{manageCniPlugin: tc.managed, adoptedAssignment: nodeAssignment{site: "old"}}

			err := disableCNIForAssignmentTransition(t.Context(), cfg, state, client, nodeAssignment{site: tc.destination})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tc.wantErr)
			}

			data, err := os.ReadFile(active)
			if tc.wantDisabled {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("active conflist remains: %v", err)
				}

				data, err = os.ReadFile(cniDisabledPath(cfg))
			}

			if err != nil || string(data) != "original" {
				t.Fatalf("conflist contents = %q, err=%v", data, err)
			}
		})
	}
}

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

	sites := []*unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-default"}, Spec: unboundedv1alpha3.SiteSpec{ManageCniPlugin: nil}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-true"}, Spec: unboundedv1alpha3.SiteSpec{ManageCniPlugin: &trueVal}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-false"}, Spec: unboundedv1alpha3.SiteSpec{ManageCniPlugin: &falseVal}},
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
