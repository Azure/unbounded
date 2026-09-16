// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/net/routeplan"
)

func summaryRoute(destination string, index, table, distance int) netlink.Route {
	_, prefix, _ := net.ParseCIDR(destination)

	return netlink.Route{Dst: prefix, LinkIndex: index, Table: table, Priority: distance, Protocol: unix.RTPROT_BOOT}
}

func summaryRouteFixture() *nodeStatusServer {
	return &nodeStatusServer{
		cfg: &config{
			NodeName: "local", WireGuardInterfacePrefix: "wg", WireGuardPort: 51820,
			GeneveInterfaceName: "gn0", VXLANInterfaceName: "vx0", IPIPInterfaceName: "ip0",
		},
		state: &wireGuardState{routeTableID: 100},
		netlinkOps: &fakeNetlinkOps{
			links: map[int]netlink.Link{
				1: &fakeLink{attrs: netlink.LinkAttrs{Index: 1, Name: "wg51820"}},
				2: &fakeLink{attrs: netlink.LinkAttrs{Index: 2, Name: unbounded0DeviceName}},
				3: &fakeLink{attrs: netlink.LinkAttrs{Index: 3, Name: "eth0"}},
				4: &fakeLink{attrs: netlink.LinkAttrs{Index: 4, Name: "gn0"}},
			},
		},
	}
}

func TestRouteSummaryParity(t *testing.T) {
	peer := WireGuardPeerStatus{
		Name: "peer", PeerType: "site", SiteName: "local",
		PodCIDRGateways: []string{"10.42.1.1", "fd00:1::1"},
		Tunnel:          PeerTunnelStatus{Interface: "wg51820", AllowedIPs: []string{"10.42.1.0/24", "fd00:1::/64"}},
	}
	planPeer := routeplan.Peer{
		Name: peer.Name, PeerType: peer.PeerType, SiteName: peer.SiteName,
		Interface: peer.Tunnel.Interface, AllowedIPs: peer.Tunnel.AllowedIPs, PodCIDRGateways: peer.PodCIDRGateways,
	}

	for _, tc := range []struct {
		name        string
		v4          []netlink.Route
		v6          []netlink.Route
		table       []netlink.Route
		withoutPeer bool
	}{
		{name: "empty kernel does not synthesize"},
		{name: "missing expected", v4: []netlink.Route{summaryRoute("10.9.0.0/24", 1, 0, 0)}},
		{name: "matched", v4: []netlink.Route{summaryRoute("10.42.1.0/24", 1, 0, 0)}},
		{name: "unexpected without peers", withoutPeer: true, v4: []netlink.Route{summaryRoute("10.9.0.0/24", 1, 0, 0)}},
		{name: "unbounded suppresses only its family", v4: []netlink.Route{summaryRoute("10.42.0.0/16", 2, 0, 0)}},
		{name: "unbounded both families", v4: []netlink.Route{summaryRoute("10.42.0.0/16", 2, 0, 0)}, v6: []netlink.Route{summaryRoute("fd00::/48", 2, 0, 0)}},
		{name: "duplicate prefix distinct tables", v4: []netlink.Route{summaryRoute("10.42.1.0/24", 1, 0, 0)}, table: []netlink.Route{summaryRoute("10.42.1.0/24", 1, 100, 0)}},
		{name: "duplicate next hop", v4: []netlink.Route{summaryRoute("10.42.1.0/24", 1, 0, 0), summaryRoute("10.42.1.0/24", 1, 0, 100)}},
		{name: "wrong distance", v4: []netlink.Route{summaryRoute("10.42.1.0/24", 1, 0, 500)}},
		{name: "unmanaged ignored", v4: []netlink.Route{summaryRoute("10.42.1.0/24", 3, 0, 0)}},
		{name: "multipath", v4: []netlink.Route{{
			Dst:       summaryRoute("10.42.1.0/24", 1, 0, 0).Dst,
			MultiPath: []*netlink.NexthopInfo{{LinkIndex: 1}, {LinkIndex: 3}, {LinkIndex: 4}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := summaryRouteFixture()
			ops := s.netlinkOps.(*fakeNetlinkOps)
			ops.mainRoutes = map[int][]netlink.Route{netlink.FAMILY_V4: tc.v4, netlink.FAMILY_V6: tc.v6}
			ops.tableRoutes = map[int]map[int][]netlink.Route{netlink.FAMILY_V4: {100: tc.table}}
			full := &NodeStatusResponse{NodeInfo: NodeInfo{SiteName: "local"}, RoutingTable: s.collectRoutingTableFromKernel()}

			var peers []routeplan.Peer

			if !tc.withoutPeer {
				full.Peers = []WireGuardPeerStatus{peer}
				peers = []routeplan.Peer{planPeer}
			}

			annotateNodeRoutes(full, s.cfg, nil, nil, nil, nil)

			wantMismatch := false

			for _, route := range full.RoutingTable.Routes {
				for _, hop := range route.NextHops {
					if (hop.Expected != nil && *hop.Expected) != (hop.Present != nil && *hop.Present) {
						wantMismatch = true
					}
				}
			}

			count, mismatch := s.collectRouteSummary(peers, "local")
			if count != len(full.RoutingTable.Routes) || mismatch != wantMismatch {
				t.Fatalf("summary=(%d,%v), legacy=(%d,%v): %+v", count, mismatch, len(full.RoutingTable.Routes), wantMismatch, full.RoutingTable.Routes)
			}
		})
	}
}
