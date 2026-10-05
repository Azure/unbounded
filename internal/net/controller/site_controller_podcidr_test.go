// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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

func TestAllocatorGenerationHeldThroughPatch(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.client.PrependReactor("patch", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		if h.sc.allocationGenerationLock.TryLock() {
			h.sc.allocationGenerationLock.Unlock()
			t.Error("allocator can retire while the Node patch is in progress")
		}

		return false, nil, nil
	})

	if err := h.sc.allocateAndPatchNodePodCIDRs(t.Context(), podCIDRTestNode, h.state, ""); err != nil {
		t.Fatal(err)
	}

	if !h.sc.allocationGenerationLock.TryLock() {
		t.Fatal("allocator generation remains locked after patch completion")
	}
	h.sc.allocationGenerationLock.Unlock()
	h.sc.updateAssignmentAllocators(nil)
}

func TestSyncObservedDifferentCIDRsReleasesReservationCopies(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(apierrors.NewTimeoutError("unconfirmed", 1))

	if err := h.sc.allocateAndPatchNodePodCIDRs(t.Context(), podCIDRTestNode, h.state, ""); err == nil {
		t.Fatal("expected patch failure")
	}

	site := h.sites[0].DeepCopy()
	site.Spec.NodeCidrs = []string{"10.0.0.0/16"}
	copySite := site.DeepCopy()
	copySite.Name = "site-b"
	h.sc.updateAssignmentAllocators([]unboundedv1alpha3.Site{*site, *copySite})
	copyState := h.sc.getAssignmentAllocator("site-b", 0)

	observed := liveNodeWithRV("8", "10.244.7.0/24")
	observed.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}}

	observed.Labels = map[string]string{}
	for _, key := range siteLabelKeys() {
		observed.Labels[key] = site.Name
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(observed); err != nil {
		t.Fatal(err)
	}

	h.sc.nodeLister = corev1listers.NewNodeLister(indexer)

	h.sc.sitesCache = []unboundedv1alpha3.Site{*site}
	if err := h.sc.syncNode(t.Context(), observed.Name); err != nil {
		t.Fatal(err)
	}

	if h.pending(observed.Name) != nil || h.state.allocator.IsAllocated("10.244.0.0/24") || copyState.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("informer observation of different CIDRs retained stale reservations")
	}

	if !h.state.allocator.IsAllocated("10.244.7.0/24") {
		t.Fatal("observed CIDR was not marked allocated")
	}
}

type podCIDRTestHarness struct {
	sc      *SiteController
	client  *kubefake.Clientset
	state   *assignmentAllocator
	sites   []unboundedv1alpha3.Site
	cached  *corev1.Node
	patches [][]byte
	// patchErr, when set, is returned for every patch after it is recorded.
	patchErr error
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
		if h.patchErr != nil {
			return true, nil, h.patchErr
		}

		return false, nil, nil
	})

	return h
}

func (h *podCIDRTestHarness) failPatches(err error) {
	h.patchErr = err
}

// setLiveNode replaces the node stored on the fake API server.
func (h *podCIDRTestHarness) setLiveNode(t *testing.T, node *corev1.Node) {
	t.Helper()

	if err := h.client.Tracker().Update(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, node, ""); err != nil {
		t.Fatalf("update live node: %v", err)
	}
}

func (h *podCIDRTestHarness) pending(nodeName string) *pendingPodCIDRAssignment {
	h.sc.pendingPodCIDRsLock.Lock()
	defer h.sc.pendingPodCIDRsLock.Unlock()

	return h.sc.pendingPodCIDRs[nodeName]
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

func TestAssignPodCIDRsKeepsPendingReservationOnPatchFailure(t *testing.T) {
	gr := schema.GroupResource{Resource: "nodes"}

	cases := map[string]error{
		"conflict":  apierrors.NewConflict(gr, podCIDRTestNode, errors.New("resourceVersion changed")),
		"invalid":   apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, podCIDRTestNode, nil),
		"timeout":   apierrors.NewTimeoutError("request timed out", 1),
		"transport": errors.New("connection reset by peer"),
	}

	for name, patchErr := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
			h.failPatches(patchErr)

			if err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a"); err == nil {
				t.Fatal("expected patch error")
			}

			if !h.state.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("CIDR was released after a failed patch without confirming the node state")
			}

			pending := h.pending(podCIDRTestNode)
			if pending == nil || pending.podCIDR != "10.244.0.0/24" {
				t.Fatalf("pending assignment = %+v, want 10.244.0.0/24", pending)
			}
		})
	}
}

func TestAssignPodCIDRsReusesPendingReservationOnNextSync(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	h.failPatches(nil)

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := decodePatch(t, h.patches[len(h.patches)-1])["spec"]["podCIDR"]; got != "10.244.0.0/24" {
		t.Fatalf("second sync patched podCIDR %v, want pending 10.244.0.0/24", got)
	}

	if h.state.allocator.IsAllocated("10.244.1.0/24") {
		t.Fatal("second sync allocated a new CIDR instead of reusing the pending one")
	}

	if h.pending(podCIDRTestNode) == nil {
		t.Fatal("reservation was cleared before informer observation")
	}

	if !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("assigned CIDR is not marked allocated")
	}
}

func TestAssignPodCIDRsAdoptsPatchAppliedDespiteError(t *testing.T) {
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

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if h.pending(podCIDRTestNode) == nil {
		t.Fatal("live observation cleared the reservation before informer observation")
	}

	if !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("CIDR held by the node was released when adopting it")
	}
}

func TestAssignPodCIDRsReleasesPendingWhenNodeHoldsOtherCIDRs(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	h.setLiveNode(t, liveNodeWithRV("6", "10.244.7.0/24"))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("pending CIDR was not released after the node was observed holding other CIDRs")
	}

	if !h.state.allocator.IsAllocated("10.244.7.0/24") {
		t.Fatal("node's CIDR was not marked allocated")
	}

	if h.pending(podCIDRTestNode) != nil {
		t.Fatal("pending assignment was not cleared")
	}
}

func TestAssignPodCIDRsReleasesPendingWhenNodeDeleted(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	if err := h.client.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}, "", podCIDRTestNode); err != nil {
		t.Fatalf("delete node: %v", err)
	}

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("pending CIDR was not released after the node was deleted")
	}
}

func TestReleasePendingPodCIDRsMatchesNodeUID(t *testing.T) {
	live := liveNodeWithRV("4")
	live.UID = "uid-new"
	h := newPodCIDRTestHarness(t, live)
	h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	h.sc.releasePendingPodCIDRs(podCIDRTestNode, "uid-old", nil)

	if h.pending(podCIDRTestNode) == nil || !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("delete of a previous node incarnation released the current node's pending CIDRs")
	}

	h.sc.releasePendingPodCIDRs(podCIDRTestNode, "uid-new", nil)

	if h.pending(podCIDRTestNode) != nil || h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("delete of the current node incarnation did not release its pending CIDRs")
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

	if h.state.allocator.IsAllocated("10.244.1.0/24") {
		t.Fatal("conflict retries allocated more than one CIDR")
	}
}

func TestAssignPodCIDRsPendingFromPreviousAssignment(t *testing.T) {
	newState := func(t *testing.T, pool string) *assignmentAllocator {
		t.Helper()

		_, ipNet, err := net.ParseCIDR(pool)
		if err != nil {
			t.Fatalf("parse pool: %v", err)
		}

		alloc, err := allocator.NewAllocator([]*net.IPNet{ipNet}, nil, 24, 64)
		if err != nil {
			t.Fatalf("NewAllocator: %v", err)
		}

		return &assignmentAllocator{siteName: "site-a", allocator: alloc}
	}

	t.Run("same-resource-version-blocks", func(t *testing.T) {
		h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
		h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

		if err := h.sc.allocateAndPatchNodePodCIDRs(context.Background(), podCIDRTestNode, h.state, ""); err == nil {
			t.Fatal("expected patch error")
		}

		other := newState(t, "10.250.0.0/16")
		h.sc.assignmentAllocators[assignmentKey("site-a", 0)] = other
		h.failPatches(nil)

		if err := h.sc.allocateAndPatchNodePodCIDRs(context.Background(), podCIDRTestNode, other, ""); err == nil {
			t.Fatal("expected an error while the previous patch may still apply")
		}

		if !h.state.allocator.IsAllocated("10.244.0.0/24") {
			t.Fatal("previous assignment's CIDR was released while its patch may still apply")
		}

		if other.allocator.IsAllocated("10.250.0.0/24") {
			t.Fatal("a second CIDR was allocated while the previous patch may still apply")
		}
	})

	t.Run("changed-resource-version-releases", func(t *testing.T) {
		h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
		h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

		if err := h.sc.allocateAndPatchNodePodCIDRs(context.Background(), podCIDRTestNode, h.state, ""); err == nil {
			t.Fatal("expected patch error")
		}

		h.setLiveNode(t, liveNodeWithRV("8"))

		other := newState(t, "10.250.0.0/16")
		h.sc.assignmentAllocators[assignmentKey("site-a", 0)] = other
		h.failPatches(nil)

		if err := h.sc.allocateAndPatchNodePodCIDRs(context.Background(), podCIDRTestNode, other, ""); err != nil {
			t.Fatalf("expected allocation from the new assignment, got %v", err)
		}

		if h.state.allocator.IsAllocated("10.244.0.0/24") {
			t.Fatal("previous assignment's CIDR was not released after its patch could no longer apply")
		}

		if !other.allocator.IsAllocated("10.250.0.0/24") {
			t.Fatal("new assignment's CIDR was not allocated")
		}
	})
}

func TestSeedAllocatorsMarksPendingPodCIDRs(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	_, pool, err := net.ParseCIDR("10.244.0.0/16")
	if err != nil {
		t.Fatalf("parse pool: %v", err)
	}

	rebuilt, err := allocator.NewAllocator([]*net.IPNet{pool}, nil, 24, 64)
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}

	key := assignmentKey("site-a", 1)
	h.sc.assignmentAllocators[key] = &assignmentAllocator{siteName: "site-a", assignmentIndex: 1, allocator: rebuilt}

	if err := h.sc.seedAllocatorsForNodes(map[string]struct{}{key: {}}); err != nil {
		t.Fatalf("seedAllocatorsForNodes: %v", err)
	}

	if !rebuilt.IsAllocated("10.244.0.0/24") {
		t.Fatal("new allocator was not seeded with pending CIDRs")
	}
}

type inspectingNodeLister struct {
	corev1listers.NodeLister
	beforeList func()
	listErr    error
}

func (l inspectingNodeLister) List(selector labels.Selector) ([]*corev1.Node, error) {
	l.beforeList()

	if l.listErr != nil {
		return nil, l.listErr
	}

	return l.NodeLister.List(selector)
}

func TestNewAllocatorPublishedOnlyAfterSeeding(t *testing.T) {
	for _, failSeed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "list-failure"}[failSeed], func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
			h.failPatches(apierrors.NewTimeoutError("unconfirmed", 1))

			if err := h.sc.allocateAndPatchNodePodCIDRs(t.Context(), podCIDRTestNode, h.state, ""); err == nil {
				t.Fatal("expected patch failure")
			}

			// A confirmed patch not yet in the informer cache must also seed
			// the replacement allocator.
			h.state.allocator.MarkAllocated("10.244.1.0/24")
			site := h.sites[0].DeepCopy()
			disabled := false
			site.Spec.PodCidrAssignments[0].AssignmentEnabled = &disabled
			site.Spec.PodCidrAssignments = append(site.Spec.PodCidrAssignments, unboundednetv1alpha1.PodCidrAssignment{
				CidrBlocks: []string{"10.244.0.0/16"},
			})
			key := assignmentKey(site.Name, 1)

			lister := inspectingNodeLister{NodeLister: h.sc.nodeLister, beforeList: func() {
				if h.sc.assignmentAllocators[key] != nil {
					t.Error("new allocator published before seeding")
				}

				if h.sc.pendingPodCIDRsLock.TryLock() {
					h.sc.pendingPodCIDRsLock.Unlock()
					t.Error("reservations can change during seeding")
				}
			}}
			if failSeed {
				lister.listErr = errors.New("list failed")
			}

			h.sc.nodeLister = lister
			h.sc.updateAssignmentAllocators([]unboundedv1alpha3.Site{*site})

			state := h.sc.getAssignmentAllocator(site.Name, 1)
			if failSeed {
				if state != nil {
					t.Fatal("published allocator after failed seeding")
				}

				return
			}

			if state == nil {
				t.Fatal("seeded allocator not published")
			}

			cidr, err := state.allocator.AllocateIPv4()
			if err != nil || cidr != "10.244.2.0/24" {
				t.Fatalf("allocation = %q, %v; want 10.244.2.0/24", cidr, err)
			}

			if _, _, err := h.sc.pendingPodCIDRsForNode(liveNodeWithRV("5"), h.state); err == nil {
				t.Fatal("worker allocated through a retired allocator pointer")
			}
		})
	}
}

func TestPendingAssignmentRequiresMatchingAllocationShape(t *testing.T) {
	for _, tc := range []struct {
		name       string
		blocks     []string
		mask       int
		compatible bool
	}{
		{name: "compatible", blocks: []string{"10.244.0.0/16"}, mask: 24, compatible: true},
		{name: "smaller-block", blocks: []string{"10.244.0.0/16"}, mask: 25},
		{name: "larger-block", blocks: []string{"10.244.0.0/16"}, mask: 23},
		{name: "dual-stack", blocks: []string{"10.244.0.0/16", "fd00::/48"}, mask: 24},
		{name: "not-contained", blocks: []string{"10.244.0.0/25"}, mask: 26},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
			if _, _, err := h.sc.pendingPodCIDRsForNode(liveNodeWithRV("4"), h.state); err != nil {
				t.Fatal(err)
			}

			state, err := h.sc.buildAssignmentAllocator(assignmentRef{
				site: h.sites[0],
				assignment: unboundednetv1alpha1.PodCidrAssignment{
					CidrBlocks:     tc.blocks,
					NodeBlockSizes: &unboundednetv1alpha1.NodeBlockSizes{IPv4: tc.mask, IPv6: 64},
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			h.sc.assignmentAllocators[assignmentKey("site-a", 0)] = state

			cidr, _, err := h.sc.pendingPodCIDRsForNode(liveNodeWithRV("4"), state)
			if tc.compatible {
				if err != nil || cidr != "10.244.0.0/24" {
					t.Fatalf("compatible adoption = %q, %v", cidr, err)
				}

				return
			}

			if err == nil || cidr != "" {
				t.Fatalf("incompatible adoption = %q, %v", cidr, err)
			}

			if !h.state.allocator.IsAllocated("10.244.0.0/24") || state.allocator.DebugState().AllocatedCount != 0 {
				t.Fatal("incompatible attempt released the pending CIDR or allocated another")
			}

			cidr, cidrs, err := h.sc.pendingPodCIDRsForNode(liveNodeWithRV("5"), state)
			if err != nil || cidr == "" || !state.allocator.MatchesAllocation(cidrs) {
				t.Fatalf("changed-version allocation = %v, %v", cidrs, err)
			}

			if h.state.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("old reservation retained after its patch became impossible")
			}
		})
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

func TestConfirmedReservationSurvivesAssignmentRemoval(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	if err := h.sc.allocateAndPatchNodePodCIDRs(t.Context(), podCIDRTestNode, h.state, ""); err != nil {
		t.Fatal(err)
	}

	h.sc.updateAssignmentAllocators(nil)
	site := h.sites[0].DeepCopy()
	site.Name = "site-b"
	h.sc.updateAssignmentAllocators([]unboundedv1alpha3.Site{*site})

	state := h.sc.getAssignmentAllocator("site-b", 0)
	if state == nil || !state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("assignment removal reopened a confirmed but unobserved CIDR")
	}

	cidr, err := state.allocator.AllocateIPv4()
	if err != nil || cidr != "10.244.1.0/24" {
		t.Fatalf("allocation = %q, %v", cidr, err)
	}

	live, err := h.client.CoreV1().Nodes().Get(t.Context(), podCIDRTestNode, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(live); err != nil {
		t.Fatal(err)
	}

	h.sc.nodeLister = corev1listers.NewNodeLister(indexer)
	h.sc.markNodeCIDRsAllocated(live, []unboundedv1alpha3.Site{*site}, "site-b")

	if h.pending(podCIDRTestNode) != nil {
		t.Fatal("reservation retained after informer observation")
	}

	if !state.allocator.IsAllocated(live.Spec.PodCIDR) {
		t.Fatal("informer observation released the node's CIDR")
	}

	h.sc.releaseNodeCIDRs(live)

	if state.allocator.IsAllocated(live.Spec.PodCIDR) {
		t.Fatal("node deletion left the confirmed CIDR copied in a new allocator")
	}
}

func TestReservationCleanupReleasesAllRecipients(t *testing.T) {
	for _, mode := range []string{"deleted", "different-cidr", "incompatible", "adopted", "other-owner", "other-pending", "list-error"} {
		t.Run(mode, func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
			h.failPatches(apierrors.NewTimeoutError("unconfirmed", 1))

			if err := h.sc.allocateAndPatchNodePodCIDRs(t.Context(), podCIDRTestNode, h.state, ""); err == nil {
				t.Fatal("expected patch failure")
			}

			site := h.sites[0].DeepCopy()
			site.Name = "site-b"
			h.sc.updateAssignmentAllocators(append(h.sites, *site))

			copyState := h.sc.getAssignmentAllocator(site.Name, 0)
			if copyState == nil || !copyState.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("pending allocation not copied")
			}

			switch mode {
			case "deleted":
				h.sc.releasePendingPodCIDRs(podCIDRTestNode, "", nil)
			case "different-cidr":
				h.sc.releasePendingPodCIDRs(podCIDRTestNode, "", []string{"10.244.7.0/24"})
			case "incompatible":
				other, err := h.sc.buildAssignmentAllocator(assignmentRef{
					site:       h.sites[0],
					assignment: unboundednetv1alpha1.PodCidrAssignment{CidrBlocks: []string{"10.250.0.0/16"}},
				})
				if err != nil {
					t.Fatal(err)
				}

				h.sc.assignmentAllocators[assignmentKey("site-a", 0)] = other
				if _, _, err := h.sc.pendingPodCIDRsForNode(liveNodeWithRV("5"), other); err != nil {
					t.Fatal(err)
				}
			case "adopted":
				if _, _, err := h.sc.pendingPodCIDRsForNode(liveNodeWithRV("5"), copyState); err != nil {
					t.Fatal(err)
				}

				h.sc.releasePendingPodCIDRs(podCIDRTestNode, "", nil)
			case "other-pending":
				h.sc.pendingPodCIDRs["other-node"] = &pendingPodCIDRAssignment{
					podCIDRs: []string{"10.244.0.0/24"}, allocator: copyState.allocator,
				}
				h.sc.releasePendingPodCIDRs(podCIDRTestNode, "", nil)
			case "other-owner":
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
				owner := liveNodeWithRV("8", "10.244.0.0/24")

				owner.Name = "other-node"
				if err := indexer.Add(owner); err != nil {
					t.Fatal(err)
				}

				h.sc.nodeLister = corev1listers.NewNodeLister(indexer)
				h.sc.releasePendingPodCIDRs(podCIDRTestNode, "", nil)
			case "list-error":
				h.sc.nodeLister = inspectingNodeLister{
					NodeLister: h.sc.nodeLister, beforeList: func() {}, listErr: errors.New("unavailable"),
				}
				h.sc.releasePendingPodCIDRs(podCIDRTestNode, "", nil)

				if h.pending(podCIDRTestNode) == nil {
					t.Fatal("lost reservation on cleanup error")
				}
			}

			wantReserved := mode == "other-owner" || mode == "other-pending" || mode == "list-error"
			for _, state := range []*assignmentAllocator{h.state, copyState} {
				if got := state.allocator.IsAllocated("10.244.0.0/24"); got != wantReserved {
					t.Fatalf("recipient retains CIDR = %v, want %v", got, wantReserved)
				}
			}

			if mode == "other-pending" {
				h.sc.releasePendingPodCIDRs("other-node", "", nil)

				if h.state.allocator.IsAllocated("10.244.0.0/24") || copyState.allocator.IsAllocated("10.244.0.0/24") {
					t.Fatal("last pending owner left copied reservations allocated")
				}
			}
		})
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

	h.client.PrependReactor("patch", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		if attempts > 1 {
			return false, nil, nil
		}

		// This reactor runs before the harness recorder, so record here.
		h.patches = append(h.patches, action.(clienttesting.PatchAction).GetPatch())

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

type fakeLeaseFence struct {
	validUntil time.Time
}

func (f *fakeLeaseFence) ValidUntil() time.Time {
	return f.validUntil
}

func TestAssignPodCIDRsRefusedWithoutLease(t *testing.T) {
	cases := map[string]time.Time{
		"never-held": {},
		"expired":    time.Now().Add(-time.Second),
	}

	for name, validUntil := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
			h.sc.SetLeaseFence(&fakeLeaseFence{validUntil: validUntil})

			err := h.sc.assignPodCIDRsForNodeWithLabel(context.Background(), h.cached, h.sites, "site-a")
			if !errors.Is(err, errLeaseNotHeld) {
				t.Fatalf("assign pod CIDRs error = %v, want errLeaseNotHeld", err)
			}

			if len(h.patches) != 0 {
				t.Fatalf("patched node without the lease: %s", h.patches[0])
			}

			if h.state.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("allocated a CIDR without the lease")
			}

			if h.pending(podCIDRTestNode) != nil {
				t.Fatal("recorded a pending assignment without the lease")
			}
		})
	}
}

// TestAssignPodCIDRsKeepsPendingWhenLeaseLapses checks that losing the lease
// between attempts leaves an earlier reservation in place: whether its patch
// was applied is still unknown, so releasing it could hand it out twice.
func TestAssignPodCIDRsKeepsPendingWhenLeaseLapses(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	fence := &fakeLeaseFence{validUntil: time.Now().Add(time.Minute)}
	h.sc.SetLeaseFence(fence)
	h.failPatches(apierrors.NewTimeoutError("request timed out", 1))

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err == nil {
		t.Fatal("expected patch error")
	}

	fence.validUntil = time.Time{}

	h.failPatches(nil)

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); !errors.Is(err, errLeaseNotHeld) {
		t.Fatalf("second sync error = %v, want errLeaseNotHeld", err)
	}

	if len(h.patches) != 1 {
		t.Fatalf("expected only the first patch attempt, got %d patches", len(h.patches))
	}

	pending := h.pending(podCIDRTestNode)
	if pending == nil || pending.podCIDR != "10.244.0.0/24" {
		t.Fatalf("pending assignment = %+v, want 10.244.0.0/24", pending)
	}

	if !h.state.allocator.IsAllocated("10.244.0.0/24") {
		t.Fatal("pending CIDR was released after the lease lapsed")
	}
}

func TestAssignPodCIDRsAllowedWithLease(t *testing.T) {
	h := newPodCIDRTestHarness(t, liveNodeWithRV("4"))
	h.sc.SetLeaseFence(&fakeLeaseFence{validUntil: time.Now().Add(time.Minute)})

	if err := h.sc.assignPodCIDRsForNode(context.Background(), h.cached, h.sites, "site-a"); err != nil {
		t.Fatalf("assign pod CIDRs: %v", err)
	}

	if len(h.patches) != 1 {
		t.Fatalf("expected one patch, got %d", len(h.patches))
	}

	if got := decodePatch(t, h.patches[0])["spec"]["podCIDR"]; got != "10.244.0.0/24" {
		t.Fatalf("patch podCIDR = %v, want 10.244.0.0/24", got)
	}
}

func TestTryAllocateForNodeLeaseFence(t *testing.T) {
	// Admission now resolves sites without allocating, regardless of the fence.
	cases := map[string]time.Time{
		"held":       time.Now().Add(time.Minute),
		"never-held": {},
		"expired":    time.Now().Add(-time.Second),
	}

	for name, validUntil := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPodCIDRTestHarness(t, liveNodeWithRV("1"))
			h.sites[0].Spec.NodeCidrs = []string{"10.0.0.0/16"}
			h.sc.sitesCache = h.sites
			h.sc.SetLeaseFence(&fakeLeaseFence{validUntil: validUntil})

			node := liveNodeWithRV("1")
			node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}}

			if siteName := h.sc.GetSiteForNode(node); siteName != "site-a" {
				t.Fatalf("site = %q, want site-a", siteName)
			}

			if h.state.allocator.IsAllocated("10.244.0.0/24") {
				t.Fatal("site resolution allocated a pod CIDR")
			}
		})
	}
}
