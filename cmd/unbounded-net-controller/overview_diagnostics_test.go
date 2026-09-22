// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestOverviewDiagnosticMessagesMatchFullProblems(t *testing.T) {
	now := time.Now()
	yes := true
	node := &NodeStatusResponse{
		NodeInfo: NodeInfo{Name: "node", K8sReady: "Ready", ProviderID: "azure://vm", WireGuard: &WireGuardStatusInfo{Interface: "wg0"}},
		Peers: []WireGuardPeerStatus{
			{Name: "same", Tunnel: PeerTunnelStatus{Protocol: "IPIP"}, HealthCheck: &HealthCheckPeerStatus{Status: " UP "}},
			{Name: "same", Tunnel: PeerTunnelStatus{LastHandshake: now}, HealthCheck: &HealthCheckPeerStatus{Status: " down "}},
			{Name: "same", HealthCheck: &HealthCheckPeerStatus{Enabled: true, Status: "DOWN"}},
		},
		RoutingTable: RoutingTableInfo{Routes: []RouteEntry{{
			NextHops: []NextHop{{Expected: &yes}, {Present: &yes}, {Expected: &yes}},
		}}},
	}

	fullProblems := collectClusterProblems(&ClusterStatusResponse{Nodes: []*NodeStatusResponse{node}})
	if len(fullProblems) != 1 || len(fullProblems[0].Errors) != 3 {
		t.Fatalf("legacy diagnostic fixture changed: %+v", fullProblems)
	}

	overview := statuspkg.OverviewFromStatus(node, now)
	overview.NodeInfo.ProviderID = ""
	messages := statuspkg.OverviewDiagnosticMessages(overview, node.NodeInfo.ProviderID)
	slices.Sort(messages)

	want := slices.Clone(fullProblems[0].Errors)
	slices.Sort(want)

	if !slices.Equal(messages, want) || overview.RouteMismatchCount != 3 || overview.UnhealthyPeerLinks != 2 {
		t.Fatalf("summary diagnostics=%v, legacy=%v, overview=%+v", messages, want, overview)
	}

	if otherCloud := statuspkg.OverviewDiagnosticMessages(overview, "other://vm"); len(otherCloud) != 2 {
		t.Fatalf("IPIP warning applied outside Azure: %v", otherCloud)
	}

	metadata := statuspkg.OverviewMetadata(overview)
	metadata.NodeInfo.ProviderID = node.NodeInfo.ProviderID
	cluster := &ClusterStatusResponse{
		Nodes:         []*NodeStatusResponse{&metadata},
		NodeOverviews: map[string]*statusv1alpha1.NodeStatusOverview{"node": &overview},
	}

	problems := collectClusterProblems(cluster)
	if len(problems) != 1 || !slices.Equal(problems[0].Errors, fullProblems[0].Errors) {
		t.Fatalf("overview problem pipeline=%+v, legacy=%+v", problems, fullProblems)
	}

	metadata.NodeInfo.ProviderID = "other://vm"

	if problems = collectClusterProblems(cluster); len(problems) != 1 || len(problems[0].Errors) != 2 {
		t.Fatalf("controller ignored enriched cloud identity: %+v", problems)
	}

	overview.RouteMismatch = false
	overview.RouteMismatchCount = 0
	overview.UnhealthyPeerLinks = 0

	if problems = collectClusterProblems(cluster); len(problems) != 0 {
		t.Fatalf("overview peer counts incorrectly replaced diagnostic link health: %+v", problems)
	}

	overview.RouteMismatchCount = 3
	overview.UnhealthyPeerLinks = 2

	overview.UsesIPIP = false
	if noIPIP := statuspkg.OverviewDiagnosticMessages(overview, node.NodeInfo.ProviderID); len(noIPIP) != 2 {
		t.Fatalf("Azure warning applied without IPIP: %v", noIPIP)
	}
}

func TestProtoOverviewPreservesDiagnosticFacts(t *testing.T) {
	got := protoToNodeOverview(&statusproto.NodeStatusOverview{
		NodeInfo:  &statusproto.NodeInfo{Name: "node", ProviderId: "azure://vm"},
		PeerCount: 4, HealthyPeers: 1, RouteCount: 2, RouteMismatch: true,
		RouteMismatchCount: 3, UnhealthyPeerLinks: 2, UsesIpip: true,
	})
	if got.RouteMismatchCount != 3 || got.UnhealthyPeerLinks != 2 || !got.UsesIPIP ||
		got.PeerCount != 4 || got.HealthyPeers != 1 || !got.RouteMismatch || got.NodeInfo.ProviderID != "azure://vm" {
		t.Fatalf("overview converter lost diagnostics: %+v", got)
	}
}

func TestViewerSummaryPreservesInterfaceOnlineIndependentOfCNI(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			status *WireGuardStatusInfo
			online bool
		}{
			{"missing", nil, false},
			{"public key without interface", &WireGuardStatusInfo{PublicKey: "key"}, false},
			{"interface with CNI failure", &WireGuardStatusInfo{Interface: "wg0"}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				node := &NodeStatusResponse{
					NodeInfo:   NodeInfo{Name: "node", WireGuard: tc.status},
					NodeErrors: []NodeError{{Type: "cni", Message: "blocked"}},
				}

				cluster := &ClusterStatusResponse{Nodes: []*NodeStatusResponse{node}}
				if native {
					cluster.NodeOverviews = map[string]*statusv1alpha1.NodeStatusOverview{
						"node": {NodeInfo: node.NodeInfo, NodeErrors: node.NodeErrors},
					}
				}

				summary := buildClusterSummary(cluster).NodeSummaries[0]
				if summary.WireGuardOnline != tc.online || summary.ErrorCount != 1 ||
					summary.FirstError != "blocked" || summary.CniStatus != "Errors" {
					t.Fatalf("interface/CNI facts conflated: %+v", summary)
				}

				data, err := json.Marshal(summary)
				if err != nil {
					t.Fatal(err)
				}

				var decoded struct {
					Online *bool `json:"wireGuardOnline"`
				}
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}

				if decoded.Online == nil || *decoded.Online != tc.online {
					t.Fatalf("explicit online/offline fact omitted or changed: %s", data)
				}
			})
		}
	}
}
