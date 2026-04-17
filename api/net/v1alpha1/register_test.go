// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestResourceAndAddToScheme tests resource and add to scheme.
func TestResourceAndAddToScheme(t *testing.T) {
	gr := Resource("sites")
	if gr.Group != GroupName || gr.Resource != "sites" {
		t.Fatalf("unexpected group resource: %#v", gr)
	}

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	objects := []runtime.Object{
		&Site{}, &SiteList{}, &SiteNodeSlice{}, &SiteNodeSliceList{},
		&GatewayPool{}, &GatewayPoolList{}, &GatewayPoolNode{}, &GatewayPoolNodeList{},
		&SitePeering{}, &SitePeeringList{},
		&SiteGatewayPoolAssignment{}, &SiteGatewayPoolAssignmentList{},
		&GatewayPoolPeering{}, &GatewayPoolPeeringList{},
	}
	for _, obj := range objects {
		kinds, _, err := scheme.ObjectKinds(obj)
		if err != nil {
			t.Fatalf("ObjectKinds(%T) error = %v", obj, err)
		}

		if len(kinds) == 0 {
			t.Fatalf("expected kinds for %T", obj)
		}

		if kinds[0].Group != GroupName || kinds[0].Version != SchemeGroupVersion.Version {
			t.Fatalf("unexpected GVK for %T: %s", obj, kinds[0].String())
		}
	}
}

// TestDeepCopySiteAndLists tests deep copy site and lists.
func TestDeepCopySiteAndLists(t *testing.T) {
	enabled := true
	priority := int32(10)
	detectMultiplier := int32(3)
	receive := intstr.FromString("300ms")
	transmit := intstr.FromInt(400)

	site := &Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a"},
		Spec: SiteSpec{
			NodeCidrs: []string{"10.0.0.0/16"},
			PodCidrAssignments: []PodCidrAssignment{
				{
					AssignmentEnabled: &enabled,
					CidrBlocks:        []string{"10.244.0.0/16"},
					NodeBlockSizes:    &NodeBlockSizes{IPv4: 24, IPv6: 80},
					NodeRegex:         []string{"^node-"},
					Priority:          &priority,
				},
			},
			ManageCniPlugin:    &enabled,
			NonMasqueradeCIDRs: []string{"172.16.0.0/12"},
			LocalCIDRs:         []string{"10.0.0.0/8"},
			HealthCheckSettings: &HealthCheckSettings{
				Enabled:          &enabled,
				DetectMultiplier: &detectMultiplier,
				ReceiveInterval:  &receive,
				TransmitInterval: &transmit,
			},
		},
		Status: SiteStatus{NodeCount: 2, SliceCount: 1},
	}

	copied := site.DeepCopy()
	if copied == nil {
		t.Fatalf("DeepCopy() returned nil")
	}

	if copied.Name != "site-a" || copied.Spec.PodCidrAssignments[0].NodeBlockSizes.IPv4 != 24 {
		t.Fatalf("unexpected copied site: %#v", copied)
	}

	site.Spec.NodeCidrs[0] = "10.99.0.0/16"
	site.Spec.PodCidrAssignments[0].CidrBlocks[0] = "10.250.0.0/16"

	site.Spec.HealthCheckSettings.DetectMultiplier = ptrInt32(9)
	if copied.Spec.NodeCidrs[0] != "10.0.0.0/16" {
		t.Fatalf("expected deep-copied NodeCidrs to be isolated")
	}

	if copied.Spec.PodCidrAssignments[0].CidrBlocks[0] != "10.244.0.0/16" {
		t.Fatalf("expected deep-copied assignment CidrBlocks to be isolated")
	}

	if copied.Spec.HealthCheckSettings.DetectMultiplier == nil || *copied.Spec.HealthCheckSettings.DetectMultiplier != 3 {
		t.Fatalf("expected deep-copied health check settings to be isolated")
	}

	if site.DeepCopyObject() == nil {
		t.Fatalf("expected Site.DeepCopyObject() not nil")
	}

	siteList := &SiteList{Items: []Site{*site}}
	if got := siteList.DeepCopy(); got == nil || len(got.Items) != 1 {
		t.Fatalf("unexpected SiteList deepcopy result: %#v", got)
	}

	if siteList.DeepCopyObject() == nil {
		t.Fatalf("expected SiteList.DeepCopyObject() not nil")
	}

	nodeSlice := &SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "site-a-0"},
		SiteName:   "site-a",
		SliceIndex: 0,
		Nodes: []NodeInfo{{
			Name:               "node1",
			WireGuardPublicKey: "pub",
			InternalIPs:        []string{"10.0.0.10"},
			PodCIDRs:           []string{"10.244.1.0/24"},
		}},
	}
	copiedSlice := nodeSlice.DeepCopy()

	nodeSlice.Nodes[0].InternalIPs[0] = "10.0.0.20"
	if copiedSlice.Nodes[0].InternalIPs[0] != "10.0.0.10" {
		t.Fatalf("expected deep-copied SiteNodeSlice nodes to be isolated")
	}

	if nodeSlice.DeepCopyObject() == nil {
		t.Fatalf("expected SiteNodeSlice.DeepCopyObject() not nil")
	}

	nodeSliceList := &SiteNodeSliceList{Items: []SiteNodeSlice{*nodeSlice}}
	if got := nodeSliceList.DeepCopy(); got == nil || len(got.Items) != 1 {
		t.Fatalf("unexpected SiteNodeSliceList deepcopy result: %#v", got)
	}

	if nodeSliceList.DeepCopyObject() == nil {
		t.Fatalf("expected SiteNodeSliceList.DeepCopyObject() not nil")
	}
}

// TestDeepCopyGatewayAndPeeringTypes tests deep copy gateway and peering types.
func TestDeepCopyGatewayAndPeeringTypes(t *testing.T) {
	gatewayNode := &GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-a"},
		Spec: GatewayNodeSpec{
			NodeName:    "node-a",
			GatewayPool: "pool-a",
			Site:        "site-a",
		},
		Status: GatewayNodeStatus{
			Routes: map[string]GatewayNodeRoute{
				"10.244.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]GatewayNodePathHop{
						{{Type: "Site", Name: "site-a"}, {Type: "GatewayPool", Name: "pool-a"}},
					},
				},
			},
		},
	}
	copiedGatewayNode := gatewayNode.DeepCopy()

	gatewayNode.Status.Routes["10.244.0.0/16"] = GatewayNodeRoute{Type: "NodeCidr"}
	if copiedGatewayNode.Status.Routes["10.244.0.0/16"].Type != "RoutedCidr" {
		t.Fatalf("expected deep-copied GatewayNode routes to be isolated")
	}

	if gatewayNode.DeepCopyObject() == nil {
		t.Fatalf("expected GatewayNode.DeepCopyObject() not nil")
	}

	gatewayPool := &GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: GatewayPoolSpec{
			Type:                "External",
			NodeSelector:        map[string]string{"role": "gateway"},
			RoutedCidrs:         []string{"10.250.0.0/16"},
			HealthCheckSettings: &HealthCheckSettings{DetectMultiplier: ptrInt32(5)},
		},
		Status: GatewayPoolStatus{
			Nodes: []GatewayNodeInfo{{
				Name:               "node-a",
				SiteName:           "site-a",
				InternalIPs:        []string{"10.0.0.1"},
				ExternalIPs:        []string{"52.0.0.1"},
				HealthEndpoints:    []string{"10.0.0.1"},
				WireGuardPublicKey: "pub",
				PodCIDRs:           []string{"10.244.1.0/24"},
			}},
			ConnectedSites: []string{"site-b"},
			ReachableSites: []string{"site-b", "site-c"},
		},
	}
	copiedGatewayPool := gatewayPool.DeepCopy()
	gatewayPool.Spec.NodeSelector["role"] = "changed"
	gatewayPool.Status.Nodes[0].ExternalIPs[0] = "52.0.0.2"

	if copiedGatewayPool.Spec.NodeSelector["role"] != "gateway" {
		t.Fatalf("expected deep-copied GatewayPool NodeSelector to be isolated")
	}

	if copiedGatewayPool.Status.Nodes[0].ExternalIPs[0] != "52.0.0.1" {
		t.Fatalf("expected deep-copied GatewayPool status to be isolated")
	}

	if gatewayPool.DeepCopyObject() == nil {
		t.Fatalf("expected GatewayPool.DeepCopyObject() not nil")
	}

	if (&GatewayPoolList{Items: []GatewayPool{*gatewayPool}}).DeepCopyObject() == nil {
		t.Fatalf("expected GatewayPoolList.DeepCopyObject() not nil")
	}

	if (&GatewayPoolNodeList{Items: []GatewayPoolNode{*gatewayNode}}).DeepCopyObject() == nil {
		t.Fatalf("expected GatewayPoolNodeList.DeepCopyObject() not nil")
	}

	peering := &SitePeering{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-a"},
		Spec: SitePeeringSpec{
			Sites:               []string{"site-a", "site-b"},
			HealthCheckSettings: &HealthCheckSettings{Enabled: ptrBool(true)},
		},
	}
	copiedPeering := peering.DeepCopy()

	peering.Spec.Sites[0] = "site-x"
	if copiedPeering.Spec.Sites[0] != "site-a" {
		t.Fatalf("expected deep-copied SitePeering sites to be isolated")
	}

	if peering.DeepCopyObject() == nil {
		t.Fatalf("expected SitePeering.DeepCopyObject() not nil")
	}

	if (&SitePeeringList{Items: []SitePeering{*peering}}).DeepCopyObject() == nil {
		t.Fatalf("expected SitePeeringList.DeepCopyObject() not nil")
	}

	// Cover simple deepcopy structs.
	if (&GatewayPoolRoute{CIDR: "10.0.0.0/16", Origin: GatewayPoolRouteOrigin{Site: "site-a"}}).DeepCopy() == nil {
		t.Fatalf("expected GatewayPoolRoute.DeepCopy() not nil")
	}

	if (&GatewayPoolRouteOrigin{Site: "site-a", GatewayPool: "pool-a"}).DeepCopy() == nil {
		t.Fatalf("expected GatewayPoolRouteOrigin.DeepCopy() not nil")
	}

	if (&NodeInfo{Name: "n", InternalIPs: []string{"10.0.0.1"}, PodCIDRs: []string{"10.244.1.0/24"}}).DeepCopy() == nil {
		t.Fatalf("expected NodeInfo.DeepCopy() not nil")
	}

	if (&NodeBlockSizes{IPv4: 24, IPv6: 80}).DeepCopy() == nil {
		t.Fatalf("expected NodeBlockSizes.DeepCopy() not nil")
	}
}

func TestSpecEnabled(t *testing.T) {
	if !SpecEnabled(nil) {
		t.Fatalf("nil enabled should default to true")
	}

	enabled := true
	if !SpecEnabled(&enabled) {
		t.Fatalf("true enabled should be true")
	}

	disabled := false
	if SpecEnabled(&disabled) {
		t.Fatalf("false enabled should be false")
	}
}

func ptrBool(v bool) *bool {
	return &v
}

func ptrInt32(v int32) *int32 {
	return &v
}
