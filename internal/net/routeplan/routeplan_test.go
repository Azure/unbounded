// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package routeplan

import (
	"slices"
	"testing"
	"time"
)

// TestExpectedDestinationsForSitePeer tests expected destinations for site peer.
func TestExpectedDestinationsForSitePeer(t *testing.T) {
	peer := Peer{
		Name:       "node-b",
		PeerType:   "site",
		Endpoint:   "10.0.0.2:51820",
		AllowedIPs: []string{"10.0.0.2/32", "10.244.99.0/24"},
	}
	nodes := map[string]Node{
		"node-b": {
			Name:        "node-b",
			SiteName:    "site-a",
			PodCIDRs:    []string{"10.244.2.0/24"},
			InternalIPs: []string{"10.0.0.2"},
			ExternalIPs: []string{"52.160.1.20"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if !slices.Contains(destinations, "10.244.2.0/24") {
		t.Fatalf("expected podCIDR destination, got %#v", destinations)
	}

	if !slices.Contains(destinations, "10.244.2.1/32") {
		t.Fatalf("expected podCIDR host destination, got %#v", destinations)
	}

	if slices.Contains(destinations, "10.0.0.2/32") {
		t.Fatalf("did not expect internal IP host route for site peer, got %#v", destinations)
	}

	if slices.Contains(destinations, "52.160.1.20/32") {
		t.Fatalf("did not expect external IP host route for site peer, got %#v", destinations)
	}

	if slices.Contains(destinations, "10.244.99.0/24") {
		t.Fatalf("did not expect broad allowedIP route when peer podCIDRs are known, got %#v", destinations)
	}
}

// TestExpectedDestinationsForSitePeerSkipPodCIDRRoutes tests site peer podCIDR suppression.
func TestExpectedDestinationsForSitePeerSkipPodCIDRRoutes(t *testing.T) {
	peer := Peer{
		Name:              "node-b",
		PeerType:          "site",
		SkipPodCIDRRoutes: true,
		Endpoint:          "10.0.0.2:51820",
		AllowedIPs:        []string{"10.0.0.2/32"},
	}
	nodes := map[string]Node{
		"node-b": {
			Name:        "node-b",
			SiteName:    "site-a",
			PodCIDRs:    []string{"10.244.2.0/24"},
			InternalIPs: []string{"10.0.0.2"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if slices.Contains(destinations, "10.244.2.0/24") {
		t.Fatalf("did not expect podCIDR destination when skipPodCIDRRoutes is true, got %#v", destinations)
	}

	if !slices.Contains(destinations, "10.244.2.1/32") {
		t.Fatalf("expected podCIDR host destination when skipPodCIDRRoutes is true, got %#v", destinations)
	}
}

// TestExpectedDestinationsForNonPeeredSitePeerIncludesInternalIPHostRoute tests expected destinations for non peered site peer includes internal iphost route.
func TestExpectedDestinationsForNonPeeredSitePeerIncludesInternalIPHostRoute(t *testing.T) {
	peer := Peer{
		Name:       "node-b",
		PeerType:   "site",
		SitePeered: false,
		Endpoint:   "52.160.1.20:51820",
		AllowedIPs: []string{"10.0.0.2/32", "52.160.1.20/32"},
	}
	nodes := map[string]Node{
		"node-b": {
			Name:        "node-b",
			SiteName:    "site-a",
			PodCIDRs:    []string{"10.244.2.0/24"},
			InternalIPs: []string{"10.0.0.2"},
			ExternalIPs: []string{"52.160.1.20"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if !slices.Contains(destinations, "10.0.0.2/32") {
		t.Fatalf("expected internal IP host route for non-peered site peer, got %#v", destinations)
	}

	if slices.Contains(destinations, "52.160.1.20/32") {
		t.Fatalf("did not expect external IP host route for site peer, got %#v", destinations)
	}
}

// TestExpectedDestinationsForSitePeeredSitePeerExcludesInternalIPHostRoute tests expected destinations for site peered site peer excludes internal iphost route.
func TestExpectedDestinationsForSitePeeredSitePeerExcludesInternalIPHostRoute(t *testing.T) {
	peer := Peer{
		Name:       "node-b",
		PeerType:   "site",
		SitePeered: true,
		Endpoint:   "52.160.1.20:51820",
		AllowedIPs: []string{"10.0.0.2/32"},
	}
	nodes := map[string]Node{
		"node-b": {
			Name:        "node-b",
			SiteName:    "site-b",
			PodCIDRs:    []string{"10.244.2.0/24"},
			InternalIPs: []string{"10.0.0.2"},
			ExternalIPs: []string{"52.160.1.20"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if slices.Contains(destinations, "10.0.0.2/32") {
		t.Fatalf("did not expect internal IP host route for site-peered site peer, got %#v", destinations)
	}
}

// TestExpectedDestinationsForGatewayPeer tests expected destinations for gateway peer.
func TestExpectedDestinationsForGatewayPeer(t *testing.T) {
	peer := Peer{
		Name:            "gw-a",
		PeerType:        "gateway",
		SiteName:        "site-b",
		PodCIDRGateways: []string{"10.2.0.5"},
		AllowedIPs:      []string{"10.244.0.0/16", "0.0.0.0/0"},
	}
	nodes := map[string]Node{
		"gw-a": {
			Name:        "gw-a",
			SiteName:    "site-b",
			PodCIDRs:    []string{"10.244.9.0/24"},
			InternalIPs: []string{"10.30.0.4"},
			ExternalIPs: []string{"52.170.10.20"},
		},
		"node-b": {
			Name:     "node-b",
			SiteName: "site-b",
			PodCIDRs: []string{"10.244.9.0/24"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if !slices.Contains(destinations, "10.244.0.0/16") {
		t.Fatalf("expected remote site routed CIDR destination, got %#v", destinations)
	}

	if !slices.Contains(destinations, "10.244.9.0/24") {
		t.Fatalf("expected gateway peer podCIDR destination to remain expected, got %#v", destinations)
	}

	if slices.Contains(destinations, "10.244.9.1/32") {
		t.Fatalf("did not expect host route derived from remote site podCIDR supernet, got %#v", destinations)
	}

	if !slices.Contains(destinations, "10.2.0.5/32") {
		t.Fatalf("expected gateway host destination, got %#v", destinations)
	}

	if slices.Contains(destinations, "0.0.0.0/0") {
		t.Fatalf("did not expect default route destination, got %#v", destinations)
	}

	if slices.Contains(destinations, "52.170.10.20/32") {
		t.Fatalf("did not expect gateway external IP host route, got %#v", destinations)
	}
}

// TestExpectedDestinationsForGatewayPeerSkipPodCIDRRoutes tests gateway peer podCIDR suppression.
func TestExpectedDestinationsForGatewayPeerSkipPodCIDRRoutes(t *testing.T) {
	peer := Peer{
		Name:              "gw-a",
		PeerType:          "gateway",
		SiteName:          "site-b",
		SkipPodCIDRRoutes: true,
		PodCIDRGateways:   []string{"10.2.0.5"},
		AllowedIPs:        []string{"10.244.0.0/16"},
	}
	nodes := map[string]Node{
		"gw-a": {
			Name:        "gw-a",
			SiteName:    "site-b",
			PodCIDRs:    []string{"10.244.9.0/24"},
			InternalIPs: []string{"10.30.0.4"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if !slices.Contains(destinations, "10.244.0.0/16") {
		t.Fatalf("expected routed CIDR destination to remain, got %#v", destinations)
	}

	if slices.Contains(destinations, "10.244.9.0/24") {
		t.Fatalf("did not expect gateway podCIDR destination when skipPodCIDRRoutes is true, got %#v", destinations)
	}

	if !slices.Contains(destinations, "10.244.9.1/32") {
		t.Fatalf("expected gateway podCIDR host destination when skipPodCIDRRoutes is true, got %#v", destinations)
	}
}

// TestExpectedDestinationsForExternalGatewayIncludesInternalIPHostRoute tests expected destinations for external gateway includes internal iphost route.
func TestExpectedDestinationsForExternalGatewayIncludesInternalIPHostRoute(t *testing.T) {
	peer := Peer{
		Name:            "gw-a",
		PeerType:        "gateway",
		SiteName:        "site-b",
		Endpoint:        "52.170.10.20:51820",
		PodCIDRGateways: []string{"10.2.0.5"},
		AllowedIPs:      []string{"10.30.0.4/32", "52.170.10.20/32"},
	}
	nodes := map[string]Node{
		"gw-a": {
			Name:        "gw-a",
			SiteName:    "site-b",
			PodCIDRs:    []string{"10.244.9.0/24"},
			InternalIPs: []string{"10.30.0.4"},
			ExternalIPs: []string{"52.170.10.20"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if !slices.Contains(destinations, "10.30.0.4/32") {
		t.Fatalf("expected internal IP host route for external gateway peer, got %#v", destinations)
	}

	if slices.Contains(destinations, "52.170.10.20/32") {
		t.Fatalf("did not expect external IP host route for external gateway peer, got %#v", destinations)
	}
}

// TestExpectedDestinationsForGatewayPeerIncludesFirstUsableHostForExplicitPodCIDR tests expected destinations for gateway peer includes first usable host for explicit pod cidr.
func TestExpectedDestinationsForGatewayPeerIncludesFirstUsableHostForExplicitPodCIDR(t *testing.T) {
	peer := Peer{
		Name:            "gw-v6",
		PeerType:        "gateway",
		SiteName:        "site-b",
		PodCIDRGateways: []string{"100.125.1.1"},
		AllowedIPs:      []string{"fdde:0:0:1:1::/80", "fddf::/48"},
	}
	nodes := map[string]Node{
		"gw-v6": {
			Name:        "gw-v6",
			SiteName:    "site-b",
			PodCIDRs:    []string{"fdde:0:0:1:1::/80"},
			InternalIPs: []string{"fddf::6"},
		},
	}

	destinations := ExpectedDestinationsForPeer(peer, nodes)
	if !slices.Contains(destinations, "fdde:0:0:1:1::/80") {
		t.Fatalf("expected explicit gateway podCIDR destination, got %#v", destinations)
	}

	if !slices.Contains(destinations, "fdde::1:1:0:0:1/128") {
		t.Fatalf("expected first-usable host route for explicit gateway podCIDR, got %#v", destinations)
	}

	if slices.Contains(destinations, "fddf::1/128") {
		t.Fatalf("did not expect first-usable host route for routed CIDR supernet, got %#v", destinations)
	}
}

// TestNormalizeAndLocalSets tests normalize and local sets.
func TestNormalizeAndLocalSets(t *testing.T) {
	if got := NormalizeCIDR("10.124.1.0/24"); got != "10.124.1.0/24" {
		t.Fatalf("unexpected normalized IPv4 CIDR: %q", got)
	}

	if got := NormalizeCIDR("bad"); got != "" {
		t.Fatalf("expected empty normalized CIDR for invalid input, got %q", got)
	}

	localSet := BuildNormalizedCIDRSet([]string{"10.124.1.0/24", "fdde::/48", "bad"})
	if _, ok := localSet["10.124.1.0/24"]; !ok {
		t.Fatalf("expected local set to include IPv4 CIDR")
	}

	if _, ok := localSet["fdde::/48"]; !ok {
		t.Fatalf("expected local set to include IPv6 CIDR")
	}

	hostSet := BuildLocalGatewayHostCIDRSetFromPodCIDRs([]string{"10.124.1.0/24", "fdde::/48", "bad"})
	if _, ok := hostSet["10.124.1.1/32"]; !ok {
		t.Fatalf("expected host set to include IPv4 host route")
	}

	if _, ok := hostSet["fdde::1/128"]; !ok {
		t.Fatalf("expected host set to include IPv6 host route")
	}
}

// TestClassifyRouteInfoForPeerDestination tests classify route info for peer destination.
func TestClassifyRouteInfoForPeerDestination(t *testing.T) {
	peers := map[string]Peer{
		"node-b": {
			Name:            "node-b",
			PeerType:        "site",
			PodCIDRGateways: []string{"100.124.3.1"},
		},
		"gw-a": {
			Name:            "gw-a",
			PeerType:        "gateway",
			SiteName:        "site-b",
			PodCIDRGateways: []string{"100.124.9.1"},
		},
	}
	nodes := map[string]Node{
		"node-b": {
			Name:        "node-b",
			SiteName:    "site-a",
			PodCIDRs:    []string{"10.244.2.0/24"},
			InternalIPs: []string{"10.0.0.2"},
		},
		"node-c": {
			Name:     "node-c",
			SiteName: "site-b",
			PodCIDRs: []string{"10.244.9.0/24"},
		},
	}
	siteCIDRs := BuildSitePodCIDRSetBySite(nodes)

	info := ClassifyRouteInfoForPeerDestination("10.244.2.0/24", []string{"node-b"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "node" || info.ObjectName != "node-b" || info.RouteType != "podCidr" {
		t.Fatalf("unexpected site peer route info: %#v", info)
	}

	info = ClassifyRouteInfoForPeerDestination("10.244.9.0/24", []string{"gw-a"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-b" || info.RouteType != "podCidr" {
		t.Fatalf("unexpected gateway site route info: %#v", info)
	}

	nodes["gw-a"] = Node{
		Name:     "gw-a",
		SiteName: "site-b",
		PodCIDRs: []string{"fdde:0:0:1:1::/80"},
	}
	siteCIDRs = BuildSitePodCIDRSetBySite(nodes)

	info = ClassifyRouteInfoForPeerDestination("fdde:0:0:1:1::/80", []string{"gw-a"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "gateway" || info.ObjectName != "gw-a" || info.RouteType != "podCidr" {
		t.Fatalf("expected gateway podCIDR classification to take priority over site podCIDR, got %#v", info)
	}

	info = ClassifyRouteInfoForPeerDestination("100.124.9.1/32", []string{"gw-a"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "gateway" || info.ObjectName != "gw-a" || info.RouteType != "nodeCidr" {
		t.Fatalf("unexpected gateway host route info: %#v", info)
	}

	nodes["gw-a"] = Node{
		Name:     "gw-a",
		SiteName: "site-b",
		PodCIDRs: []string{"100.65.0.0/16"},
	}
	nodes["gw-b"] = Node{
		Name:     "gw-b",
		SiteName: "site-b",
		PodCIDRs: []string{"100.65.0.0/16"},
	}
	siteCIDRs = BuildSitePodCIDRSetBySite(nodes)

	info = ClassifyRouteInfoForPeerDestination("100.65.0.0/16", []string{"gw-a"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-b" || info.RouteType != "podCidr" {
		t.Fatalf("expected shared site-wide podCIDR to classify as site podCidr, got %#v", info)
	}

	peers["gw-a"] = Peer{
		Name:       "gw-a",
		PeerType:   "gateway",
		SiteName:   "site-b",
		AllowedIPs: []string{"100.65.0.0/16"},
	}
	peers["gw-b"] = Peer{
		Name:       "gw-b",
		PeerType:   "gateway",
		SiteName:   "site-b",
		AllowedIPs: []string{"100.65.0.0/16"},
	}

	info = ClassifyRouteInfoForPeerDestination("100.65.0.0/16", []string{"gw-a"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-b" || info.RouteType != "podCidr" {
		t.Fatalf("expected shared gateway site CIDR to classify as site podCidr, got %#v", info)
	}

	nodes["gw-a"] = Node{
		Name:     "gw-a",
		SiteName: "site-b",
		PodCIDRs: []string{"100.125.4.0/24"},
	}
	nodes["gw-b"] = Node{
		Name:     "gw-b",
		SiteName: "site-b",
		PodCIDRs: []string{"100.125.6.0/24"},
	}
	siteCIDRs = BuildSitePodCIDRSetBySite(nodes)

	info = ClassifyRouteInfoForPeerDestination("100.65.0.0/16", []string{"gw-a"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-b" || info.RouteType != "nodeCidr" {
		t.Fatalf("expected shared gateway site node supernet to classify as site nodeCidr, got %#v", info)
	}

	info = ClassifyRouteInfoForPeerDestination("100.65.0.0/16", []string{"gw-a", "gw-b"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-b" || info.RouteType != "nodeCidr" {
		t.Fatalf("expected shared gateway site node supernet with ECMP peers to classify as site-b nodeCidr, got %#v", info)
	}

	peers["gw-a"] = Peer{
		Name:            "gw-a",
		PeerType:        "gateway",
		SiteName:        "site-b",
		PodCIDRGateways: []string{"100.124.3.1"},
		AllowedIPs:      []string{"100.124.0.0/16"},
	}
	peers["gw-b"] = Peer{
		Name:            "gw-b",
		PeerType:        "gateway",
		SiteName:        "site-b",
		PodCIDRGateways: []string{"100.124.4.1"},
		AllowedIPs:      []string{"100.124.0.0/16"},
	}
	nodes = map[string]Node{}
	siteCIDRs = BuildSitePodCIDRSetBySite(nodes)

	info = ClassifyRouteInfoForPeerDestination("100.124.0.0/16", []string{"gw-a", "gw-b"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-b" || info.RouteType != "podCidr" {
		t.Fatalf("expected shared gateway pod supernet with ECMP peers to classify as site-b podCidr, got %#v", info)
	}

	info = ClassifyRouteInfoForPeerDestination("10.0.0.2/32", []string{"node-b"}, peers, nodes, siteCIDRs, nil, nil)
	if info == nil || info.RouteType != "podCidr" {
		t.Fatalf("did not expect site peer internal IP host route classification as nodeCidr: %#v", info)
	}

	siteNodeCIDRs := BuildSiteNodeCIDRSetBySite(map[string][]string{
		"site-a": {"100.64.0.0/16"},
		"site-b": {"100.65.0.0/16"},
	})

	info = ClassifyRouteInfoForPeerDestination("100.64.0.0/16", []string{"gw-a", "gw-b"}, peers, nodes, siteCIDRs, siteNodeCIDRs, nil)
	if info == nil || info.ObjectType != "site" || info.ObjectName != "site-a" || info.RouteType != "nodeCidr" {
		t.Fatalf("expected unique site nodeCIDR ownership to classify as site-a nodeCidr, got %#v", info)
	}
}

// TestFilterGatewayAdvertisedRoutes tests filter gateway advertised routes.
func TestFilterGatewayAdvertisedRoutes(t *testing.T) {
	now := time.Now().UTC()
	advertisement := GatewayRouteAdvertisement{
		Name:        "gw-a",
		LastUpdated: now,
		Routes: map[string]LearnedRoute{
			"10.244.3.0/24": {
				Paths: [][]PathHop{{
					{Type: "Site", Name: "site-c"},
					{Type: "GatewayPool", Name: "pool-remote"},
				}},
			},
			"10.244.1.0/24": {
				Paths: [][]PathHop{{
					{Type: "Site", Name: "site-a"},
				}},
			},
			"10.244.5.0/24": {
				Paths: [][]PathHop{{
					{Type: "GatewayPool", Name: "pool-local"},
				}},
			},
		},
	}

	routes, distances, selected := FilterGatewayAdvertisedRoutes(advertisement, []string{"100.64.0.0/16"}, "site-a", []string{"pool-local"}, now, 30*time.Second)
	if len(routes) != 1 || routes[0] != "10.244.3.0/24" {
		t.Fatalf("unexpected filtered routes: %#v", routes)
	}

	if distances["10.244.3.0/24"] != 1 {
		t.Fatalf("unexpected route distance map: %#v", distances)
	}

	if _, ok := selected["10.244.3.0/24"]; !ok {
		t.Fatalf("expected selected route to include surviving path")
	}

	staleRoutes, _, _ := FilterGatewayAdvertisedRoutes(GatewayRouteAdvertisement{
		Name:        "gw-stale",
		LastUpdated: now.Add(-10 * time.Minute),
		Routes:      advertisement.Routes,
	}, []string{"100.64.0.0/16"}, "site-a", []string{"pool-local"}, now, 30*time.Second)
	if len(staleRoutes) != 1 || staleRoutes[0] != "100.64.0.0/16" {
		t.Fatalf("expected stale advertisement to use fallback routes, got %#v", staleRoutes)
	}
}

// TestFilterGatewayAdvertisedRoutes_RemoteSitePathWithLocalPoolIsRetained tests filter gateway advertised routes remote site path with local pool is retained.
func TestFilterGatewayAdvertisedRoutes_RemoteSitePathWithLocalPoolIsRetained(t *testing.T) {
	now := time.Now().UTC()
	advertisement := GatewayRouteAdvertisement{
		Name:        "gw-a",
		LastUpdated: now,
		Routes: map[string]LearnedRoute{
			"100.65.0.0/16": {
				Paths: [][]PathHop{{
					{Type: "Site", Name: "site2"},
					{Type: "GatewayPool", Name: "pool-local"},
				}},
			},
		},
	}

	routes, _, selected := FilterGatewayAdvertisedRoutes(advertisement, nil, "site1", []string{"pool-local"}, now, 30*time.Second)
	if len(routes) != 1 || routes[0] != "100.65.0.0/16" {
		t.Fatalf("expected remote-site route to be retained even with local pool hop, got %#v", routes)
	}

	if _, ok := selected["100.65.0.0/16"]; !ok {
		t.Fatalf("expected selected routes to include retained remote-site route")
	}
}

// TestBuildExpectedWireGuardRoutes_UsesFamilyAwareGatewayForIPv6Network tests build expected wire guard routes uses family aware gateway for ipv6 network.
func TestBuildExpectedWireGuardRoutes_UsesFamilyAwareGatewayForIPv6Network(t *testing.T) {
	peers := []Peer{
		{
			Name:            "node-v6",
			PeerType:        "site",
			Interface:       "wg51820",
			PodCIDRGateways: []string{"100.124.1.1"},
		},
	}
	nodes := map[string]Node{
		"node-v6": {
			Name:     "node-v6",
			SiteName: "site-a",
			PodCIDRs: []string{"fdde::1:0:0:0/80"},
		},
	}

	_, ipv6Routes := BuildExpectedWireGuardRoutes(peers, nodes)

	var (
		networkRoute *ExpectedRoute
		hostRoute    *ExpectedRoute
	)

	for i := range ipv6Routes {
		r := ipv6Routes[i]
		switch r.Destination {
		case "fdde::1:0:0:0/80":
			networkRoute = &r
		case "fdde::1:0:0:1/128":
			hostRoute = &r
		}
	}

	if networkRoute == nil {
		t.Fatalf("expected IPv6 network route to be present, got %#v", ipv6Routes)
	}

	if networkRoute.Gateway != "fdde::1:0:0:1" {
		t.Fatalf("expected IPv6 network gateway to be first usable in destination CIDR, got %q", networkRoute.Gateway)
	}

	if networkRoute.Family != 6 {
		t.Fatalf("expected IPv6 family for network route, got %d", networkRoute.Family)
	}

	if hostRoute == nil {
		t.Fatalf("expected IPv6 host route to be present, got %#v", ipv6Routes)
	}

	if hostRoute.Gateway != "" {
		t.Fatalf("expected IPv6 host route gateway to be empty, got %q", hostRoute.Gateway)
	}
}

// TestBuildExpectedWireGuardRoutes_GatewayPeerUsesPeerScopedIPv6FallbackGateway tests build expected wire guard routes gateway peer uses peer scoped ipv6 fallback gateway.
func TestBuildExpectedWireGuardRoutes_GatewayPeerUsesPeerScopedIPv6FallbackGateway(t *testing.T) {
	peers := []Peer{
		{
			Name:            "ext-gw",
			PeerType:        "gateway",
			Interface:       "wg51824",
			PodCIDRGateways: []string{"100.125.1.1"},
			AllowedIPs:      []string{"fdde:0:0:1:1::/80", "fddf::/48"},
		},
	}

	_, ipv6Routes := BuildExpectedWireGuardRoutes(peers, map[string]Node{})

	var siteRoute *ExpectedRoute

	for i := range ipv6Routes {
		r := ipv6Routes[i]
		if r.Destination == "fddf::/48" {
			siteRoute = &r
			break
		}
	}

	if siteRoute == nil {
		t.Fatalf("expected site IPv6 route to be present, got %#v", ipv6Routes)
	}

	if siteRoute.Gateway != "fdde::1:1:0:0:1" {
		t.Fatalf("expected gateway peer fallback gateway from most specific allowed IPv6 CIDR, got %q", siteRoute.Gateway)
	}
}
