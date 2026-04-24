// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"

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
		DetectMultiplier: 5,
		ReceiveInterval:  150 * time.Millisecond,
		TransmitInterval: 275 * time.Millisecond,
	}
	if !healthCheckProfilesEqual(profile, want) {
		t.Fatalf("unexpected profile %#v, want %#v", profile, want)
	}

	siteMap := map[string]*unboundednetv1alpha1.Site{
		"site-a": {
			Spec: unboundednetv1alpha1.SiteSpec{
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
}

// TestResolveMeshPeerTunnelMTU tests resolveMeshPeerTunnelMTU.
func TestResolveMeshPeerTunnelMTU(t *testing.T) {
	// Same-site peer: only site MTU applies
	peer := meshPeerInfo{SiteName: "site-a"}

	got := resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{"site-a": 1300},
		map[string]int{"site-b": 1200},
		map[string]int{"site-b": 1100},
	)
	if got != 1300 {
		t.Fatalf("expected 1300 (site MTU), got %d", got)
	}

	// Cross-site peer: min of site, peering, assignment
	peer = meshPeerInfo{SiteName: "site-b"}

	got = resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{"site-a": 1350},
		map[string]int{"site-b": 1200},
		map[string]int{"site-b": 1250},
	)
	if got != 1200 {
		t.Fatalf("expected 1200 (peering MTU wins), got %d", got)
	}

	// No CRD overrides -- global wins
	peer = meshPeerInfo{SiteName: "site-b"}

	got = resolveMeshPeerTunnelMTU(1420, peer, "site-a",
		map[string]int{},
		map[string]int{},
		map[string]int{},
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
