// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/allocator"
)

const podCIDRTestNode = "node-a"

type podCIDRTestHarness struct {
	sc      *SiteController
	client  *kubefake.Clientset
	state   *assignmentAllocator
	sites   []unboundedv1alpha3.Site
	cached  *corev1.Node
	patches [][]byte
}

// newPodCIDRTestHarness builds a SiteController whose informer cache holds a
// node without pod CIDRs while the API server holds liveNode. Passing nil for
// liveNode simulates a node that no longer exists on the API server.
func newPodCIDRTestHarness(t *testing.T, liveNode *corev1.Node) *podCIDRTestHarness {
	t.Helper()

	var objects []runtime.Object
	if liveNode != nil {
		objects = append(objects, liveNode)
	}

	client := kubefake.NewSimpleClientset(objects...)

	cached := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: podCIDRTestNode, ResourceVersion: "1"}}

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(cached); err != nil {
		t.Fatalf("add node to informer store: %v", err)
	}

	_, pool, err := net.ParseCIDR("10.244.0.0/16")
	if err != nil {
		t.Fatalf("parse pool: %v", err)
	}

	alloc, err := allocator.NewAllocator([]*net.IPNet{pool}, nil, 24, 64)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	site := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: unboundedv1alpha3.SiteSpec{
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
				CidrBlocks: []string{"10.244.0.0/16"},
			}},
		},
	}

	state := &assignmentAllocator{
		siteName:        site.Name,
		assignmentIndex: 0,
		assignment:      site.Spec.PodCidrAssignments[0],
		allocator:       alloc,
	}

	sc := &SiteController{
		clientset:            client,
		nodeLister:           corev1listers.NewNodeLister(nodeIndexer),
		assignmentAllocators: map[string]*assignmentAllocator{assignmentKey(site.Name, 0): state},
	}
	sc.hasSynced.Store(true)
	sc.allocatorsReady.Store(true)

	h := &podCIDRTestHarness{
		sc:     sc,
		client: client,
		state:  state,
		sites:  []unboundedv1alpha3.Site{site},
		cached: cached,
	}

	client.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		h.patches = append(h.patches, action.(clienttesting.PatchAction).GetPatch())
		return false, nil, nil
	})

	return h
}

func (h *podCIDRTestHarness) failPatches(err error) {
	h.client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	})
}

func liveNodeWithRV(resourceVersion string, podCIDRs ...string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: podCIDRTestNode, ResourceVersion: resourceVersion}}
	if len(podCIDRs) > 0 {
		node.Spec.PodCIDR = podCIDRs[0]
		node.Spec.PodCIDRs = podCIDRs
	}

	return node
}

func decodePatch(t *testing.T, patch []byte) map[string]map[string]interface{} {
	t.Helper()

	var decoded map[string]map[string]interface{}
	if err := json.Unmarshal(patch, &decoded); err != nil {
		t.Fatalf("decode patch %s: %v", patch, err)
	}

	return decoded
}

// TestAssignPodCIDRsUsesLiveNodeWhenCacheIsStale reproduces the requeue race:
// the informer cache has not yet observed this controller's earlier patch, so
// the cached node has no pod CIDRs while the API server already has them.
func TestAssignPodCIDRsUsesLiveNodeWhenCacheIsStale(t *testing.T) {
	for _, withLabel := range []bool{false, true} {
		t.Run(map[bool]string{false: "cidr-only", true: "with-label"}[withLabel], func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("5", "10.244.0.0/24"))

			var err error
			if withLabel {
				err = h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a")
			} else {
				err = h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a")
			}

			if err != nil {
				t.Fatalf("assign pod CIDRs: %v", err)
			}

			if len(h.patches) != 0 {
				t.Fatalf("expected no patch for node that already has pod CIDRs, got %s", h.patches[0])
			}

			if !h.state.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("live node CIDR was not marked allocated")
			}

			if h.state.allocator.IsAllocated("10.244.1.0/24") {
				t.Fatal("a second CIDR was allocated for a node that already has one")
			}
		})
	}
}

func TestAssignPodCIDRsWithLabelPatchesWithResourceVersion(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("7"))

	if err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("assign pod CIDRs: %v", err)
	}

	if len(h.patches) != 1 {
		t.Fatalf("expected one patch, got %d", len(h.patches))
	}

	patch := decodePatch(t, h.patches[0])
	if got := patch["metadata"]["resourceVersion"]; got != "7" {
		t.Fatalf("patch resourceVersion = %v, want live resourceVersion 7", got)
	}

	labels, ok := patch["metadata"]["labels"].(map[string]interface{})
	if !ok {
		t.Fatalf("patch missing labels: %s", h.patches[0])
	}

	for _, key := range siteLabelKeys() {
		if labels[key] != "site-a" {
			t.Fatalf("label %s = %v, want site-a", key, labels[key])
		}
	}

	if got := patch["spec"]["podCIDR"]; got != "10.244.0.0/24" {
		t.Fatalf("patch podCIDR = %v, want 10.244.0.0/24", got)
	}

	got, err := h.client.CoreV1().Nodes().Get(context.Background(), podCIDRTestNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	if got.Spec.PodCIDR != "10.244.0.0/24" || len(got.Spec.PodCIDRs) != 1 || got.Spec.PodCIDRs[0] != "10.244.0.0/24" {
		t.Fatalf("node spec = %+v, want podCIDR 10.244.0.0/24", got.Spec)
	}

	if !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("assigned CIDR was not marked allocated")
	}
}

func TestAssignPodCIDRsWithoutLabelOmitsLabels(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("3"))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("assign pod CIDRs: %v", err)
	}

	if len(h.patches) != 1 {
		t.Fatalf("expected one patch, got %d", len(h.patches))
	}

	patch := decodePatch(t, h.patches[0])
	if _, ok := patch["metadata"]["labels"]; ok {
		t.Fatalf("CIDR-only patch unexpectedly sets labels: %s", h.patches[0])
	}

	if got := patch["metadata"]["resourceVersion"]; got != "3" {
		t.Fatalf("patch resourceVersion = %v, want 3", got)
	}
}

func TestAssignPodCIDRsReleasesOnDefinitivePatchRejection(t *testing.T) {
	gr := schema.GroupResource{Resource: "nodes"}

	cases := map[string]error{
		"conflict":  apierrors.NewConflict(gr, podCIDRTestNode, errors.New("resourceVersion changed")),
		"invalid":   apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, podCIDRTestNode, nil),
		"not-found": apierrors.NewNotFound(gr, podCIDRTestNode),
	}

	for name, patchErr := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
			h.failPatches(patchErr)

			err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a")
			if err == nil {
				t.Fatal("expected patch error")
			}

			if h.state.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("CIDR from a rejected patch was not released")
			}
		})
	}
}

func TestAssignPodCIDRsReleasesOnAmbiguousPatchErrorWhenNotApplied(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(errors.New("connection reset by peer"))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR was not released after the assignment failed and the node does not hold it")
	}
}

func TestAssignPodCIDRsKeepsAllocationWhenFailedPatchWasApplied(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	applyPatch := clienttesting.ObjectReaction(h.client.Tracker())
	h.client.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		// Apply the patch, then report a timeout as if the response was lost.
		if _, _, err := applyPatch(action); err != nil {
			t.Errorf("apply patch: %v", err)
		}

		return true, nil, apierrors.NewTimeoutError("response lost", 1)
	})

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	if !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR held by the node was released after a lost patch response")
	}
}

func TestAssignPodCIDRsAllocatesOnceAcrossExhaustedConflictRetries(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, podCIDRTestNode, errors.New("resourceVersion changed")))

	err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a")
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict after retries are exhausted, got %v", err)
	}

	if len(h.patches) < 2 {
		t.Fatalf("expected conflicts to be retried, got %d patch attempts", len(h.patches))
	}

	for i, raw := range h.patches {
		if got := decodePatch(t, raw)["spec"]["podCIDR"]; got != "10.244.0.0/24" {
			t.Fatalf("patch attempt %d podCIDR = %v, want the single allocation 10.244.0.0/24", i, got)
		}
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR was not released after conflict retries were exhausted")
	}
}

func TestAssignPodCIDRsLiveNodeMissing(t *testing.T) {
	h := newPodCIDRTestHarness(t, nil)

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("expected nil error for deleted node, got %v", err)
	}

	if len(h.patches) != 0 {
		t.Fatalf("expected no patch for deleted node, got %s", h.patches[0])
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR was allocated for a deleted node")
	}
}

func TestAssignPodCIDRsLiveGetError(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("2"))
	h.client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})

	if err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected error when the live node read fails")
	}

	if len(h.patches) != 0 {
		t.Fatalf("expected no patch after failed live read, got %s", h.patches[0])
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR was allocated after failed live read")
	}
}

// failFirstPatchWithConflict makes the first patch fail with a conflict. If
// concurrentCIDR is non-empty, it is written to the live node before the
// conflict is returned, simulating another writer assigning pod CIDRs.
func (h *podCIDRTestHarness) failFirstPatchWithConflict(t *testing.T, concurrentCIDR string) {
	t.Helper()

	attempts := 0

	h.client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts > 1 {
			return false, nil, nil
		}

		node, err := h.client.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, "", podCIDRTestNode)
		if err != nil {
			t.Errorf("get node from tracker: %v", err)
			return true, nil, err
		}

		updated := node.(*corev1.Node).DeepCopy()
		updated.ResourceVersion = "9"

		if concurrentCIDR != "" {
			updated.Spec.PodCIDR = concurrentCIDR
			updated.Spec.PodCIDRs = []string{concurrentCIDR}
		}

		if err := h.client.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, updated, ""); err != nil {
			t.Errorf("update node in tracker: %v", err)
			return true, nil, err
		}

		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, podCIDRTestNode, errors.New("resourceVersion changed"))
	})
}

func TestAssignPodCIDRsRetriesOnConflict(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failFirstPatchWithConflict(t, "")

	if err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("expected conflict to be retried, got %v", err)
	}

	if len(h.patches) != 2 {
		t.Fatalf("expected two patch attempts, got %d", len(h.patches))
	}

	if got := decodePatch(t, h.patches[0])["metadata"]["resourceVersion"]; got != "4" {
		t.Fatalf("first patch resourceVersion = %v, want 4", got)
	}

	if got := decodePatch(t, h.patches[1])["metadata"]["resourceVersion"]; got != "9" {
		t.Fatalf("retry patch resourceVersion = %v, want re-read resourceVersion 9", got)
	}

	if got := decodePatch(t, h.patches[1])["spec"]["podCIDR"]; got != "10.244.0.0/24" {
		t.Fatalf("retry patch podCIDR = %v, want original allocation 10.244.0.0/24 reused", got)
	}

	if !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("assigned CIDR is not marked allocated after the retry")
	}

	if h.state.allocator.IsAllocated("10.244.1.0/24") {
		t.Fatal("conflict retry allocated an extra CIDR")
	}

	node, err := h.client.CoreV1().Nodes().Get(context.Background(), podCIDRTestNode, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}

	if node.Spec.PodCIDR != "10.244.0.0/24" {
		t.Fatalf("node podCIDR = %q, want 10.244.0.0/24", node.Spec.PodCIDR)
	}
}

func TestAssignPodCIDRsConflictRetryAdoptsConcurrentAssignment(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failFirstPatchWithConflict(t, "10.244.5.0/24")

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("expected conflict retry to succeed, got %v", err)
	}

	if len(h.patches) != 1 {
		t.Fatalf("expected no patch after concurrent assignment, got %d patches", len(h.patches))
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR from the conflicted attempt was not released")
	}

	if !h.state.allocator.IsAllocated("10.244.5.0/24") {
		t.Fatal("concurrently assigned CIDR was not marked allocated")
	}
}
