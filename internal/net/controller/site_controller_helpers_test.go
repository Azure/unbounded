// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	site := unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: unboundednetv1alpha1.SiteSpec{
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

	enabled := sc.collectEnabledAssignments([]unboundednetv1alpha1.Site{site})
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
		site:  unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}},
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
		site:       unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}},
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
			Labels:      map[string]string{SiteLabelKey: "site-a", "role": "gateway"},
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

	noOverlap := []unboundednetv1alpha1.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.0.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.1.0.0/16"}}},
	}
	if err := validateSiteCIDRsNoOverlap(noOverlap); err != nil {
		t.Fatalf("expected non-overlapping CIDRs, got %v", err)
	}

	exactOverlap := []unboundednetv1alpha1.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.2.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.2.0.0/16"}}},
	}
	if err := validateSiteCIDRsNoOverlap(exactOverlap); err == nil {
		t.Fatalf("expected exact CIDR overlap to fail")
	}

	rangeOverlap := []unboundednetv1alpha1.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "site-a"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.3.0.0/16"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}, Spec: unboundednetv1alpha1.SiteSpec{NodeCidrs: []string{"10.3.1.0/24"}}},
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
