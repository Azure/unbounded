// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"github.com/Azure/unbounded-kube/internal/net/routeplan"
	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// TestSortExpectedRoutesAndDestinationType tests SortExpectedRoutesAndDestinationType.
func TestSortExpectedRoutesAndDestinationType(t *testing.T) {
	routes := []expectedRoute{
		{Destination: "10.0.0.0/24", Gateway: "10.0.0.2", Device: "wg1", Weight: 20},
		{Destination: "10.0.0.0/24", Gateway: "10.0.0.1", Device: "wg1", Weight: 10},
		{Destination: "10.0.1.0/24", Gateway: "10.0.0.1", Device: "wg0", Weight: 10},
	}
	sortExpectedRoutes(routes)

	if routes[0].Destination != "10.0.0.0/24" || routes[0].Gateway != "10.0.0.1" {
		t.Fatalf("unexpected route ordering after sort: %#v", routes)
	}

	if got := destinationType("10.0.0.1/32"); got != "host" {
		t.Fatalf("expected /32 route to be host type, got %q", got)
	}

	if got := destinationType("10.0.0.0/24"); got != "network" {
		t.Fatalf("expected /24 route to be network type, got %q", got)
	}
}

// TestEndpointHostAndWireGuardInterfaceName tests EndpointHostAndWireGuardInterfaceName.
func TestEndpointHostAndWireGuardInterfaceName(t *testing.T) {
	cases := []struct {
		endpoint string
		expect   string
	}{
		{endpoint: "10.0.0.1:51820", expect: "10.0.0.1"},
		{endpoint: "[fd00::1]:51820", expect: "fd00::1"},
		{endpoint: "fd00::1", expect: "fd00::1"},
		{endpoint: "", expect: ""},
	}
	for _, tc := range cases {
		if got := endpointHost(tc.endpoint); got != tc.expect {
			t.Fatalf("endpointHost(%q) expected %q, got %q", tc.endpoint, tc.expect, got)
		}
	}

	if !isWireGuardInterfaceName("wg0") || !isWireGuardInterfaceName("WG1") {
		t.Fatalf("expected wg-prefixed interface names to match")
	}

	if isWireGuardInterfaceName("eth0") {
		t.Fatalf("expected non-wg interface name to not match")
	}
}

// TestSourceForPeerDestinationAndHealthCheckDecisions tests SourceForPeerDestinationAndBFDDecisions.
func TestSourceForPeerDestinationAndHealthCheckDecisions(t *testing.T) {
	peerByName := map[string]routeplan.Peer{
		"peer-a": {
			Name:     "peer-a",
			SiteName: "site-a",
		},
	}
	nodesByName := map[string]routeplan.Node{
		"peer-a": {
			Name:     "peer-a",
			SiteName: "site-a",
			PodCIDRs: []string{"10.244.1.0/24"},
		},
	}
	sitePodCIDRs := routeplan.BuildSitePodCIDRSetBySite(nodesByName)

	src := sourceForPeerDestination(peerByName["peer-a"], "10.244.1.0/24", peerByName, nodesByName, sitePodCIDRs, nil)
	if src == "derived" {
		t.Fatalf("expected peer destination source to be classified, got %q", src)
	}

	if got := sourceForPeerDestination(routeplan.Peer{}, "10.244.1.0/24", peerByName, nodesByName, sitePodCIDRs, nil); got != "derived" {
		t.Fatalf("expected empty peer to fall back to derived source, got %q", got)
	}

	hcDecisions := explainHealthCheckDecisions(true, []string{"10.0.0.1/32", "10.0.0.0/24"})
	if len(hcDecisions) != 2 {
		t.Fatalf("expected 2 health check decisions, got %#v", hcDecisions)
	}

	for _, decision := range hcDecisions {
		if decision.Destination == "10.0.0.1/32" && decision.Expected {
			t.Fatalf("expected host-route health check decision to be false")
		}

		if decision.Destination == "10.0.0.0/24" && !decision.Expected {
			t.Fatalf("expected network-route health check decision to be true when health check enabled")
		}
	}
}

// TestBuildUnexpectedRouteOutputAndExplainPeerDecisions tests BuildUnexpectedRouteOutputAndExplainPeerDecisions.
func TestBuildUnexpectedRouteOutputAndExplainPeerDecisions(t *testing.T) {
	target := statusv1alpha1.NodeStatusResponse{
		RoutingTable: statusv1alpha1.RoutingTableInfo{
			Routes: []statusv1alpha1.RouteEntry{
				{
					Destination: "10.0.0.0/24",
					Family:      "IPv4",
					NextHops:    []statusv1alpha1.NextHop{{Gateway: "10.0.0.1", Device: "wg0", Weight: 10}},
				},
				{
					Destination: "10.0.0.0/24",
					Family:      "IPv4",
					NextHops:    []statusv1alpha1.NextHop{{Gateway: "10.0.0.1", Device: "wg0", Weight: 20}},
				},
				{
					Destination: "10.0.1.0/24",
					Family:      "IPv4",
					NextHops: []statusv1alpha1.NextHop{{
						Gateway: "10.0.0.2",
						Device:  "wg0",
						Weight:  1,
						Info: &statusv1alpha1.NextHopInfo{
							ObjectType: "Site",
							ObjectName: "site-a",
							RouteType:  "podCidr",
						},
					}},
				},
				{
					Destination: "10.0.2.1/32",
					Family:      "IPv4",
					NextHops: []statusv1alpha1.NextHop{{
						Device:     "wg0",
						RouteTypes: []statusv1alpha1.RouteType{{Type: "connected"}},
					}},
				},
			},
		},
	}

	expectedIPv4 := []expectedRoute{{Destination: "10.0.0.0/24", Gateway: "10.0.0.1", Device: "wg0", Weight: 10, Family: 4}}

	unexpectedIPv4, unexpectedIPv6 := buildUnexpectedRouteOutput(target, expectedIPv4, nil)
	if len(unexpectedIPv6) != 0 {
		t.Fatalf("expected no unexpected IPv6 routes, got %#v", unexpectedIPv6)
	}

	if len(unexpectedIPv4) != 1 {
		t.Fatalf("expected exactly one unexpected IPv4 route, got %#v", unexpectedIPv4)
	}

	if unexpectedIPv4[0].Destination != "10.0.1.0/24" {
		t.Fatalf("unexpected unexpected-route destination: %#v", unexpectedIPv4)
	}

	if unexpectedIPv4[0].Source != "Site:site-a/podCidr" {
		t.Fatalf("unexpected source labeling: %#v", unexpectedIPv4[0])
	}

	peer := routeplan.Peer{
		Name:            "peer-a",
		PeerType:        "gateway",
		Endpoint:        "52.0.0.10:51820",
		AllowedIPs:      []string{"0.0.0.0/0", "10.244.1.0/24"},
		PodCIDRGateways: []string{"10.244.1.1"},
	}
	nodesByName := map[string]routeplan.Node{
		"peer-a": {
			Name:        "peer-a",
			PodCIDRs:    []string{"10.244.1.0/24"},
			InternalIPs: []string{"10.0.0.10"},
			ExternalIPs: []string{"52.0.0.10"},
		},
	}

	decisions := explainPeerDecisions(peer, nodesByName)
	if len(decisions) == 0 {
		t.Fatalf("expected explainPeerDecisions to produce decisions")
	}

	foundDefaultRouteExclusion := false

	for _, decision := range decisions {
		if decision.Destination == "0.0.0.0/0" && decision.Reason == "default route excluded" {
			foundDefaultRouteExclusion = true
		}
	}

	if !foundDefaultRouteExclusion {
		t.Fatalf("expected default route exclusion decision, got %#v", decisions)
	}
}

// TestBuildExpectedRouteOutputAndExpectedDestinationsForPeer tests BuildExpectedRouteOutputAndExpectedDestinationsForPeer.
func TestBuildExpectedRouteOutputAndExpectedDestinationsForPeer(t *testing.T) {
	peers := []routeplan.Peer{
		{
			Name:            "peer-a",
			PeerType:        "gateway",
			SiteName:        "site-a",
			Interface:       "wg0",
			Endpoint:        "52.0.0.10:51820",
			AllowedIPs:      []string{"10.244.1.0/24"},
			PodCIDRGateways: []string{"10.244.1.1"},
		},
	}
	nodesByName := map[string]routeplan.Node{
		"peer-a": {
			Name:        "peer-a",
			SiteName:    "site-a",
			PodCIDRs:    []string{"10.244.1.0/24"},
			InternalIPs: []string{"10.0.0.10"},
			ExternalIPs: []string{"52.0.0.10"},
		},
	}

	entries := expectedDestinationsForPeer(peers[0], nodesByName, peers)
	if len(entries) == 0 {
		t.Fatalf("expected expectedDestinationsForPeer to return entries")
	}

	ipv4Routes, _ := routeplan.BuildExpectedWireGuardRoutes(peers, nodesByName)

	output := buildExpectedRouteOutput(ipv4Routes, peers, nodesByName)
	if len(output) == 0 {
		t.Fatalf("expected buildExpectedRouteOutput to return routes")
	}

	for _, route := range output {
		if route.Source == "derived" {
			t.Fatalf("expected route source classification, got derived route %#v", route)
		}
	}
}
