// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"github.com/Azure/unbounded/internal/dashboard/contract"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func sampleStatus() *ClusterStatusResponse {
	return &ClusterStatusResponse{
		SiteCount: 2,
		Sites: []SiteStatus{
			{Name: "east", NodeCount: 2, OnlineCount: 2, ManageCniPlugin: true, NodeCidrs: []string{"10.0.0.0/16"}},
			{Name: "west", NodeCount: 2, OnlineCount: 1, OfflineCount: 1},
		},
		GatewayPools: []GatewayPoolStatus{
			{Name: "pool-a", SiteName: "east", Gateways: []string{"n1"}, ConnectedSites: []string{"west"}},
		},
		Peerings: []PeeringStatus{
			{Name: "east-west", Sites: []string{"east", "west"}, HealthCheckEnabled: true},
		},
		Nodes: []*NodeStatusResponse{
			{
				NodeInfo: statusv1alpha1.NodeInfo{Name: "n1", SiteName: "east", IsGateway: true, K8sReady: "True"},
				Peers: []statusv1alpha1.PeerStatus{
					{Name: "n2", PeerType: "node", Tunnel: statusv1alpha1.PeerTunnelStatus{Protocol: "wireguard", Endpoint: "1.2.3.4:51820"}, HealthCheck: &statusv1alpha1.HealthCheckPeerStatus{Status: "up"}},
				},
				RoutingTable: statusv1alpha1.RoutingTableInfo{Routes: []statusv1alpha1.RouteEntry{{Destination: "10.1.0.0/24"}}, ManagedRouteCount: 1},
			},
			{
				NodeInfo:   statusv1alpha1.NodeInfo{Name: "n3", SiteName: "west", K8sReady: "True"},
				NodeErrors: []statusv1alpha1.NodeError{{Type: "wireguard", Message: "peer down"}},
			},
		},
		ConnectivityMatrix: map[string]*SiteMatrix{
			"east": {Results: map[string]map[string]string{"east": {"west": "ok"}}},
			"west": {Results: map[string]map[string]string{"west": {"east": "down"}}},
		},
	}
}

func TestNetSummaryHealthAndMetrics(t *testing.T) {
	sum := netSummary(sampleStatus())

	// A node-level error degrades node health -> overall warning (cluster-level
	// Problems/Errors would be required for an overall error).
	if sum.Health != contract.HealthWarning {
		t.Errorf("summary health = %q, want warning", sum.Health)
	}

	want := map[string]string{"Sites": "2", "Gateway Pools": "1", "Nodes": "1/2", "Peerings": "1/1"}
	for _, m := range sum.Metrics {
		if exp, ok := want[m.Label]; ok && m.Value != exp {
			t.Errorf("metric %q = %q, want %q", m.Label, m.Value, exp)
		}
	}

	if len(sum.Alerts) == 0 {
		t.Error("expected alerts for node error")
	}
}

func TestNetOverviewPanels(t *testing.T) {
	ov := netOverview(sampleStatus())

	types := map[contract.PanelType]bool{}
	for _, p := range ov.Panels {
		types[p.Type] = true
	}

	for _, want := range []contract.PanelType{contract.PanelMetrics, contract.PanelGraph, contract.PanelMatrix, contract.PanelTable} {
		if !types[want] {
			t.Errorf("overview missing panel type %q", want)
		}
	}
}

func TestNetGraphStructure(t *testing.T) {
	g := netGraph(sampleStatus())

	if len(g.Nodes) == 0 {
		t.Fatal("graph has no nodes")
	}

	var haveSite, havePool bool

	for _, n := range g.Nodes {
		switch n.Kind {
		case "site":
			haveSite = true
		case "gatewaypool":
			havePool = true
		}
	}

	if !haveSite || !havePool {
		t.Errorf("graph missing node kinds: site=%v pool=%v", haveSite, havePool)
	}

	// The peering should connect east and west sites.
	var peeringEdge bool

	for _, e := range g.Edges {
		if e.Kind == "peering" {
			peeringEdge = true
		}
	}

	if !peeringEdge {
		t.Error("graph missing peering edge")
	}
}

func TestNetMatrixMapping(t *testing.T) {
	m := netMatrix(sampleStatus())

	if len(m.Rows) != 2 {
		t.Fatalf("matrix rows = %d, want 2", len(m.Rows))
	}

	if got := m.Cells["east"]["west"].Health; got != contract.HealthOK {
		t.Errorf("east->west health = %q, want ok", got)
	}

	if got := m.Cells["east"]["east"].Value; got != "-" {
		t.Errorf("diagonal east->east = %q, want -", got)
	}
}

func TestNetResourceListKinds(t *testing.T) {
	status := sampleStatus()

	for _, kind := range []string{"sites", "nodes", "gatewaypools", "peerings"} {
		list, ok := netResourceList(status, kind)
		if !ok {
			t.Errorf("kind %q not found", kind)
			continue
		}

		if len(list.Rows) == 0 {
			t.Errorf("kind %q has no rows", kind)
		}
	}

	if _, ok := netResourceList(status, "bogus"); ok {
		t.Error("bogus kind should not be found")
	}
}

func TestNetNodeDetail(t *testing.T) {
	detail, ok := netResourceDetail(sampleStatus(), "nodes", "n1")
	if !ok {
		t.Fatal("node n1 detail not found")
	}

	var haveRouting, havePeers bool

	for _, s := range detail.Sections {
		switch s.Title {
		case "Routing":
			haveRouting = true
		case "WireGuard Peers":
			havePeers = true
		}
	}

	if !haveRouting || !havePeers {
		t.Errorf("node detail missing sections: routing=%v peers=%v", haveRouting, havePeers)
	}

	if _, ok := netResourceDetail(sampleStatus(), "nodes", "missing"); ok {
		t.Error("missing node should not be found")
	}
}

func TestNetSiteDetail(t *testing.T) {
	detail, ok := netResourceDetail(sampleStatus(), "sites", "west")
	if !ok {
		t.Fatal("site west detail not found")
	}

	if detail.Health != contract.HealthWarning {
		t.Errorf("west site health = %q, want warning (has offline node)", detail.Health)
	}
}
