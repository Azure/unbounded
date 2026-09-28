// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/net/routeplan"
)

func TestRouteClassificationSnapshot(t *testing.T) {
	peers := []WireGuardPeerStatus{
		{Name: " peer-a ", PeerType: "site", SiteName: "site-a", Tunnel: PeerTunnelStatus{Endpoint: "old"}},
		{Name: "peer-a", PeerType: "site", SiteName: "site-a", PodCIDRGateways: []string{"10.244.1.1", "fd00:1::1"}},
		{Name: "peer-b", PeerType: "site", SiteName: "site-a"},
		{Name: " peer-c ", PeerType: "site", SiteName: "site-b"},
		{Name: "gw", PeerType: "gateway", SiteName: "site-a", Tunnel: PeerTunnelStatus{
			Endpoint: "203.0.113.1:51820", AllowedIPs: []string{"100.64.0.0/16"},
		}},
		{Name: " \t", PeerType: "site"},
	}
	ctx := &annotationContext{
		routeNodes: map[string]routeplan.Node{
			"peer-a": {Name: "peer-a", SiteName: "site-a", PodCIDRs: []string{"10.244.1.0/24", "fd00:1::/64"}},
			"peer-b": {Name: "peer-b", SiteName: "site-a", PodCIDRs: []string{"10.244.2.0/24"}},
			"gw":     {Name: "gw", SiteName: "site-a", InternalIPs: []string{"172.20.0.1"}, ExternalIPs: []string{"203.0.113.1"}},
		},
		sitePodCIDRs: map[string]map[string]struct{}{
			"site-a": {"10.244.1.0/24": {}, "10.244.2.0/24": {}},
		},
		gatewayPoolRoutedCIDRs: map[string]map[string]struct{}{
			"pool-a": {"100.64.0.0/16": {}},
		},
	}

	index := buildRouteClassificationPeers(peers)
	if len(index) != 4 || index["peer-a"].Endpoint != "" || index["peer-c"].Name != " peer-c " {
		t.Fatalf("blank-name filtering or last-duplicate precedence changed: %+v", index)
	}

	before, err := json.Marshal(struct {
		Peers []WireGuardPeerStatus
		Index map[string]routeplan.Peer
	}{peers, index})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		destination string
		peers       []string
		want        *NextHopInfo
	}{
		{"node-v4", "10.244.1.1/32", []string{"peer-a"}, &NextHopInfo{ObjectName: "peer-a", ObjectType: "node", RouteType: "podCidr"}},
		{"node-v6", "fd00:1::1/128", []string{"peer-a"}, &NextHopInfo{ObjectName: "peer-a", ObjectType: "node", RouteType: "podCidr"}},
		{"site-supernet", "10.244.0.0/16", []string{"peer-a", "peer-b"}, &NextHopInfo{ObjectName: "site-a", ObjectType: "site", RouteType: "podCidr"}},
		{"pool", "100.64.0.0/16", []string{"gw"}, &NextHopInfo{ObjectName: "pool-a", ObjectType: "gatewayPool", RouteType: "routedCidr"}},
		{"gateway-host", "172.20.0.1/32", []string{"gw"}, &NextHopInfo{ObjectName: "gw", ObjectType: "gateway", RouteType: "nodeCidr"}},
		{"unknown-peer", "10.1.0.0/24", []string{"missing"}, nil},
		{"empty-destination", "", []string{"peer-a"}, nil},
		{"empty-peers", "10.244.1.0/24", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeInfoForNextHop(tc.destination, tc.peers, index, ctx); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("classification: got %+v, want %+v", got, tc.want)
			}
		})
	}

	after, err := json.Marshal(struct {
		Peers []WireGuardPeerStatus
		Index map[string]routeplan.Peer
	}{peers, index})
	if err != nil {
		t.Fatal(err)
	}

	if string(before) != string(after) {
		t.Fatal("classification mutated snapshot data")
	}

	peers[1].SiteName = "site-new"

	next := buildRouteClassificationPeers(peers)
	if next["peer-a"].SiteName != "site-new" || index["peer-a"].SiteName != "site-a" {
		t.Fatal("peer metadata leaked between snapshot indexes")
	}

	if len(buildRouteClassificationPeers(nil)) != 0 {
		t.Fatal("empty snapshot created peer entries")
	}
}

func routeClassificationFixture(count int) ([]WireGuardPeerStatus, []string, *annotationContext) {
	peers := make([]WireGuardPeerStatus, count)
	names := make([]string, count)
	ctx := &annotationContext{routeNodes: make(map[string]routeplan.Node, count)}

	for i := range peers {
		name := fmt.Sprintf("peer-%d", i)
		cidr := fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
		names[i] = name
		peers[i] = WireGuardPeerStatus{
			Name: name, PeerType: "site", SiteName: "site-a",
			Tunnel: PeerTunnelStatus{Interface: "wg51820", AllowedIPs: []string{cidr}},
		}
		ctx.routeNodes[name] = routeplan.Node{Name: name, PodCIDRs: []string{cidr}}
	}

	return peers, names, ctx
}

// legacyRouteClassification retains the original per-hop projection as a benchmark baseline.
func legacyRouteClassification(destination string, names []string, peers map[string]WireGuardPeerStatus, ctx *annotationContext) *NextHopInfo {
	index := make(map[string]routeplan.Peer, len(peers))
	for name, peer := range peers {
		index[name] = routeplan.Peer{
			Name: peer.Name, PeerType: peer.PeerType, SiteName: peer.SiteName,
			SkipPodCIDRRoutes: peer.SkipPodCIDRRoutes, Endpoint: peer.Tunnel.Endpoint,
			PodCIDRGateways: peer.PodCIDRGateways, AllowedIPs: peer.Tunnel.AllowedIPs,
		}
	}

	return routeInfoForNextHop(destination, names, index, ctx)
}

func TestRouteClassificationAllocationBound(t *testing.T) {
	peers, names, ctx := routeClassificationFixture(2000)
	index := buildRouteClassificationPeers(peers)

	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			info := routeInfoForNextHop("10.0.0.0/24", names[:1], index, ctx)
			if info == nil || info.ObjectName != names[0] {
				b.Fatal("unexpected classification")
			}
		}
	})
	if result.AllocedBytesPerOp() > 4096 {
		t.Fatalf("classification allocated %d B/op with 2000 indexed peers; limit 4096", result.AllocedBytesPerOp())
	}
}

func TestRouteAnnotationFamiliesShareReadOnlyIndex(t *testing.T) {
	cfg := &config{WireGuardInterfacePrefix: "wg"}
	peers := []WireGuardPeerStatus{{
		Name: "node-b", PeerType: "site", SiteName: "site-a",
		Tunnel: PeerTunnelStatus{Interface: "wg51820", AllowedIPs: []string{"10.1.0.0/24", "fd00:1::/64"}},
	}}
	ctx := &annotationContext{routeNodes: map[string]routeplan.Node{
		"node-b": {Name: "node-b", SiteName: "site-a", PodCIDRs: []string{"10.1.0.0/24", "fd00:1::/64"}},
	}}
	index := buildRouteClassificationPeers(peers)

	for _, destination := range []string{"10.1.0.0/24", "fd00:1::/64"} {
		routes := []RouteEntry{{Destination: destination, NextHops: []NextHop{{
			Device: "wg51820", PeerDestinations: []string{" node-b ", "node-b", ""},
		}}}}
		got := annotateRoutePeerDestinationsForFamily(cfg, routes, peers, index, ctx, "site-a")

		hop := got[0].NextHops[0]
		if !reflect.DeepEqual(hop.PeerDestinations, []string{"node-b"}) ||
			!reflect.DeepEqual(hop.Info, &NextHopInfo{ObjectName: "node-b", ObjectType: "node", RouteType: "podCidr"}) {
			t.Fatalf("%s: unexpected annotation %+v", destination, hop)
		}

		for _, tc := range []struct{ destination, device string }{
			{"invalid", "wg51820"},
			{"10.1.0.0/24", "eth0"},
			{"10.2.0.0/24", "wg51820"},
		} {
			routes := []RouteEntry{{Destination: tc.destination, NextHops: []NextHop{{
				Device: tc.device, Info: &NextHopInfo{ObjectName: "stale"},
			}}}}

			got := annotateRoutePeerDestinationsForFamily(cfg, routes, peers, index, ctx, "site-a")
			if hop := got[0].NextHops[0]; hop.Info != nil || len(hop.PeerDestinations) != 0 {
				t.Fatalf("unmatched route retained classification: %+v", hop)
			}
		}
	}
}

func BenchmarkRouteClassificationSnapshot(b *testing.B) {
	for _, count := range []int{10, 100, 2000} {
		peers, names, ctx := routeClassificationFixture(count)

		legacy := make(map[string]WireGuardPeerStatus, count)
		for _, peer := range peers {
			legacy[strings.TrimSpace(peer.Name)] = peer
		}

		for _, rebuild := range []bool{true, false} {
			b.Run(fmt.Sprintf("peers-%d/rebuild-per-hop-%t", count, rebuild), func(b *testing.B) {
				b.ReportAllocs()

				for b.Loop() {
					var index map[string]routeplan.Peer
					if !rebuild {
						index = buildRouteClassificationPeers(peers)
					}

					for i, name := range names {
						destination := ctx.routeNodes[name].PodCIDRs[0]

						var info *NextHopInfo
						if rebuild {
							info = legacyRouteClassification(destination, names[i:i+1], legacy, ctx)
						} else {
							info = routeInfoForNextHop(destination, names[i:i+1], index, ctx)
						}

						if info == nil || info.ObjectName != name {
							b.Fatal("unexpected classification")
						}
					}
				}
			})
		}
	}
}
