// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
)

// TestIntOrStringMilliseconds tests IntOrStringMilliseconds.
func TestIntOrStringMilliseconds(t *testing.T) {
	tests := []struct {
		name  string
		value *intstr.IntOrString
		want  int
		ok    bool
	}{
		{name: "nil", value: nil, want: 0, ok: false},
		{name: "int", value: ptrIntOrString(intstr.FromInt(250)), want: 250, ok: true},
		{name: "duration", value: ptrIntOrString(intstr.FromString("300ms")), want: 300, ok: true},
		{name: "numeric string", value: ptrIntOrString(intstr.FromString("450")), want: 450, ok: true},
		{name: "duration string", value: ptrIntOrString(intstr.FromString("10s")), want: 10000, ok: true},
		{name: "int type with both fields uses int value", value: &intstr.IntOrString{Type: intstr.Int, IntVal: 300, StrVal: "10s"}, want: 300, ok: true},
		{name: "duration string with int type ignored", value: &intstr.IntOrString{Type: intstr.Int, StrVal: "300ms"}, want: 0, ok: false},
		{name: "numeric string with int type", value: &intstr.IntOrString{Type: intstr.Int, StrVal: "450"}, want: 450, ok: true},
		{name: "empty string", value: ptrIntOrString(intstr.FromString(" ")), want: 0, ok: false},
		{name: "invalid string", value: ptrIntOrString(intstr.FromString("bad")), want: 0, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := intOrStringMilliseconds(tt.value, "test.spec.healthCheckSettings.interval")
			if got != tt.want || ok != tt.ok {
				t.Fatalf("intOrStringMilliseconds() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestHealthCheckProfileSettingsHelpers tests health check profile settings helpers.
func TestHealthCheckProfileSettingsHelpers(t *testing.T) {
	enabled, profile := healthCheckProfileFromSettings(nil, "test-gvr/test")
	if !enabled {
		t.Fatalf("expected health check enabled by default")
	}

	defaultSettings := healthcheck.DefaultSettings()
	if !healthCheckProfilesEqual(profile, defaultSettings) {
		t.Fatalf("expected default profile, got %#v", profile)
	}

	settings := &unboundednetv1alpha1.HealthCheckSettings{
		Enabled:          ptrBool(false),
		DetectMultiplier: ptrInt32(5),
		ReceiveInterval:  ptrIntOrString(intstr.FromString("150ms")),
		TransmitInterval: ptrIntOrString(intstr.FromInt(275)),
	}

	enabled, profile = healthCheckProfileFromSettings(settings, "test-gvr/test")
	if enabled {
		t.Fatalf("expected explicit disabled health check")
	}

	want := healthcheck.HealthCheckSettings{
		MaxBackoff:       120 * time.Second,
		DetectMultiplier: 5,
		ReceiveInterval:  150 * time.Millisecond,
		TransmitInterval: 275 * time.Millisecond,
	}
	if !healthCheckProfilesEqual(profile, want) {
		t.Fatalf("unexpected profile %#v, want %#v", profile, want)
	}

	siteMap := map[string]*unboundedv1alpha3.Site{
		"site-a": {
			Spec: unboundedv1alpha3.SiteSpec{
				HealthCheckSettings: &unboundednetv1alpha1.HealthCheckSettings{
					Enabled:          ptrBool(true),
					DetectMultiplier: ptrInt32(7),
					ReceiveInterval:  ptrIntOrString(intstr.FromString("200ms")),
					TransmitInterval: ptrIntOrString(intstr.FromString("400")),
				},
			},
		},
	}

	enabled, profile = effectiveSiteHealthCheckSettings("site-a", siteMap)
	if !enabled {
		t.Fatalf("expected enabled from site settings")
	}

	siteWant := healthcheck.HealthCheckSettings{
		MaxBackoff:       120 * time.Second,
		DetectMultiplier: 7,
		ReceiveInterval:  200 * time.Millisecond,
		TransmitInterval: 400 * time.Millisecond,
	}
	if !healthCheckProfilesEqual(profile, siteWant) {
		t.Fatalf("unexpected site profile %#v, want %#v", profile, siteWant)
	}

	if got := healthCheckProfileNameForSite("alpha"); got != "s-alpha" {
		t.Fatalf("unexpected site profile name: %s", got)
	}

	if got := healthCheckProfileNameForGatewayPool("pool-a"); got != "gp-pool-a" {
		t.Fatalf("unexpected gateway pool profile name: %s", got)
	}

	if got := healthCheckProfileNameForSitePeering("peer-a"); got != "sp-peer-a" {
		t.Fatalf("unexpected site peering profile name: %s", got)
	}

	if got := healthCheckProfileNameForSiteGatewayPoolAssignment("asg-a"); got != "sgpa-asg-a" {
		t.Fatalf("unexpected site-gateway-pool-assignment profile name: %s", got)
	}

	if got := healthCheckProfileNameForGatewayPoolPeering("gpp-a"); got != "gpp-gpp-a" {
		t.Fatalf("unexpected gateway-pool-peering profile name: %s", got)
	}
}

// TestResolveMeshPeerHealthCheckProfileName tests ResolveMeshPeerHealthCheckProfileName.
func TestResolveMeshPeerHealthCheckProfileName(t *testing.T) {
	peer := meshPeerInfo{SiteName: "site-a"}
	if got := resolveMeshPeerHealthCheckProfileName(
		false,
		peer,
		"site-a",
		map[string]string{"site-a": "s-site-a"},
		map[string]string{"site-b": "sp-site-b"},
		map[string]string{"site-c": "sgpa-site-c"},
	); got != "s-site-a" {
		t.Fatalf("expected site profile for same-site peer, got %q", got)
	}

	peer = meshPeerInfo{SiteName: "site-b"}
	if got := resolveMeshPeerHealthCheckProfileName(
		false,
		peer,
		"site-a",
		map[string]string{"site-a": "s-site-a"},
		map[string]string{"site-b": "sp-site-b"},
		map[string]string{"site-b": "sgpa-site-b"},
	); got != "sgpa-site-b" {
		t.Fatalf("expected assignment profile to override peering (more specific), got %q", got)
	}

	peer = meshPeerInfo{SiteName: "site-c"}
	if got := resolveMeshPeerHealthCheckProfileName(
		false,
		peer,
		"site-a",
		map[string]string{"site-a": "s-site-a"},
		map[string]string{},
		map[string]string{"site-c": "sgpa-site-c"},
	); got != "sgpa-site-c" {
		t.Fatalf("expected assignment profile fallback for non-peered site, got %q", got)
	}

	peer = meshPeerInfo{SiteName: "site-a", HealthCheckProfileName: "gp-pool-a"}
	if got := resolveMeshPeerHealthCheckProfileName(
		false,
		peer,
		"site-a",
		map[string]string{"site-a": "s-site-a"},
		map[string]string{},
		map[string]string{},
	); got != "gp-pool-a" {
		t.Fatalf("expected explicit per-link profile override, got %q", got)
	}
}

// TestResolveGatewayPeerHealthCheckProfileName tests ResolveGatewayPeerHealthCheckProfileName.
func TestResolveGatewayPeerHealthCheckProfileName(t *testing.T) {
	peer := gatewayPeerInfo{PoolName: "pool-a", SiteName: "s1", HealthCheckProfileName: "gpp-peer-a"}
	if got := resolveGatewayPeerHealthCheckProfileName(
		true,
		"s1",
		peer,
		map[string]string{"s1|pool-a": "sgpa-pool-a"},
		map[string]string{"pool-a": "gp-pool-a"},
	); got != "gpp-peer-a" {
		t.Fatalf("expected gateway-pool-peering override for gateway-to-gateway link, got %q", got)
	}

	peer = gatewayPeerInfo{PoolName: "pool-a", SiteName: "s1"}
	if got := resolveGatewayPeerHealthCheckProfileName(
		false,
		"s1",
		peer,
		map[string]string{"s1|pool-a": "sgpa-pool-a"},
		map[string]string{"pool-a": "gp-pool-a"},
	); got != "sgpa-pool-a" {
		t.Fatalf("expected assignment profile for site-to-gateway link, got %q", got)
	}

	if got := resolveGatewayPeerHealthCheckProfileName(
		true,
		"s1",
		peer,
		map[string]string{"s1|pool-a": "sgpa-pool-a"},
		map[string]string{"pool-a": "gp-pool-a"},
	); got != "gp-pool-a" {
		t.Fatalf("expected gateway pool profile for same-pool gateway link fallback, got %q", got)
	}
}

// TestTunnelMTUFromSpec tests tunnelMTUFromSpec.
func TestTunnelMTUFromSpec(t *testing.T) {
	if got := tunnelMTUFromSpec(nil); got != 0 {
		t.Fatalf("expected 0 for nil, got %d", got)
	}

	v := int32(1400)
	if got := tunnelMTUFromSpec(&v); got != 1400 {
		t.Fatalf("expected 1400, got %d", got)
	}
}

func TestMergeLowestTunnelMTU(t *testing.T) {
	tunnelMTUs := map[string]int{"pool-a": 1400}

	mergeLowestTunnelMTU(tunnelMTUs, "pool-a", 1450)
	mergeLowestTunnelMTU(tunnelMTUs, "pool-a", 1280)
	mergeLowestTunnelMTU(tunnelMTUs, " pool-b ", 1300)
	mergeLowestTunnelMTU(tunnelMTUs, "", 1200)
	mergeLowestTunnelMTU(tunnelMTUs, "pool-c", 0)

	if got := tunnelMTUs["pool-a"]; got != 1280 {
		t.Fatalf("pool-a MTU = %d, want 1280", got)
	}

	if got := tunnelMTUs["pool-b"]; got != 1300 {
		t.Fatalf("pool-b MTU = %d, want 1300", got)
	}

	if _, exists := tunnelMTUs["pool-c"]; exists {
		t.Fatal("zero MTU should not create a scope")
	}
}

func TestGatewayMeshTransitCaps(t *testing.T) {
	routes := map[string]unboundednetv1alpha1.GatewayNodeRoute{
		"10.0.0.0/24": {Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{{
			{Type: "Site", Name: "site-b"}, {Type: "GatewayPool", Name: "pool-g"},
		}}},
	}

	peerings := []unboundednetv1alpha1.SitePeering{
		{Spec: unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"site-b", "site-c"}, TunnelMTU: ptrInt32(1200)}},
		{Spec: unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"site-x", "site-c"}, TunnelMTU: ptrInt32(900)}},
	}
	if got := gatewayMeshTransitTunnelMTU("pool-g", "site-c", routes, nil, nil, sitePeeringEdgeTunnelMTUs(peerings)); got != 1200 {
		t.Fatalf("gateway-to-origin mesh edge cap=%d, want 1200", got)
	}

	if got := gatewayMeshTransitTunnelMTU("pool-g", "site-b", routes, nil, nil, sitePeeringEdgeTunnelMTUs(peerings)); got != 0 {
		t.Fatalf("same-Site edge constrained by unrelated peering: %d", got)
	}

	pools := map[string]*unboundednetv1alpha1.GatewayPool{
		"pool-g": {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{SiteName: "site-c"}}}},
	}
	if got := gatewayMeshTransitTunnelMTU("pool-upstream", "site-a", routes, pools, nil, sitePeeringEdgeTunnelMTUs(peerings)); got != 1200 {
		t.Fatalf("intermediate pool mesh edge cap=%d, want 1200", got)
	}

	if got := resolveGatewayPeerTunnelMTU(1420, "site-a", gatewayPeerInfo{SiteName: "site-c"},
		map[string]int{"site-a": 1420, "site-c": 1200}, nil, nil); got != 1200 {
		t.Fatalf("next-hop gateway Site cap=%d, want 1200", got)
	}

	if got := gatewayMeshTransitTunnelMTU("pool-g", "site-c", nil, pools, nil, sitePeeringEdgeTunnelMTUs(peerings)); got != 0 {
		t.Fatalf("absent transit routes constrained MTU: %d", got)
	}
}

func TestIntermediateGatewaySiteMTUCaps(t *testing.T) {
	routes := map[string]unboundednetv1alpha1.GatewayNodeRoute{
		"10.0.0.0/24": {Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{{
			{Type: "Site", Name: "site-b"},
			{Type: "GatewayPool", Name: "pool-h"},
			{Type: "GatewayPool", Name: "pool-j"},
			{Type: "GatewayPool", Name: "pool-g"},
		}}},
	}
	pools := map[string]*unboundednetv1alpha1.GatewayPool{
		"pool-h":    {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{SiteName: "site-b"}}}},
		"pool-j":    {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{SiteName: "site-d"}, {SiteName: "site-e"}}}},
		"pool-g":    {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{SiteName: "unused-advertiser"}}}},
		"unrelated": {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{SiteName: "unrelated"}}}},
	}

	caps := map[string]int{"site-b": 1420, "site-d": 1200, "site-e": 1300, "site-c": 1420, "unrelated": 900, "unused-advertiser": 800}
	if got := gatewayMeshTransitTunnelMTU("pool-g", "site-c", routes, pools, caps, nil); got != 1200 {
		t.Fatalf("three-pool candidate Site cap=%d, want 1200", got)
	}

	if got := gatewayMeshTransitTunnelMTU("pool-g", "site-c", nil, pools, caps, nil); got != 0 {
		t.Fatalf("unused pools constrained MTU without routes: %d", got)
	}
}

func TestPublishedGatewayForwardingMTUs(t *testing.T) {
	mesh := []meshPeerInfo{
		{SiteName: "b", TunnelProtocol: "WireGuard", TunnelMTU: 1200},
		{SiteName: "d", TunnelProtocol: "WireGuard", TunnelMTU: 1400},
	}

	sharedMTU := protocolInterfaceMTU(1420, 0, 1500, "WireGuard", mesh, nil)
	if sharedMTU != 1200 {
		t.Fatalf("shared mesh MTU=%d, want 1200", sharedMTU)
	}

	nodes := map[string]*unboundednetv1alpha1.GatewayPoolNode{
		"g":         {Status: unboundednetv1alpha1.GatewayNodeStatus{ForwardingMTU: ptrInt32(int32(sharedMTU))}},
		"h":         {Status: unboundednetv1alpha1.GatewayNodeStatus{ForwardingMTU: ptrInt32(1100)}},
		"unrelated": {Status: unboundednetv1alpha1.GatewayNodeStatus{ForwardingMTU: ptrInt32(900)}},
	}

	pools := map[string]*unboundednetv1alpha1.GatewayPool{
		"pool-g": {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "g"}, {Name: "unrelated"}}}},
		"pool-h": {Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "h"}}}},
	}
	for _, intermediate := range []bool{false, true} {
		path := []unboundednetv1alpha1.GatewayNodePathHop{{Type: "Site", Name: "d"}}
		if intermediate {
			path = append(path, unboundednetv1alpha1.GatewayNodePathHop{Type: "GatewayPool", Name: "pool-h"})
		}

		path = append(path, unboundednetv1alpha1.GatewayNodePathHop{Type: "GatewayPool", Name: "pool-g"})
		peers := []gatewayPeerInfo{{Name: "g", PoolName: "pool-g", LearnedRoutes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
			"10.0.0.0/24": {Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{path}},
		}}}
		applyPublishedGatewayMTUs(peers, pools, nodes)

		want := 1200
		if intermediate {
			want = 1100
		}

		if peers[0].TransitTunnelMTU != want {
			t.Fatalf("intermediate=%v: cap=%d, want %d", intermediate, peers[0].TransitTunnelMTU, want)
		}
	}

	nodes["g"].Status.ForwardingMTU = ptrInt32(1400)
	peers := []gatewayPeerInfo{{Name: "g", PoolName: "pool-g"}}
	applyPublishedGatewayMTUs(peers, pools, nodes)

	if peers[0].TransitTunnelMTU != 1400 {
		t.Fatalf("raised local cap remained pinned: %d", peers[0].TransitTunnelMTU)
	}

	nodes["g"].Status.ForwardingMTU = nil
	peers = []gatewayPeerInfo{{Name: "g", PoolName: "pool-g", TransitTunnelMTU: 1300}}
	applyPublishedGatewayMTUs(peers, pools, nodes)

	if peers[0].TransitTunnelMTU != 1300 {
		t.Fatal("legacy gateway without published MTU discarded topology cap")
	}
}

func TestSitePeeringEdgeTunnelMTUs(t *testing.T) {
	peerings := []unboundednetv1alpha1.SitePeering{
		{Spec: unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"c", "b", "a"}, TunnelMTU: ptrInt32(1400)}},
		{Spec: unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"b", "a"}, TunnelMTU: ptrInt32(1200)}},
		{Spec: unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"b", "a"}, TunnelMTU: ptrInt32(900), Enabled: new(false)}},
		{Spec: unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"unused", "a"}}},
	}

	got := sitePeeringEdgeTunnelMTUs(peerings)
	if len(got) != 3 || got[sitePairMTUKey{"a", "b"}] != 1200 ||
		got[sitePairMTUKey{"b", "c"}] != 1400 || got[sitePairMTUKey{"a", "c"}] != 1400 {
		t.Fatalf("unexpected enabled peering minima: %v", got)
	}
}

func TestAssignmentEdgeTunnelMTUs(t *testing.T) {
	assignments := []unboundednetv1alpha1.SiteGatewayPoolAssignment{
		{Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"a", "b"}, GatewayPools: []string{"g", "h"}, TunnelMTU: ptrInt32(1400),
		}},
		{Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"b"}, GatewayPools: []string{"g"}, TunnelMTU: ptrInt32(1200),
		}},
		{Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"a"}, GatewayPools: []string{"g"}, TunnelMTU: ptrInt32(900), Enabled: new(false),
		}},
		{Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"unused"}, GatewayPools: []string{"g"},
		}},
	}

	got := assignmentEdgeTunnelMTUs(assignments)
	if len(got) != 4 || got[sitePoolMTUKey{"a", "g"}] != 1400 ||
		got[sitePoolMTUKey{"b", "g"}] != 1200 || got[sitePoolMTUKey{"b", "h"}] != 1400 {
		t.Fatalf("unexpected enabled edge minima: %v", got)
	}
}

func TestLearnedGatewayTransitPathCaps(t *testing.T) {
	path := []unboundednetv1alpha1.GatewayNodePathHop{
		{Type: "Site", Name: "site-b"},
		{Type: "GatewayPool", Name: "pool-h"},
		{Type: "GatewayPool", Name: "pool-j"},
		{Type: "GatewayPool", Name: "pool-g"},
	}
	routes := map[string]unboundednetv1alpha1.GatewayNodeRoute{
		"10.0.0.0/24": {Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{path}},
	}

	for _, tc := range []struct {
		name        string
		sites       map[string]int
		pools       map[string]int
		assignments []unboundednetv1alpha1.SiteGatewayPoolAssignment
		peerings    []unboundednetv1alpha1.GatewayPoolPeering
		want        int
	}{
		{name: "origin Site", sites: map[string]int{"site-b": 1200, "unrelated": 900}, want: 1200},
		{name: "downstream pool", pools: map[string]int{"pool-h": 1250, "unrelated": 900}, want: 1250},
		{name: "assignment edge", assignments: []unboundednetv1alpha1.SiteGatewayPoolAssignment{
			{Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{Sites: []string{"site-b"}, GatewayPools: []string{"pool-h"}, TunnelMTU: ptrInt32(1100)}},
		}, want: 1100},
		{name: "assignment nonadjacent", assignments: []unboundednetv1alpha1.SiteGatewayPoolAssignment{
			{Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{Sites: []string{"site-b"}, GatewayPools: []string{"pool-g"}, TunnelMTU: ptrInt32(1100)}},
		}},
		{name: "adjacent peering", peerings: []unboundednetv1alpha1.GatewayPoolPeering{
			{Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{GatewayPools: []string{"pool-h", "pool-j"}, TunnelMTU: ptrInt32(1300)}},
		}, want: 1300},
		{name: "nonadjacent peering", peerings: []unboundednetv1alpha1.GatewayPoolPeering{
			{Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{GatewayPools: []string{"pool-h", "pool-g"}, TunnelMTU: ptrInt32(1000)}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := learnedGatewayTransitTunnelMTU(routes, tc.sites, tc.pools, assignmentEdgeTunnelMTUs(tc.assignments), tc.peerings)
			if got != tc.want {
				t.Fatalf("transit MTU=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestForwardingAwareTunnelMTU(t *testing.T) {
	siteMTUs := map[string]int{
		"site-a": 1450,
		"site-b": 1300,
	}
	poolMTUs := map[string]int{
		"pool-g": 1100,
		"pool-h": 1400,
	}

	meshPeer := meshPeerInfo{SiteName: "site-b"}
	if got := resolveMeshPeerTunnelMTU(0, meshPeer, "site-a", siteMTUs, map[string]int{"site-b": 1350}, nil, false); got != 1300 {
		t.Fatalf("A-B mesh MTU = %d, want remote Site minimum 1300", got)
	}

	// Site A meshes Site B, and only Site B is assigned to pool G. Mesh peers
	// advertise B's PodCIDRs, not transit through B to G, so G must not lower A.
	if got := resolveCNIConfigMTU(0, siteMTUs["site-a"], []meshPeerInfo{{TunnelMTU: 1300}}, nil, 1500); got != 1300 {
		t.Fatalf("A-B-G unsupported transit MTU = %d, want 1300", got)
	}

	gatewayPeer := gatewayPeerInfo{PoolName: "pool-g"}
	if got := resolveGatewayPeerTunnelMTU(0, "site-a", gatewayPeer, siteMTUs, map[string]int{"pool-g": 1250}, poolMTUs); got != 1100 {
		t.Fatalf("direct gateway MTU = %d, want pool minimum 1100", got)
	}

	peering := unboundednetv1alpha1.GatewayPoolPeering{
		Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{
			GatewayPools: []string{"pool-g", "pool-h"},
			TunnelMTU:    ptrInt32(1200),
		},
	}

	transitMTU := gatewayPoolPeeringTunnelMTU([]string{"pool-g"}, "pool-h", []unboundednetv1alpha1.GatewayPoolPeering{peering})
	if got := resolveGatewayPeerTunnelMTU(0, "site-a", gatewayPeerInfo{PoolName: "pool-h", TransitTunnelMTU: transitMTU}, siteMTUs, nil, poolMTUs); got != 1200 {
		t.Fatalf("gateway multi-pool routed path MTU = %d, want peering minimum 1200", got)
	}

	learnedRoutes := map[string]unboundednetv1alpha1.GatewayNodeRoute{
		"10.0.0.0/24": {
			Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{{
				{Type: "GatewayPool", Name: "pool-h"},
				{Type: "GatewayPool", Name: "pool-g"},
			}},
		},
	}
	if got := learnedGatewayTransitTunnelMTU(learnedRoutes, nil, nil, nil, []unboundednetv1alpha1.GatewayPoolPeering{peering}); got != 1200 {
		t.Fatalf("worker learned gateway transit MTU = %d, want 1200", got)
	}

	if got := learnedGatewayTransitTunnelMTU(nil, nil, nil, nil, []unboundednetv1alpha1.GatewayPoolPeering{peering}); got != 0 {
		t.Fatalf("gateway peering without a learned forwarding path constrained worker MTU: %d", got)
	}

	if got := resolveMeshPeerTunnelMTU(1420, meshPeerInfo{SiteName: "missing"}, "site-a", siteMTUs, nil, nil, false); got != 1420 {
		t.Fatalf("missing remote resources MTU = %d, want configured default 1420", got)
	}

	if got := resolveTunnelMTU(0, -1, 0, 1280); got != 1280 {
		t.Fatalf("invalid non-positive MTUs changed result: %d", got)
	}
}

// TestResolveTunnelMTU tests resolveTunnelMTU.
func TestResolveTunnelMTU(t *testing.T) {
	// No overrides -- global wins
	if got := resolveTunnelMTU(1420); got != 1420 {
		t.Fatalf("expected 1420, got %d", got)
	}
	// Lower CRD value wins
	if got := resolveTunnelMTU(1420, 1300, 1400); got != 1300 {
		t.Fatalf("expected 1300, got %d", got)
	}
	// Zero values are ignored
	if got := resolveTunnelMTU(1420, 0, 1400); got != 1400 {
		t.Fatalf("expected 1400, got %d", got)
	}
	// Higher CRD value does not override global
	if got := resolveTunnelMTU(1420, 1500); got != 1420 {
		t.Fatalf("expected 1420, got %d", got)
	}
	// Zero global means automatic/unset, so a CRD value can establish the MTU.
	if got := resolveTunnelMTU(0, 1420); got != 1420 {
		t.Fatalf("expected 1420 with automatic global MTU, got %d", got)
	}
}

// TestResolveMeshPeerTunnelMTU tests resolveMeshPeerTunnelMTU.
func TestResolveMeshPeerTunnelMTU(t *testing.T) {
	// Same-site peer: only site MTU applies
	peer := meshPeerInfo{SiteName: "site-a"}

	got := resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{"site-a": 1300},
		map[string]int{"site-b": 1200},
		map[string]int{"site-b": 1100},
		false,
	)
	if got != 1300 {
		t.Fatalf("expected 1300 (site MTU), got %d", got)
	}

	// An ordinary worker's direct mesh path does not transit the gateway
	// assignment, so the assignment MTU must not constrain that peer.
	peer = meshPeerInfo{SiteName: "site-b"}

	got = resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{"site-a": 1350, "site-b": 1300},
		map[string]int{"site-b": 1280},
		map[string]int{"site-b": 1100},
		false,
	)
	if got != 1280 {
		t.Fatalf("expected 1280 (peering MTU wins without gateway assignment), got %d", got)
	}

	// A gateway's mesh link to an assigned Site is part of the assignment
	// forwarding path, so the assignment MTU applies there.
	got = resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{"site-a": 1350, "site-b": 1300},
		map[string]int{"site-b": 1280},
		map[string]int{"site-b": 1100},
		true,
	)
	if got != 1100 {
		t.Fatalf("expected 1100 (gateway assignment MTU wins), got %d", got)
	}

	// No CRD overrides -- global wins
	peer = meshPeerInfo{SiteName: "site-b"}

	got = resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{},
		map[string]int{},
		map[string]int{},
		false,
	)
	if got != 1420 {
		t.Fatalf("expected 1420 (global MTU), got %d", got)
	}
}

// TestResolveGatewayPeerTunnelMTU tests resolveGatewayPeerTunnelMTU.
func TestResolveGatewayPeerTunnelMTU(t *testing.T) {
	peer := gatewayPeerInfo{PoolName: "pool-a"}

	// Pool MTU lower than site MTU
	got := resolveGatewayPeerTunnelMTU(1420, "site-a", peer,
		map[string]int{"site-a": 1350},
		map[string]int{"pool-a": 1300},
		map[string]int{"pool-a": 1280},
	)
	if got != 1280 {
		t.Fatalf("expected 1280 (pool MTU wins), got %d", got)
	}

	// Assignment pool MTU lower than pool MTU
	got = resolveGatewayPeerTunnelMTU(1420, "site-a", peer,
		map[string]int{"site-a": 1350},
		map[string]int{"pool-a": 1250},
		map[string]int{"pool-a": 1300},
	)
	if got != 1250 {
		t.Fatalf("expected 1250 (assignment pool MTU wins), got %d", got)
	}

	// No overrides -- global wins
	got = resolveGatewayPeerTunnelMTU(1420, "site-a", peer,
		map[string]int{},
		map[string]int{},
		map[string]int{},
	)
	if got != 1420 {
		t.Fatalf("expected 1420 (global MTU), got %d", got)
	}
}

// TestParseSitePeering tests ParseSitePeering.
func TestParseSitePeering(t *testing.T) {
	src := &unboundednetv1alpha1.SitePeering{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-a"},
		Spec: unboundednetv1alpha1.SitePeeringSpec{
			Sites: []string{"site1", "site2"},
		},
	}

	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal site peering: %v", err)
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal site peering map: %v", err)
	}

	parsed, err := parseSitePeering(&unstructured.Unstructured{Object: obj})
	if err != nil {
		t.Fatalf("parseSitePeering returned error: %v", err)
	}

	if parsed.Name != "peer-a" || len(parsed.Spec.Sites) != 2 || parsed.Spec.Sites[0] != "site1" {
		t.Fatalf("unexpected site peering parsed: %#v", parsed)
	}
}

// TestMergeAssignmentBFDStateCombinesMultipleAssignmentsByPool tests MergeAssignmentBFDStateCombinesMultipleAssignmentsByPool.
func TestMergeAssignmentBFDStateCombinesMultipleAssignmentsByPool(t *testing.T) {
	hcProfiles := make(map[string]healthcheck.HealthCheckSettings)
	assignmentPoolHealthCheckProfileNames := make(map[string]string)
	assignmentPoolBFDSourceAssignment := make(map[string]string)
	assignmentSiteHealthCheckProfileNames := make(map[string]string)
	assignmentSiteBFDSourceAssignment := make(map[string]string)

	includedPools := map[string]struct{}{"s1gw1": {}}

	a1 := unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "s1gw1-site1"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			GatewayPools: []string{"s1gw1"},
			Sites:        []string{"site1"},
		},
	}
	a2 := unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "s1gw1-site2"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			GatewayPools: []string{"s1gw1"},
			Sites:        []string{"site2"},
		},
	}

	mergeAssignmentHealthCheckState(
		a1,
		"site1",
		includedPools,
		hcProfiles,
		nil,
		assignmentPoolHealthCheckProfileNames,
		assignmentPoolBFDSourceAssignment,
		assignmentSiteHealthCheckProfileNames,
		assignmentSiteBFDSourceAssignment,
	)
	mergeAssignmentHealthCheckState(
		a2,
		"site1",
		includedPools,
		hcProfiles,
		nil,
		assignmentPoolHealthCheckProfileNames,
		assignmentPoolBFDSourceAssignment,
		assignmentSiteHealthCheckProfileNames,
		assignmentSiteBFDSourceAssignment,
	)

	// With composite keying, each site gets its own entry for the same pool.
	if got := assignmentPoolHealthCheckProfileNames["site1|s1gw1"]; got != "sgpa-s1gw1-site1" {
		t.Fatalf("expected site1 assignment profile for pool s1gw1, got %q", got)
	}

	if got := assignmentPoolHealthCheckProfileNames["site2|s1gw1"]; got != "sgpa-s1gw1-site2" {
		t.Fatalf("expected site2 assignment profile for pool s1gw1, got %q", got)
	}

	if got := assignmentSiteHealthCheckProfileNames["site2"]; got != "sgpa-s1gw1-site2" {
		t.Fatalf("expected site2 to map to second assignment profile, got %q", got)
	}

	if _, ok := hcProfiles["sgpa-s1gw1-site1"]; !ok {
		t.Fatalf("expected sgpa-s1gw1-site1 profile to be present")
	}

	if _, ok := hcProfiles["sgpa-s1gw1-site2"]; !ok {
		t.Fatalf("expected sgpa-s1gw1-site2 profile to be present")
	}
}

// TestMergeAssignmentBFDStateSkipsPoolsOutsideFilter tests MergeAssignmentBFDStateSkipsPoolsOutsideFilter.
func TestMergeAssignmentBFDStateSkipsPoolsOutsideFilter(t *testing.T) {
	hcProfiles := make(map[string]healthcheck.HealthCheckSettings)
	assignmentPoolHealthCheckProfileNames := make(map[string]string)
	assignmentPoolBFDSourceAssignment := make(map[string]string)
	assignmentSiteHealthCheckProfileNames := make(map[string]string)
	assignmentSiteBFDSourceAssignment := make(map[string]string)

	assignment := unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "pool2-site2"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			GatewayPools: []string{"pool2"},
			Sites:        []string{"site2"},
		},
	}

	mergeAssignmentHealthCheckState(
		assignment,
		"site1",
		map[string]struct{}{"s1gw1": {}},
		hcProfiles,
		nil,
		assignmentPoolHealthCheckProfileNames,
		assignmentPoolBFDSourceAssignment,
		assignmentSiteHealthCheckProfileNames,
		assignmentSiteBFDSourceAssignment,
	)

	if _, ok := assignmentPoolHealthCheckProfileNames["pool2"]; ok {
		t.Fatalf("did not expect pool2 BFD profile mapping when pool filter excludes it")
	}

	if got := assignmentSiteHealthCheckProfileNames["site2"]; got != "sgpa-pool2-site2" {
		t.Fatalf("expected site-level mapping to remain available for included assignment, got %q", got)
	}
}

// TestPeerEqualityAndRouteIdentityHelpers tests PeerEqualityAndRouteIdentityHelpers.
func TestPeerEqualityAndRouteIdentityHelpers(t *testing.T) {
	meshA := []meshPeerInfo{{
		Name:               "n1",
		SiteName:           "site-a",
		WireGuardPublicKey: "pub1",
		InternalIPs:        []string{"10.0.0.1"},
		PodCIDRs:           []string{"10.244.1.0/24"},
	}}

	meshB := []meshPeerInfo{{
		Name:               "n1",
		SiteName:           "site-a",
		WireGuardPublicKey: "pub1",
		InternalIPs:        []string{"10.0.0.1"},
		PodCIDRs:           []string{"10.244.1.0/24"},
	}}
	if !meshPeersEqual(meshA, meshB) {
		t.Fatalf("expected mesh peers to be equal")
	}

	meshB[0].PodCIDRs = []string{"10.244.2.0/24"}
	if meshPeersEqual(meshA, meshB) {
		t.Fatalf("expected mesh peers to differ")
	}

	gwA := []gatewayPeerInfo{{
		Name:                 "gw1",
		SiteName:             "site-b",
		PoolName:             "pool1",
		PoolType:             "External",
		WireGuardPublicKey:   "gwpub",
		GatewayWireguardPort: 51821,
		InternalIPs:          []string{"10.0.1.1"},
		ExternalIPs:          []string{"52.1.1.1"},
		HealthEndpoints:      []string{"http://10.0.1.1:9998/healthz"},
		RoutedCidrs:          []string{"10.244.2.0/24"},
		RouteDistances:       map[string]int{"10.244.2.0/24": 2},
		PodCIDRs:             []string{"10.244.1.0/24"},
	}}

	gwB := []gatewayPeerInfo{{
		Name:                 "gw1",
		SiteName:             "site-b",
		PoolName:             "pool1",
		PoolType:             "External",
		WireGuardPublicKey:   "gwpub",
		GatewayWireguardPort: 51821,
		InternalIPs:          []string{"10.0.1.1"},
		ExternalIPs:          []string{"52.1.1.1"},
		HealthEndpoints:      []string{"http://10.0.1.1:9998/healthz"},
		RoutedCidrs:          []string{"10.244.2.0/24"},
		RouteDistances:       map[string]int{"10.244.2.0/24": 2},
		PodCIDRs:             []string{"10.244.1.0/24"},
	}}
	if !gatewayPeersEqual(gwA, gwB) {
		t.Fatalf("expected gateway peers to be equal")
	}

	gwB[0].RouteDistances["10.244.2.0/24"] = 3
	if gatewayPeersEqual(gwA, gwB) {
		t.Fatalf("expected gateway peers to differ")
	}
}

func ptrBool(v bool) *bool {
	return &v
}

func ptrInt32(v int32) *int32 {
	return &v
}

func ptrIntOrString(v intstr.IntOrString) *intstr.IntOrString {
	return &v
}
