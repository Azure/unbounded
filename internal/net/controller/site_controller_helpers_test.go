// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

// TestAssignmentPrimitiveHelpers tests assignment primitive helpers.
func TestAssignmentPrimitiveHelpers(t *testing.T) {
	if got := assignmentKey("site-a", 2); got != "site-a/2" {
		t.Fatalf("unexpected assignment key: %s", got)
	}

	if !assignmentEnabled(nil) || assignmentEnabled(ptrBool(false)) {
		t.Fatalf("assignmentEnabled nil/false behavior mismatch")
	}

	if got := assignmentPriority(nil); got != 100 {
		t.Fatalf("expected default priority 100, got %d", got)
	}

	if got := assignmentPriority(ptrInt32(5)); got != 5 {
		t.Fatalf("expected explicit priority 5, got %d", got)
	}
}

// TestAssignmentConfigAndRegexHelpers tests assignment config and regex helpers.
func TestAssignmentConfigAndRegexHelpers(t *testing.T) {
	a := unboundednetv1alpha1.PodCidrAssignment{
		CidrBlocks: []string{"10.244.0.0/16"},
		NodeBlockSizes: &unboundednetv1alpha1.NodeBlockSizes{
			IPv4: 24,
		},
		NodeRegex: []string{"^node-"},
		Priority:  ptrInt32(10),
	}

	b := a
	if !assignmentMatchConfigEqual(a, b) {
		t.Fatalf("expected identical assignment configs to match")
	}

	b.NodeRegex = []string{"^gw-"}
	if assignmentMatchConfigEqual(a, b) {
		t.Fatalf("expected different regex config to mismatch")
	}

	regexes, err := compileNodeRegexes([]string{"^node-[0-9]+$", "^gw-"})
	if err != nil || len(regexes) != 2 {
		t.Fatalf("unexpected compileNodeRegexes result: regexes=%d err=%v", len(regexes), err)
	}

	if _, err := compileNodeRegexes([]string{"("}); err == nil {
		t.Fatalf("expected invalid regex to fail")
	}
}

// TestCollectEnabledAssignmentsAndSelection tests collect enabled assignments and selection.
func TestCollectEnabledAssignmentsAndSelection(t *testing.T) {
	site := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: unboundedv1alpha3.SiteSpec{
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
				{AssignmentEnabled: ptrBool(true), CidrBlocks: []string{"10.244.0.0/16"}, NodeRegex: []string{"^node-"}, Priority: ptrInt32(20)},
				{AssignmentEnabled: ptrBool(false), CidrBlocks: []string{"10.245.0.0/16"}, Priority: ptrInt32(1)},
				{CidrBlocks: []string{"10.246.0.0/16"}, NodeRegex: []string{"^node-"}, Priority: ptrInt32(5)},
			},
		},
	}

	sc := &SiteController{
		assignmentAllocators: map[string]*assignmentAllocator{},
	}

	enabled := sc.collectEnabledAssignments([]unboundedv1alpha3.Site{site})
	if len(enabled) != 2 {
		t.Fatalf("expected two enabled assignments, got %d", len(enabled))
	}

	r1, _ := compileNodeRegexes(site.Spec.PodCidrAssignments[0].NodeRegex)
	r3, _ := compileNodeRegexes(site.Spec.PodCidrAssignments[2].NodeRegex)
	sc.assignmentAllocators[assignmentKey("site-a", 0)] = &assignmentAllocator{
		siteName:        "site-a",
		assignmentIndex: 0,
		assignment:      site.Spec.PodCidrAssignments[0],
		nodeRegexes:     r1,
	}
	sc.assignmentAllocators[assignmentKey("site-a", 2)] = &assignmentAllocator{
		siteName:        "site-a",
		assignmentIndex: 2,
		assignment:      site.Spec.PodCidrAssignments[2],
		nodeRegexes:     r3,
	}

	selected := sc.selectAssignmentForNode(site, "node-1")
	if selected == nil || selected.assignmentIndex != 2 {
		t.Fatalf("expected lower-priority assignment index 2 selected, got %#v", selected)
	}

	if !assignmentMatchesNode(selected, "node-2") || assignmentMatchesNode(selected, "gw-1") {
		t.Fatalf("assignmentMatchesNode behavior mismatch")
	}
}

// TestCIDRAndAllocatorHelpers tests cidrand allocator helpers.
func TestCIDRAndAllocatorHelpers(t *testing.T) {
	ipv4Pools, ipv6Pools, err := splitCIDRBlocks([]string{"10.0.0.0/16", "fd00::/64"})
	if err != nil {
		t.Fatalf("splitCIDRBlocks() error = %v", err)
	}

	if len(ipv4Pools) != 1 || len(ipv6Pools) != 1 {
		t.Fatalf("unexpected split pools: v4=%d v6=%d", len(ipv4Pools), len(ipv6Pools))
	}

	if _, _, err := splitCIDRBlocks([]string{"invalid"}); err == nil {
		t.Fatalf("expected invalid CIDR to fail")
	}

	mask4, mask6 := resolveMaskSizes(nil, ipv4Pools, ipv6Pools)
	if mask4 != 24 || mask6 != 80 {
		t.Fatalf("unexpected default masks: v4=%d v6=%d", mask4, mask6)
	}

	mask4, mask6 = resolveMaskSizes(&unboundednetv1alpha1.NodeBlockSizes{IPv4: 26, IPv6: 120}, ipv4Pools, ipv6Pools)
	if mask4 != 26 || mask6 != 120 {
		t.Fatalf("unexpected explicit masks: v4=%d v6=%d", mask4, mask6)
	}

	sc := &SiteController{}

	state, err := sc.buildAssignmentAllocator(assignmentRef{
		site:  unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}},
		index: 0,
		assignment: unboundednetv1alpha1.PodCidrAssignment{
			CidrBlocks: []string{"10.250.0.0/16"},
			NodeRegex:  []string{"^node-"},
		},
	})
	if err != nil || state == nil || state.allocator == nil {
		t.Fatalf("expected allocator state, got state=%#v err=%v", state, err)
	}

	if _, err := sc.buildAssignmentAllocator(assignmentRef{
		site:       unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}},
		index:      1,
		assignment: unboundednetv1alpha1.PodCidrAssignment{},
	}); err == nil {
		t.Fatalf("expected buildAssignmentAllocator to fail without pools")
	}
}

// TestNodeAndStringHelpers tests node and string helpers.
func TestNodeAndStringHelpers(t *testing.T) {
	nodeA := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Labels:      map[string]string{canonicalSiteLabelKey: "site-a", "role": "gateway"},
			Annotations: map[string]string{WireGuardPubKeyAnnotation: "pub-a"},
		},
		Spec: corev1.NodeSpec{
			PodCIDR:  "10.244.1.0/24",
			PodCIDRs: []string{"10.244.1.0/24", "fd00:1::/80"},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.10"},
				{Type: corev1.NodeExternalIP, Address: "52.0.0.10"},
			},
		},
	}

	nodeB := nodeA.DeepCopy()
	if !nodeAddressesEqual(nodeA, nodeB) {
		t.Fatalf("expected node addresses to match")
	}

	nodeB.Status.Addresses[0].Address = "10.0.0.11"
	if nodeAddressesEqual(nodeA, nodeB) {
		t.Fatalf("expected changed node addresses to differ")
	}

	if getNodeSiteLabel(nodeA) != "site-a" || getNodeAnnotation(nodeA, WireGuardPubKeyAnnotation) != "pub-a" {
		t.Fatalf("unexpected node label/annotation helpers")
	}

	internalIPs := getNodeInternalIPStrings(nodeA)
	if len(internalIPs) != 1 || internalIPs[0] != "10.0.0.10" {
		t.Fatalf("unexpected internal IPs: %#v", internalIPs)
	}

	if !nodeHasPodCIDRs(nodeA) {
		t.Fatalf("expected nodeHasPodCIDRs true")
	}

	if got := nodePodCIDRs(nodeA); len(got) != 2 {
		t.Fatalf("expected nodePodCIDRs to prefer PodCIDRs, got %#v", got)
	}

	nodeOnlySingle := nodeA.DeepCopy()

	nodeOnlySingle.Spec.PodCIDRs = nil
	if got := nodePodCIDRs(nodeOnlySingle); len(got) != 1 || got[0] != "10.244.1.0/24" {
		t.Fatalf("expected fallback to PodCIDR, got %#v", got)
	}

	if !stringSlicesEqual([]string{"a", "b"}, []string{"a", "b"}) || stringSlicesEqual([]string{"a"}, []string{"b"}) {
		t.Fatalf("stringSlicesEqual behavior mismatch")
	}

	if got := escapeJSONPointer("a/b~c"); got != "a~1b~0c" {
		t.Fatalf("unexpected escaped JSON pointer: %s", got)
	}
}

// TestGatewayAndCIDROverlapHelpers tests gateway and cidroverlap helpers.
func TestGatewayAndCIDROverlapHelpers(t *testing.T) {
	sc := &SiteController{
		gatewayPoolsCache: []unboundednetv1alpha1.GatewayPool{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
				Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
			},
		},
	}
	nodeGateway := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"role": "gateway"}}}

	nodeRegular := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2", Labels: map[string]string{"role": "worker"}}}
	if !sc.isNodeGateway(nodeGateway) || sc.isNodeGateway(nodeRegular) {
		t.Fatalf("isNodeGateway behavior mismatch")
	}

	noOverlap := []unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.0.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.1.0.0/16"}}},
	}
	if err := validateSiteCIDRsNoOverlap(noOverlap); err != nil {
		t.Fatalf("expected non-overlapping CIDRs, got %v", err)
	}

	exactOverlap := []unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.2.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.2.0.0/16"}}},
	}
	if err := validateSiteCIDRsNoOverlap(exactOverlap); err == nil {
		t.Fatalf("expected exact CIDR overlap to fail")
	}

	rangeOverlap := []unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.3.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.3.1.0/24"}}},
	}
	if err := validateSiteCIDRsNoOverlap(rangeOverlap); err == nil {
		t.Fatalf("expected range overlap to fail")
	}
}

// TestFindDuplicateNodePodCIDRs tests find duplicate node pod cidrs.
func TestFindDuplicateNodePodCIDRs(t *testing.T) {
	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24", "fd00:1::/80"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
			Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.2.0/24", "fd00:1::/80"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-c"},
			Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-d"},
			Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.4.0/24"}},
		},
	}

	conflicts := findDuplicateNodePodCIDRs(nodes)
	if len(conflicts) != 2 {
		t.Fatalf("expected two conflicting CIDRs, got %#v", conflicts)
	}

	if got := conflicts["10.244.1.0/24"]; len(got) != 2 || got[0] != "node-a" || got[1] != "node-c" {
		t.Fatalf("unexpected IPv4 conflicts: %#v", got)
	}

	if got := conflicts["fd00:1::/80"]; len(got) != 2 || got[0] != "node-a" || got[1] != "node-b" {
		t.Fatalf("unexpected IPv6 conflicts: %#v", got)
	}

	if formatted := formatCIDRConflicts(conflicts); formatted != "10.244.1.0/24 -> [node-a,node-c]; fd00:1::/80 -> [node-a,node-b]" {
		t.Fatalf("unexpected conflict format: %q", formatted)
	}
}

func ptrBool(v bool) *bool {
	return &v
}

func ptrInt32(v int32) *int32 {
	return &v
}

func ptrString(v string) *string {
	return &v
}

func ptrUID(v types.UID) *types.UID {
	return &v
}

func siteUnstructured(t *testing.T, site unboundedv1alpha3.Site) *unstructured.Unstructured {
	t.Helper()

	site.APIVersion = unboundedv1alpha3.GroupVersion.String()
	site.Kind = "Site"

	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&site)
	if err != nil {
		t.Fatalf("convert site: %v", err)
	}

	return &unstructured.Unstructured{Object: object}
}

// legacySiteUnstructured builds a minimal pre-migration net-group Site object
// (net.unbounded-cloud.io/v1alpha1) for orphan-cleanup tests that exercise the
// migration guard.
func legacySiteUnstructured(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   legacySiteGVR.Group,
		Version: legacySiteGVR.Version,
		Kind:    "Site",
	})
	obj.SetName(name)

	return obj
}

// TestNodeSiteLabelFallback verifies the canonical-first, deprecated-fallback
// read of a Node's site membership.
func TestNodeSiteLabelFallback(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{name: "canonical", labels: map[string]string{canonicalSiteLabelKey: "s1"}, want: "s1"},
		{name: "deprecated only", labels: map[string]string{deprecatedSiteLabelKey: "s2"}, want: "s2"},
		{name: "both", labels: map[string]string{canonicalSiteLabelKey: "s3", deprecatedSiteLabelKey: "old"}, want: "s3"},
		{name: "none", labels: map[string]string{}, want: ""},
		{name: "nil labels", labels: nil, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: tc.labels}}
			if got := NodeSiteLabel(node); got != tc.want {
				t.Fatalf("NodeSiteLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNodeSiteLabelsCurrent verifies dual-write convergence: a Node is only
// up-to-date when it carries the site under BOTH keys, so a node labeled only
// with the deprecated key by an older controller is re-labeled.
func TestNodeSiteLabelsCurrent(t *testing.T) {
	deprecatedOnly := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{deprecatedSiteLabelKey: "s1"}}}
	if nodeSiteLabelsCurrent(deprecatedOnly, "s1") {
		t.Fatalf("deprecated-only node must be considered stale so the canonical label is added")
	}

	both := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{canonicalSiteLabelKey: "s1", deprecatedSiteLabelKey: "s1"}}}
	if !nodeSiteLabelsCurrent(both, "s1") {
		t.Fatalf("node carrying both keys must be current")
	}

	unlabeled := &corev1.Node{ObjectMeta: metav1.ObjectMeta{}}
	if !nodeSiteLabelsCurrent(unlabeled, "") {
		t.Fatalf("unlabeled node must be current for empty site")
	}

	if nodeSiteLabelsCurrent(both, "") {
		t.Fatalf("labeled node must be stale for empty site (labels need removing)")
	}
}

// TestSiteLabelPatches verifies the add/remove patch builders cover both keys.
func TestSiteLabelPatches(t *testing.T) {
	add, err := siteLabelAddMergePatch("s1")
	if err != nil {
		t.Fatalf("siteLabelAddMergePatch: %v", err)
	}

	for _, key := range siteLabelKeys() {
		if !strings.Contains(string(add), key) || !strings.Contains(string(add), "s1") {
			t.Fatalf("add patch missing key %q or value: %s", key, add)
		}
	}

	remove, err := siteLabelRemoveMergePatch()
	if err != nil {
		t.Fatalf("siteLabelRemoveMergePatch: %v", err)
	}

	for _, key := range siteLabelKeys() {
		if !strings.Contains(string(remove), key) {
			t.Fatalf("remove patch missing key %q: %s", key, remove)
		}
	}
}

// TestBuildSliceObjectOwnerRefUsesMachinaSiteGVK guards that SiteNodeSlice owner
// references point at the Site's real GVK (unbounded-cloud.io/v1alpha3). A stale
// net-group ownerRef would let the garbage collector orphan or wrongly collect
// slices once the legacy net Site CRD is reaped during migration.
func TestBuildSliceObjectOwnerRefUsesMachinaSiteGVK(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "uid-123"}}

	obj := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, nil)

	owners, found, err := unstructured.NestedSlice(obj.Object, "metadata", "ownerReferences")
	if err != nil || !found || len(owners) != 1 {
		t.Fatalf("ownerReferences missing: found=%t err=%v len=%d", found, err, len(owners))
	}

	owner, ok := owners[0].(map[string]interface{})
	if !ok {
		t.Fatalf("owner reference has unexpected type %T", owners[0])
	}

	if got := owner["apiVersion"]; got != unboundedv1alpha3.GroupVersion.String() {
		t.Fatalf("owner apiVersion = %v, want %s", got, unboundedv1alpha3.GroupVersion.String())
	}

	if got := owner["kind"]; got != "Site" {
		t.Fatalf("owner kind = %v, want Site", got)
	}

	if got := owner["uid"]; got != "uid-123" {
		t.Fatalf("owner uid = %v, want uid-123", got)
	}

	refs := obj.GetOwnerReferences()
	if len(refs) != 1 || refs[0].Controller == nil || !*refs[0].Controller {
		t.Fatalf("owner reference is not controlling: %#v", refs)
	}

	if refs[0].BlockOwnerDeletion == nil || *refs[0].BlockOwnerDeletion {
		t.Fatalf("blockOwnerDeletion must be explicitly false: %#v", refs[0].BlockOwnerDeletion)
	}

	if !hasExactSiteOwnerReference(refs, site) {
		t.Fatalf("desired owner reference was not accepted: %#v", refs)
	}

	refs[0].BlockOwnerDeletion = nil
	if !hasExactSiteOwnerReference(refs, site) {
		t.Fatalf("nil blockOwnerDeletion must be equivalent to false")
	}

	refs[0].BlockOwnerDeletion = ptrBool(true)
	if hasExactSiteOwnerReference(refs, site) {
		t.Fatalf("true blockOwnerDeletion must be repaired")
	}
}

func TestCreateOrUpdateSliceRepairsOwnerReferenceWithoutNodeChanges(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "current-uid"}}
	nodes := []interface{}{map[string]interface{}{"name": "node-a"}}
	desired := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, nodes)
	stale := desired.DeepCopy()
	stale.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "net.unbounded-cloud.io/v1alpha1",
		Kind:       "Site",
		Name:       site.Name,
		UID:        "legacy-uid",
	}})

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(stale.DeepCopy()); err != nil {
		t.Fatalf("add stale slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		stale.DeepCopy(),
	)
	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}

	if err := sc.createOrUpdateSlice(context.Background(), site, 0, []unboundednetv1alpha1.NodeInfo{{Name: "node-a"}}); err != nil {
		t.Fatalf("createOrUpdateSlice: %v", err)
	}

	got, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), "site-a-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated SiteNodeSlice: %v", err)
	}

	if !hasExactSiteOwnerReference(got.GetOwnerReferences(), site) {
		t.Fatalf("owner reference was not repaired: %#v", got.GetOwnerReferences())
	}
}

func TestCreateOrUpdateSliceGetsLiveObjectAfterConflict(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "current-uid"}}
	desired := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, []interface{}{map[string]interface{}{"name": "node-a"}})
	stale := desired.DeepCopy()
	stale.SetResourceVersion("1")
	stale.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "net.unbounded-cloud.io/v1alpha1",
		Kind:       "Site",
		Name:       site.Name,
		UID:        "legacy-uid",
	}})

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(stale.DeepCopy()); err != nil {
		t.Fatalf("add stale slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		stale.DeepCopy(),
	)

	getCalls := 0

	dynamicClient.PrependReactor("get", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		getCalls++

		return false, nil, nil
	})

	updateCalls := 0

	dynamicClient.PrependReactor("update", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		updated := action.(clienttesting.UpdateAction).GetObject().(*unstructured.Unstructured) //nolint:errcheck

		if updateCalls == 1 {
			fresh := stale.DeepCopy()
			fresh.SetResourceVersion("2")

			if err := dynamicClient.Tracker().Update(siteNodeSliceGVR, fresh, ""); err != nil {
				t.Fatalf("update live object after conflict: %v", err)
			}

			return true, nil, apierrors.NewConflict(siteNodeSliceGVR.GroupResource(), stale.GetName(), errors.New("test conflict"))
		}

		if got := updated.GetResourceVersion(); got != "2" {
			t.Errorf("retry resourceVersion = %q, want live version 2", got)
		}

		return false, nil, nil
	})

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}
	if err := sc.createOrUpdateSlice(context.Background(), site, 0, []unboundednetv1alpha1.NodeInfo{{Name: "node-a"}}); err != nil {
		t.Fatalf("createOrUpdateSlice: %v", err)
	}

	if getCalls < 2 {
		t.Fatalf("live GET calls = %d, want at least 2", getCalls)
	}

	if updateCalls != 2 {
		t.Fatalf("update calls = %d, want 2", updateCalls)
	}
}

func TestCreateOrUpdateSliceGetsLiveObjectAfterAlreadyExists(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "current-uid"}}
	existing := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, []interface{}{map[string]interface{}{"name": "old-node"}})
	existing.SetResourceVersion("1")

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		existing.DeepCopy(),
	)

	getCalls := 0

	dynamicClient.PrependReactor("get", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewNotFound(siteNodeSliceGVR.GroupResource(), existing.GetName())
		}

		return false, nil, nil
	})

	sc := &SiteController{dynamicClient: dynamicClient}
	if err := sc.createOrUpdateSlice(context.Background(), site, 0, []unboundednetv1alpha1.NodeInfo{{Name: "node-a"}}); err != nil {
		t.Fatalf("createOrUpdateSlice: %v", err)
	}

	if getCalls < 2 {
		t.Fatalf("live GET calls = %d, want at least 2", getCalls)
	}

	got, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), existing.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated SiteNodeSlice: %v", err)
	}

	nodes, _, err := unstructured.NestedSlice(got.Object, "nodes")
	if err != nil {
		t.Fatalf("get updated nodes: %v", err)
	}

	if len(nodes) != 1 || nodes[0].(map[string]interface{})["name"] != "node-a" { //nolint:errcheck
		t.Fatalf("slice was not updated after AlreadyExists: %#v", nodes)
	}
}

func TestCreateOrUpdateSlicePreservesLiveMetadata(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}
	existing := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, []interface{}{map[string]interface{}{"name": "old-node"}})
	existing.SetLabels(map[string]string{"concurrent-label": "keep"})
	existing.SetAnnotations(map[string]string{"concurrent-annotation": "keep"})
	existing.SetFinalizers([]string{"example.com/concurrent-finalizer"})
	existing.Object["concurrentField"] = map[string]interface{}{"value": "keep"}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		existing.DeepCopy(),
	)

	sc := &SiteController{dynamicClient: dynamicClient}
	if err := sc.createOrUpdateSlice(context.Background(), site, 0, []unboundednetv1alpha1.NodeInfo{{Name: "node-a"}}); err != nil {
		t.Fatalf("createOrUpdateSlice: %v", err)
	}

	got, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), existing.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated SiteNodeSlice: %v", err)
	}

	if got.GetLabels()["concurrent-label"] != "keep" {
		t.Fatalf("labels were not preserved: %#v", got.GetLabels())
	}

	if got.GetAnnotations()["concurrent-annotation"] != "keep" {
		t.Fatalf("annotations were not preserved: %#v", got.GetAnnotations())
	}

	if len(got.GetFinalizers()) != 1 || got.GetFinalizers()[0] != "example.com/concurrent-finalizer" {
		t.Fatalf("finalizers were not preserved: %#v", got.GetFinalizers())
	}

	if value, found, _ := unstructured.NestedString(got.Object, "concurrentField", "value"); !found || value != "keep" { //nolint:errcheck
		t.Fatalf("uncontrolled field was not preserved: %#v", got.Object["concurrentField"])
	}

	if !hasExactSiteOwnerReference(got.GetOwnerReferences(), site) {
		t.Fatalf("owner reference was not converged: %#v", got.GetOwnerReferences())
	}
}

func TestCreateOrUpdateSliceRetriesUpdateNotFound(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}
	existing := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, []interface{}{map[string]interface{}{"name": "old-node"}})

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		existing.DeepCopy(),
	)

	updateCalls := 0

	dynamicClient.PrependReactor("update", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		if updateCalls != 1 {
			return false, nil, nil
		}

		if err := dynamicClient.Tracker().Delete(siteNodeSliceGVR, "", existing.GetName()); err != nil {
			t.Fatalf("delete slice before NotFound response: %v", err)
		}

		return true, nil, apierrors.NewNotFound(siteNodeSliceGVR.GroupResource(), existing.GetName())
	})

	sc := &SiteController{dynamicClient: dynamicClient}
	if err := sc.createOrUpdateSlice(context.Background(), site, 0, []unboundednetv1alpha1.NodeInfo{{Name: "node-a"}}); err != nil {
		t.Fatalf("createOrUpdateSlice: %v", err)
	}

	got, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), existing.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get recreated SiteNodeSlice: %v", err)
	}

	nodes, _, err := unstructured.NestedSlice(got.Object, "nodes")
	if err != nil {
		t.Fatalf("get recreated nodes: %v", err)
	}

	if updateCalls != 1 || len(nodes) != 1 || nodes[0].(map[string]interface{})["name"] != "node-a" { //nolint:errcheck
		t.Fatalf("slice was not recreated after update NotFound: updates=%d nodes=%#v", updateCalls, nodes)
	}
}

func TestUpdateSiteSlicesRevalidatesExtraSlicesAndUsesDeletePreconditions(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}
	reassignedCached := (&SiteController{}).buildSliceObject(site, "site-a-reassigned", 5, nil)
	reassignedCached.SetUID("cached-uid")
	reassignedLive := reassignedCached.DeepCopy()
	reassignedLive.Object["siteName"] = "site-b"
	reassignedLive.SetUID("replacement-uid")

	extraCached := (&SiteController{}).buildSliceObject(site, "site-a-extra", 6, nil)
	extraCached.SetUID("cached-extra-uid")
	extraLive := extraCached.DeepCopy()
	extraLive.SetUID("live-extra-uid")
	extraLive.SetResourceVersion("live-extra-version")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(reassignedCached); err != nil {
		t.Fatalf("add reassigned slice to cache: %v", err)
	}

	if err := store.Add(extraCached); err != nil {
		t.Fatalf("add extra slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		reassignedLive,
		extraLive,
	)

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}
	if err := sc.updateSiteSlices(context.Background(), site, nil, 0); err != nil {
		t.Fatalf("updateSiteSlices: %v", err)
	}

	if _, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), reassignedLive.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("reassigned replacement was deleted: %v", err)
	}

	deleteActions := 0

	for _, action := range dynamicClient.Actions() {
		deleteAction, ok := action.(clienttesting.DeleteAction)
		if !ok {
			continue
		}

		deleteActions++

		if deleteAction.GetName() != extraLive.GetName() {
			t.Fatalf("deleted unexpected slice %q", deleteAction.GetName())
		}

		preconditions := deleteAction.GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil || *preconditions.UID != extraLive.GetUID() {
			t.Fatalf("delete UID precondition = %#v, want %q", preconditions, extraLive.GetUID())
		}

		if preconditions.ResourceVersion == nil || *preconditions.ResourceVersion != extraLive.GetResourceVersion() {
			t.Fatalf("delete resourceVersion precondition = %#v, want %q", preconditions, extraLive.GetResourceVersion())
		}
	}

	if deleteActions != 1 {
		t.Fatalf("delete actions = %d, want 1", deleteActions)
	}
}

func TestUpdateSiteSlicesDoesNotDeleteSameUIDConcurrentUpdate(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}
	extra := (&SiteController{}).buildSliceObject(site, "site-a-extra", 1, nil)
	extra.SetUID("extra-uid")
	extra.SetResourceVersion("1")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(extra.DeepCopy()); err != nil {
		t.Fatalf("add extra slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		extra.DeepCopy(),
	)

	deleteCalls := 0

	dynamicClient.PrependReactor("delete", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deleteCalls++
		deleteAction := action.(clienttesting.DeleteAction) //nolint:errcheck

		concurrent := extra.DeepCopy()
		concurrent.SetResourceVersion("2")
		concurrent.SetAnnotations(map[string]string{"concurrent-update": "preserve"})

		if err := dynamicClient.Tracker().Update(siteNodeSliceGVR, concurrent, ""); err != nil {
			t.Fatalf("store concurrent slice update: %v", err)
		}

		preconditions := deleteAction.GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil || *preconditions.UID != concurrent.GetUID() {
			t.Errorf("delete UID precondition = %#v, want %q", preconditions, concurrent.GetUID())
		}

		if preconditions == nil || preconditions.ResourceVersion == nil {
			t.Errorf("delete resourceVersion precondition = %#v, want %q", preconditions, extra.GetResourceVersion())

			return false, nil, nil
		}

		if *preconditions.ResourceVersion != extra.GetResourceVersion() {
			t.Errorf("delete resourceVersion precondition = %q, want %q", *preconditions.ResourceVersion, extra.GetResourceVersion())
		}

		if *preconditions.ResourceVersion != concurrent.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(
				siteNodeSliceGVR.GroupResource(),
				concurrent.GetName(),
				errors.New("resourceVersion precondition failed"),
			)
		}

		return false, nil, nil
	})

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}

	err := sc.updateSiteSlices(context.Background(), site, nil, 0)
	if err == nil || !apierrors.IsConflict(err) {
		t.Fatalf("updateSiteSlices error = %v, want conflict", err)
	}

	if deleteCalls != 1 {
		t.Fatalf("delete calls = %d, want 1", deleteCalls)
	}

	got, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), extra.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get concurrently updated slice: %v", err)
	}

	if got.GetUID() != extra.GetUID() || got.GetResourceVersion() != "2" || got.GetAnnotations()["concurrent-update"] != "preserve" {
		t.Fatalf("concurrently updated slice was not preserved: %#v", got.Object["metadata"])
	}
}

func TestCreateOrUpdateSliceDoesNotUseSameNameReplacementSite(t *testing.T) {
	oldSite := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "old-site-uid"}}
	replacement := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: oldSite.Name, UID: "replacement-site-uid"}}
	existing := (&SiteController{}).buildSliceObject(oldSite, "site-a-0", 0, []interface{}{map[string]interface{}{"name": "old-node"}})
	existing.SetUID("slice-uid")
	existing.SetResourceVersion("1")

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, oldSite),
		existing.DeepCopy(),
	)

	replaced := false

	dynamicClient.PrependReactor("get", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if replaced {
			return false, nil, nil
		}

		replaced = true

		if err := dynamicClient.Tracker().Delete(siteGVR, "", oldSite.Name); err != nil {
			t.Fatalf("delete old site: %v", err)
		}

		if err := dynamicClient.Tracker().Add(siteUnstructured(t, replacement)); err != nil {
			t.Fatalf("add replacement site: %v", err)
		}

		return false, nil, nil
	})

	sc := &SiteController{dynamicClient: dynamicClient}

	err := sc.createOrUpdateSlice(context.Background(), oldSite, 0, []unboundednetv1alpha1.NodeInfo{{Name: "new-node"}})
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("createOrUpdateSlice error = %v, want Site UID change", err)
	}

	for _, action := range dynamicClient.Actions() {
		if action.GetResource() == siteNodeSliceGVR && (action.GetVerb() == "create" || action.GetVerb() == "update") {
			t.Fatalf("slice was mutated after Site replacement: %#v", action)
		}
	}

	got, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), existing.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get preserved slice: %v", err)
	}

	if !hasExactSiteOwnerReference(got.GetOwnerReferences(), oldSite) {
		t.Fatalf("slice owner was changed to the replacement Site: %#v", got.GetOwnerReferences())
	}
}

func TestCleanupOrphanSiteNodeSlicesRevalidatesSitesAndDeletePreconditions(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}

	emptySiteName := (&SiteController{}).buildSliceObject(site, "empty-site-name", 0, nil)
	emptySiteName.Object["siteName"] = ""
	emptySiteName.SetUID("empty-uid")
	emptySiteName.SetResourceVersion("empty-rv")

	missingSite := (&SiteController{}).buildSliceObject(site, "missing-site", 0, nil)
	missingSite.Object["siteName"] = "missing"
	missingSite.SetUID("missing-uid")
	missingSite.SetResourceVersion("missing-rv")

	presentSite := (&SiteController{}).buildSliceObject(site, "present-site", 0, nil)
	presentSite.Object["siteName"] = site.Name
	presentSite.SetUID("present-slice-uid")
	presentSite.SetResourceVersion("present-slice-rv")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, slice := range []*unstructured.Unstructured{emptySiteName, missingSite, presentSite} {
		if err := store.Add(slice.DeepCopy()); err != nil {
			t.Fatalf("add slice %s to cache: %v", slice.GetName(), err)
		}
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			legacySiteGVR:    "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, site),
		emptySiteName.DeepCopy(),
		missingSite.DeepCopy(),
		presentSite.DeepCopy(),
	)

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}
	if err := sc.cleanupOrphanSiteNodeSlices(context.Background()); err != nil {
		t.Fatalf("cleanupOrphanSiteNodeSlices: %v", err)
	}

	if _, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), presentSite.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("slice for present Site was deleted: %v", err)
	}

	wantPreconditions := map[string]metav1.Preconditions{
		emptySiteName.GetName(): {
			UID:             ptrUID(emptySiteName.GetUID()),
			ResourceVersion: ptrString(emptySiteName.GetResourceVersion()),
		},
		missingSite.GetName(): {
			UID:             ptrUID(missingSite.GetUID()),
			ResourceVersion: ptrString(missingSite.GetResourceVersion()),
		},
	}

	deleteActions := 0

	for _, action := range dynamicClient.Actions() {
		deleteAction, ok := action.(clienttesting.DeleteAction)
		if !ok || action.GetResource() != siteNodeSliceGVR {
			continue
		}

		deleteActions++
		want, ok := wantPreconditions[deleteAction.GetName()]

		if !ok {
			t.Fatalf("deleted non-orphan slice %q", deleteAction.GetName())
		}

		got := deleteAction.GetDeleteOptions().Preconditions
		if got == nil || got.UID == nil || *got.UID != *want.UID || got.ResourceVersion == nil || *got.ResourceVersion != *want.ResourceVersion {
			t.Fatalf("delete preconditions for %s = %#v, want %#v", deleteAction.GetName(), got, want)
		}
	}

	if deleteActions != len(wantPreconditions) {
		t.Fatalf("delete actions = %d, want %d", deleteActions, len(wantPreconditions))
	}
}

func TestCleanupOrphanSiteNodeSlicesPreservesSliceOnSiteLookupError(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}
	slice := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, nil)
	slice.SetUID("slice-uid")
	slice.SetResourceVersion("slice-rv")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(slice.DeepCopy()); err != nil {
		t.Fatalf("add slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			legacySiteGVR:    "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		slice.DeepCopy(),
	)
	// The live Site listing is the health check that gates all deletion. If it
	// cannot be read the routine must not delete anything.
	dynamicClient.PrependReactor("list", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(siteGVR.GroupResource(), "", errors.New("test denial"))
	})

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}

	err := sc.cleanupOrphanSiteNodeSlices(context.Background())
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("cleanupOrphanSiteNodeSlices error = %v, want forbidden", err)
	}

	if _, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), slice.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("slice was deleted after conservative Site lookup failure: %v", err)
	}
}

// TestCleanupOrphanSiteNodeSlicesSkipsWhenNoSitesPresent covers the
// migration/upgrade window where the machina Site set is observed empty (Sites
// not yet translated) while legacy slices still exist. Deleting them would tear
// down every inter-node tunnel cluster-wide, so cleanup must be a no-op.
func TestCleanupOrphanSiteNodeSlicesSkipsWhenNoSitesPresent(t *testing.T) {
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}}
	slice := (&SiteController{}).buildSliceObject(site, "site-a-0", 0, nil)
	slice.Object["siteName"] = site.Name
	slice.SetUID("slice-uid")
	slice.SetResourceVersion("slice-rv")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(slice.DeepCopy()); err != nil {
		t.Fatalf("add slice to cache: %v", err)
	}

	// No Site objects registered: the live Site listing returns zero items.
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			legacySiteGVR:    "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		slice.DeepCopy(),
	)

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}

	if err := sc.cleanupOrphanSiteNodeSlices(context.Background()); err != nil {
		t.Fatalf("cleanupOrphanSiteNodeSlices: %v", err)
	}

	if _, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), slice.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("slice was deleted while no Sites were present: %v", err)
	}

	for _, action := range dynamicClient.Actions() {
		if _, ok := action.(clienttesting.DeleteAction); ok && action.GetResource() == siteNodeSliceGVR {
			t.Fatalf("unexpected slice delete while Site source was empty")
		}
	}
}

// TestCleanupOrphanSiteNodeSlicesKeepsSliceForUntranslatedLegacySite covers a
// partially-migrated cluster: at least one Site has been translated into the
// machina group, but this slice references a Site that is still only in the
// legacy net group. It is not an orphan and must be preserved.
func TestCleanupOrphanSiteNodeSlicesKeepsSliceForUntranslatedLegacySite(t *testing.T) {
	translated := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "translated", UID: "translated-uid"}}

	const legacyName = "legacy-site"

	slice := (&SiteController{}).buildSliceObject(translated, "legacy-site-0", 0, nil)
	slice.Object["siteName"] = legacyName
	slice.SetUID("slice-uid")
	slice.SetResourceVersion("slice-rv")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(slice.DeepCopy()); err != nil {
		t.Fatalf("add slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			legacySiteGVR:    "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, translated),
		legacySiteUnstructured(legacyName),
		slice.DeepCopy(),
	)

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}

	if err := sc.cleanupOrphanSiteNodeSlices(context.Background()); err != nil {
		t.Fatalf("cleanupOrphanSiteNodeSlices: %v", err)
	}

	if _, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), slice.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("slice for untranslated legacy Site was deleted: %v", err)
	}

	for _, action := range dynamicClient.Actions() {
		if _, ok := action.(clienttesting.DeleteAction); ok && action.GetResource() == siteNodeSliceGVR {
			t.Fatalf("unexpected slice delete for an untranslated legacy Site")
		}
	}
}

// TestCleanupOrphanSiteNodeSlicesPreservesSliceOnLegacySiteLookupError asserts
// that if the legacy net-group Site lookup is denied (e.g. the net controller is
// missing the read grant on net.unbounded-cloud.io/sites), cleanup preserves the
// slice rather than deleting it. The check is safe-by-default: a lookup error is
// never read as "the Site is gone".
func TestCleanupOrphanSiteNodeSlicesPreservesSliceOnLegacySiteLookupError(t *testing.T) {
	// A translated Site exists so the empty-source guard does not short-circuit.
	present := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "present", UID: "present-uid"}}

	// This slice references a Site absent from the machina group, so cleanup
	// consults the legacy group - which is denied below.
	slice := (&SiteController{}).buildSliceObject(present, "untranslated-0", 0, nil)
	slice.Object["siteName"] = "untranslated"
	slice.SetUID("slice-uid")
	slice.SetResourceVersion("slice-rv")

	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(slice.DeepCopy()); err != nil {
		t.Fatalf("add slice to cache: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			legacySiteGVR:    "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		siteUnstructured(t, present),
		slice.DeepCopy(),
	)
	// Deny only the legacy net-group Site GET (the migration-window check).
	dynamicClient.PrependReactor("get", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetResource().Group != legacySiteGVR.Group {
			return false, nil, nil
		}

		getAction := action.(clienttesting.GetAction) //nolint:errcheck

		return true, nil, apierrors.NewForbidden(legacySiteGVR.GroupResource(), getAction.GetName(), errors.New("test denial"))
	})

	sc := &SiteController{
		dynamicClient: dynamicClient,
		sliceInformer: &fakeInformer{store: store},
	}

	err := sc.cleanupOrphanSiteNodeSlices(context.Background())
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("cleanupOrphanSiteNodeSlices error = %v, want forbidden", err)
	}

	if _, err := dynamicClient.Resource(siteNodeSliceGVR).Get(context.Background(), slice.GetName(), metav1.GetOptions{}); err != nil {
		t.Fatalf("slice was deleted after legacy Site lookup was denied: %v", err)
	}
}

func TestSliceMutationFailurePropagatesRedirtiesAndSkipsStatus(t *testing.T) {
	site := unboundedv1alpha3.Site{
		TypeMeta: metav1.TypeMeta{APIVersion: unboundedv1alpha3.GroupVersion.String(), Kind: "Site"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "site-a",
			UID:        "site-uid",
			Finalizers: []string{ProtectionFinalizer},
		},
		Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.0.0.0/24"}},
	}

	siteObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&site)
	if err != nil {
		t.Fatalf("convert site: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		&unstructured.Unstructured{Object: siteObject},
	)
	dynamicClient.PrependReactor("create", "sitenodeslices", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(siteNodeSliceGVR.GroupResource(), "site-a-0", errors.New("test denial"))
	})

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{
			Type:    corev1.NodeInternalIP,
			Address: "10.0.0.10",
		}}},
	}); err != nil {
		t.Fatalf("add node: %v", err)
	}

	sc := &SiteController{
		dynamicClient: dynamicClient,
		nodeLister:    corev1listers.NewNodeLister(nodeIndexer),
		sliceInformer: &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc)},
		sitesCache:    []unboundedv1alpha3.Site{site},
	}
	sc.markSlicesDirty()

	err = sc.updateSiteSlicesIfDirty(context.Background())
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("updateSiteSlicesIfDirty error = %v, want forbidden", err)
	}

	if !sc.slicesDirty.Load() {
		t.Fatalf("failed slice pass did not restore dirty flag")
	}

	for _, action := range dynamicClient.Actions() {
		if action.GetVerb() == "patch" && action.GetResource() == siteGVR && action.GetSubresource() == "status" {
			t.Fatalf("site status was published after slice mutation failed: %#v", action)
		}
	}
}

func TestStatusFailurePropagatesAndRedirties(t *testing.T) {
	site := unboundedv1alpha3.Site{
		TypeMeta:   metav1.TypeMeta{APIVersion: unboundedv1alpha3.GroupVersion.String(), Kind: "Site"},
		ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"},
		Status:     unboundedv1alpha3.SiteStatus{NodeCount: 1},
	}

	siteObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&site)
	if err != nil {
		t.Fatalf("convert site: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		&unstructured.Unstructured{Object: siteObject},
	)
	dynamicClient.PrependReactor("patch", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(siteGVR.GroupResource(), site.Name, errors.New("test status denial"))
	})

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	sc := &SiteController{
		dynamicClient: dynamicClient,
		nodeLister:    corev1listers.NewNodeLister(nodeIndexer),
		sliceInformer: &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc)},
		sitesCache:    []unboundedv1alpha3.Site{site},
	}
	sc.markSlicesDirty()

	err = sc.updateSiteSlicesIfDirty(context.Background())
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("updateSiteSlicesIfDirty error = %v, want forbidden", err)
	}

	if !sc.slicesDirty.Load() {
		t.Fatalf("failed status update did not restore dirty flag")
	}
}

func TestFinalizerFailurePropagatesAndRedirties(t *testing.T) {
	site := unboundedv1alpha3.Site{
		TypeMeta:   metav1.TypeMeta{APIVersion: unboundedv1alpha3.GroupVersion.String(), Kind: "Site"},
		ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"},
		Spec:       unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.0.0.0/24"}},
		Status:     unboundedv1alpha3.SiteStatus{NodeCount: 1},
	}

	siteObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&site)
	if err != nil {
		t.Fatalf("convert site: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		&unstructured.Unstructured{Object: siteObject},
	)
	dynamicClient.PrependReactor("update", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(siteGVR.GroupResource(), site.Name, errors.New("test finalizer denial"))
	})

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-a", Labels: map[string]string{"role": "gateway"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{
			Type:    corev1.NodeInternalIP,
			Address: "10.0.0.10",
		}}},
	}); err != nil {
		t.Fatalf("add gateway node: %v", err)
	}

	sc := &SiteController{
		dynamicClient: dynamicClient,
		nodeLister:    corev1listers.NewNodeLister(nodeIndexer),
		sliceInformer: &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc)},
		sitesCache:    []unboundedv1alpha3.Site{site},
		gatewayPoolsCache: []unboundednetv1alpha1.GatewayPool{{
			Spec: unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
		}},
	}
	sc.markSlicesDirty()

	err = sc.updateSiteSlicesIfDirty(context.Background())
	if err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("updateSiteSlicesIfDirty error = %v, want forbidden", err)
	}

	if !sc.slicesDirty.Load() {
		t.Fatalf("failed finalizer update did not restore dirty flag")
	}
}

func TestFinalizerConflictRetryPreservesConcurrentFinalizers(t *testing.T) {
	resource := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": unboundedv1alpha3.GroupVersion.String(),
		"kind":       "Site",
		"metadata": map[string]interface{}{
			"name":       "site-a",
			"uid":        "site-uid",
			"finalizers": []interface{}{"example.com/existing"},
		},
	}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{siteGVR: "SiteList"},
		resource,
	)

	updateCalls := 0

	dynamicClient.PrependReactor("update", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateCalls++
		if updateCalls != 1 {
			return false, nil, nil
		}

		concurrent := resource.DeepCopy()
		concurrent.SetFinalizers([]string{"example.com/existing", "example.com/concurrent"})

		if err := dynamicClient.Tracker().Update(siteGVR, concurrent, ""); err != nil {
			t.Fatalf("store concurrent finalizer: %v", err)
		}

		return true, nil, apierrors.NewConflict(siteGVR.GroupResource(), resource.GetName(), errors.New("test conflict"))
	})

	if err := ensureFinalizer(context.Background(), dynamicClient, siteGVR, resource.GetName(), nil, resource.GetUID()); err != nil {
		t.Fatalf("ensureFinalizer: %v", err)
	}

	got, err := dynamicClient.Resource(siteGVR).Get(context.Background(), resource.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated site: %v", err)
	}

	wantFinalizers := []string{"example.com/existing", "example.com/concurrent", ProtectionFinalizer}
	if !stringSlicesEqual(got.GetFinalizers(), wantFinalizers) {
		t.Fatalf("finalizers = %#v, want %#v", got.GetFinalizers(), wantFinalizers)
	}

	if err := removeFinalizer(context.Background(), dynamicClient, siteGVR, resource.GetName(), nil, resource.GetUID()); err != nil {
		t.Fatalf("removeFinalizer: %v", err)
	}

	got, err = dynamicClient.Resource(siteGVR).Get(context.Background(), resource.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get site after remove: %v", err)
	}

	wantFinalizers = []string{"example.com/existing", "example.com/concurrent"}
	if !stringSlicesEqual(got.GetFinalizers(), wantFinalizers) {
		t.Fatalf("finalizers after remove = %#v, want %#v", got.GetFinalizers(), wantFinalizers)
	}
}

func TestFinalizerRetryDoesNotMutateSameNameReplacement(t *testing.T) {
	oldSite := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "site-a",
			UID:        "old-site-uid",
			Finalizers: []string{"example.com/old"},
		},
	}
	replacement := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{
			Name:       oldSite.Name,
			UID:        "replacement-site-uid",
			Finalizers: []string{"example.com/replacement"},
		},
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{siteGVR: "SiteList"},
		siteUnstructured(t, oldSite),
	)

	updateCalls := 0

	dynamicClient.PrependReactor("update", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateCalls++

		if err := dynamicClient.Tracker().Delete(siteGVR, "", oldSite.Name); err != nil {
			t.Fatalf("delete old site: %v", err)
		}

		if err := dynamicClient.Tracker().Add(siteUnstructured(t, replacement)); err != nil {
			t.Fatalf("add replacement site: %v", err)
		}

		return true, nil, apierrors.NewConflict(siteGVR.GroupResource(), oldSite.Name, errors.New("test replacement"))
	})

	err := ensureFinalizer(context.Background(), dynamicClient, siteGVR, oldSite.Name, oldSite.Finalizers, oldSite.UID)
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("ensureFinalizer error = %v, want Site UID change", err)
	}

	if updateCalls != 1 {
		t.Fatalf("update calls = %d, want 1", updateCalls)
	}

	got, err := dynamicClient.Resource(siteGVR).Get(context.Background(), replacement.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get replacement site: %v", err)
	}

	if got.GetUID() != replacement.UID || !stringSlicesEqual(got.GetFinalizers(), replacement.Finalizers) {
		t.Fatalf("replacement Site was mutated: uid=%q finalizers=%#v", got.GetUID(), got.GetFinalizers())
	}
}

func TestStatusRetryDoesNotPatchSameNameReplacement(t *testing.T) {
	oldSite := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "old-site-uid", ResourceVersion: "10"},
	}
	replacement := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: oldSite.Name, UID: "replacement-site-uid", ResourceVersion: "11"},
		Status:     unboundedv1alpha3.SiteStatus{NodeCount: 99, SliceCount: 99},
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{siteGVR: "SiteList"},
		siteUnstructured(t, oldSite),
	)

	patchCalls := 0

	dynamicClient.PrependReactor("patch", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchCalls++
		patchAction := action.(clienttesting.PatchAction) //nolint:errcheck

		var patch struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(patchAction.GetPatch(), &patch); err != nil {
			t.Fatalf("unmarshal status patch: %v", err)
		}

		if patch.Metadata.ResourceVersion != oldSite.ResourceVersion {
			t.Fatalf("status patch resourceVersion = %q, want %q", patch.Metadata.ResourceVersion, oldSite.ResourceVersion)
		}

		if err := dynamicClient.Tracker().Delete(siteGVR, "", oldSite.Name); err != nil {
			t.Fatalf("delete old site: %v", err)
		}

		if err := dynamicClient.Tracker().Add(siteUnstructured(t, replacement)); err != nil {
			t.Fatalf("add replacement site: %v", err)
		}

		return true, nil, apierrors.NewConflict(siteGVR.GroupResource(), oldSite.Name, errors.New("resourceVersion precondition failed"))
	})

	sc := &SiteController{dynamicClient: dynamicClient}

	err := sc.updateSiteStatusIfChanged(context.Background(), oldSite, 1, 1)
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("updateSiteStatusIfChanged error = %v, want Site UID change", err)
	}

	if patchCalls != 1 {
		t.Fatalf("status patch calls = %d, want 1", patchCalls)
	}

	got, err := dynamicClient.Resource(siteGVR).Get(context.Background(), replacement.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get replacement site: %v", err)
	}

	nodeCount, _, err := unstructured.NestedInt64(got.Object, "status", "nodeCount")
	if err != nil {
		t.Fatalf("read replacement status: %v", err)
	}

	if got.GetUID() != replacement.UID || nodeCount != int64(replacement.Status.NodeCount) {
		t.Fatalf("replacement Site was mutated: uid=%q status=%#v", got.GetUID(), got.Object["status"])
	}
}

func TestGatewayNodesIncludedInSiteStatusCount(t *testing.T) {
	site := unboundedv1alpha3.Site{
		TypeMeta: metav1.TypeMeta{APIVersion: unboundedv1alpha3.GroupVersion.String(), Kind: "Site"},
		ObjectMeta: metav1.ObjectMeta{
			Name:       "site-a",
			UID:        "site-uid",
			Finalizers: []string{ProtectionFinalizer},
		},
		Spec: unboundedv1alpha3.SiteSpec{NodeCidrs: []string{"10.0.0.0/24"}},
	}

	siteObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&site)
	if err != nil {
		t.Fatalf("convert site: %v", err)
	}

	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:          "SiteList",
			siteNodeSliceGVR: "SiteNodeSliceList",
		},
		&unstructured.Unstructured{Object: siteObject},
	)

	statusNodeCount := -1
	statusSliceCount := -1

	dynamicClient.PrependReactor("patch", "sites", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clienttesting.PatchAction) //nolint:errcheck
		if patchAction.GetSubresource() != "status" {
			return false, nil, nil
		}

		var patch struct {
			Status struct {
				NodeCount  int `json:"nodeCount"`
				SliceCount int `json:"sliceCount"`
			} `json:"status"`
		}
		if err := json.Unmarshal(patchAction.GetPatch(), &patch); err != nil {
			t.Fatalf("unmarshal status patch: %v", err)
		}

		statusNodeCount = patch.Status.NodeCount
		statusSliceCount = patch.Status.SliceCount

		return false, nil, nil
	})

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-a", Labels: map[string]string{"role": "gateway"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{
			Type:    corev1.NodeInternalIP,
			Address: "10.0.0.10",
		}}},
	}); err != nil {
		t.Fatalf("add gateway node: %v", err)
	}

	sc := &SiteController{
		dynamicClient: dynamicClient,
		nodeLister:    corev1listers.NewNodeLister(nodeIndexer),
		sliceInformer: &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc)},
		sitesCache:    []unboundedv1alpha3.Site{site},
		gatewayPoolsCache: []unboundednetv1alpha1.GatewayPool{{
			Spec: unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gateway"}},
		}},
	}

	if err := sc.updateAllSiteSlices(context.Background()); err != nil {
		t.Fatalf("updateAllSiteSlices: %v", err)
	}

	if statusNodeCount != 1 || statusSliceCount != 0 {
		t.Fatalf("site status counts = nodes %d, slices %d; want 1, 0", statusNodeCount, statusSliceCount)
	}
}

func TestSiteNodeSliceInformerEventsMarkSlicesDirty(t *testing.T) {
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			siteGVR:            "SiteList",
			siteNodeSliceGVR:   "SiteNodeSliceList",
			gatewayPoolGVRSite: "GatewayPoolList",
		},
	)
	dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	nodeInformerFactory := informers.NewSharedInformerFactory(kubefake.NewSimpleClientset(), 0)

	sc, err := NewSiteController(kubefake.NewSimpleClientset(), dynamicClient, dynamicInformerFactory, nodeInformerFactory)
	if err != nil {
		t.Fatalf("NewSiteController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dynamicInformerFactory.Start(ctx.Done())
	nodeInformerFactory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), sc.nodeSynced, sc.siteSynced, sc.sliceSynced, sc.gatewayPoolSynced) {
		t.Fatalf("informer caches did not sync")
	}

	resource := dynamicClient.Resource(siteNodeSliceGVR)
	slice := (&SiteController{}).buildSliceObject(
		unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a", UID: "site-uid"}},
		"site-a-0",
		0,
		nil,
	)

	created, err := resource.Create(ctx, slice, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create SiteNodeSlice: %v", err)
	}

	waitForSlicesDirty(t, ctx, sc)

	sc.slicesDirty.Store(false)
	created.SetAnnotations(map[string]string{"test": "updated"})

	created, err = resource.Update(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update SiteNodeSlice: %v", err)
	}

	waitForSlicesDirty(t, ctx, sc)

	sc.slicesDirty.Store(false)

	if err := resource.Delete(ctx, created.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete SiteNodeSlice: %v", err)
	}

	waitForSlicesDirty(t, ctx, sc)
}

func waitForSlicesDirty(t *testing.T, ctx context.Context, sc *SiteController) {
	t.Helper()

	err := utilwait.PollUntilContextTimeout(ctx, 10*time.Millisecond, time.Second, true, func(context.Context) (bool, error) {
		return sc.slicesDirty.Load(), nil
	})
	if err != nil {
		t.Fatalf("SiteNodeSlice event did not mark slices dirty: %v", err)
	}
}
