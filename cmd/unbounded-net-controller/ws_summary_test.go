// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"testing"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestBuildClusterSummary(t *testing.T) {
	status := &ClusterStatusResponse{
		Seq:           5,
		Timestamp:     time.Now(),
		NodeCount:     2,
		SiteCount:     1,
		AzureTenantID: "tenant-1",
		LeaderInfo:    &LeaderInfo{PodName: "ctrl-0", NodeName: "node-a"},
		BuildInfo:     &BuildInfo{Version: "v1.0.0"},
		Nodes: []*NodeStatusResponse{
			{
				NodeInfo: statusv1alpha1.NodeInfo{
					Name:      "node-a",
					SiteName:  "site-1",
					IsGateway: true,
					K8sReady:  "True",
				},
				StatusSource: "push",
				Peers: []statusv1alpha1.PeerStatus{
					{
						Name: "peer-1",
						HealthCheck: &statusv1alpha1.HealthCheckPeerStatus{
							Enabled: true,
							Status:  "up",
						},
					},
					{
						Name: "peer-2",
						HealthCheck: &statusv1alpha1.HealthCheckPeerStatus{
							Enabled: true,
							Status:  "down",
						},
					},
				},
				RoutingTable: statusv1alpha1.RoutingTableInfo{
					Routes: []statusv1alpha1.RouteEntry{
						{
							Destination: "10.0.0.0/8",
							NextHops: []statusv1alpha1.NextHop{
								{Expected: boolPtr(true), Present: boolPtr(true)},
							},
						},
					},
				},
				HealthCheck: &statusv1alpha1.HealthCheckStatus{Healthy: true},
			},
			{
				NodeInfo: statusv1alpha1.NodeInfo{
					Name:     "node-b",
					SiteName: "site-1",
					K8sReady: "True",
				},
				StatusSource: "push",
				FetchError:   "connection refused",
				Peers:        []statusv1alpha1.PeerStatus{},
				RoutingTable: statusv1alpha1.RoutingTableInfo{
					Routes: []statusv1alpha1.RouteEntry{
						{
							Destination: "10.1.0.0/16",
							NextHops: []statusv1alpha1.NextHop{
								{Expected: boolPtr(true), Present: boolPtr(false)},
							},
						},
					},
				},
			},
		},
		Sites: []SiteStatus{{Name: "site-1", NodeCount: 2}},
		Problems: []StatusProblem{
			{Name: "node-b", Type: "node", Errors: []string{"fetch error"}},
		},
		PullEnabled: true,
	}

	summary := buildClusterSummary(status)

	if summary.Seq != 5 {
		t.Fatalf("expected seq=5, got %d", summary.Seq)
	}

	if summary.NodeCount != 2 {
		t.Fatalf("expected nodeCount=2, got %d", summary.NodeCount)
	}

	if summary.SiteCount != 1 {
		t.Fatalf("expected siteCount=1, got %d", summary.SiteCount)
	}

	if summary.AzureTenantID != "tenant-1" {
		t.Fatalf("expected tenant-1, got %q", summary.AzureTenantID)
	}

	if summary.LeaderInfo == nil || summary.LeaderInfo.PodName != "ctrl-0" {
		t.Fatalf("expected leader info, got %v", summary.LeaderInfo)
	}

	if summary.BuildInfo == nil || summary.BuildInfo.Version != "v1.0.0" {
		t.Fatalf("expected build info, got %v", summary.BuildInfo)
	}

	if !summary.PullEnabled {
		t.Fatalf("expected pullEnabled=true")
	}

	if len(summary.Sites) != 1 {
		t.Fatalf("expected 1 site, got %d", len(summary.Sites))
	}

	if len(summary.Problems) != 1 {
		t.Fatalf("expected 1 problem, got %d", len(summary.Problems))
	}

	if len(summary.NodeSummaries) != 2 {
		t.Fatalf("expected 2 node summaries, got %d", len(summary.NodeSummaries))
	}

	// Verify node-a summary
	nsA := summary.NodeSummaries[0]
	if nsA.Name != "node-a" {
		t.Fatalf("expected node-a, got %q", nsA.Name)
	}

	if nsA.SiteName != "site-1" {
		t.Fatalf("expected site-1, got %q", nsA.SiteName)
	}

	if !nsA.IsGateway {
		t.Fatalf("expected isGateway=true for node-a")
	}

	if nsA.PeerCount != 2 {
		t.Fatalf("expected 2 peers, got %d", nsA.PeerCount)
	}

	if nsA.HealthyPeers != 1 {
		t.Fatalf("expected 1 healthy peer, got %d", nsA.HealthyPeers)
	}

	if nsA.RouteCount != 1 {
		t.Fatalf("expected 1 route, got %d", nsA.RouteCount)
	}

	if nsA.RouteMismatch {
		t.Fatalf("expected no route mismatch for node-a")
	}

	if nsA.CniStatus != "Healthy" {
		t.Fatalf("expected Healthy CNI status, got %q", nsA.CniStatus)
	}

	if nsA.CniTone != "success" {
		t.Fatalf("expected success tone, got %q", nsA.CniTone)
	}

	// Verify node-b summary
	nsB := summary.NodeSummaries[1]
	if nsB.Name != "node-b" {
		t.Fatalf("expected node-b, got %q", nsB.Name)
	}

	if nsB.FetchError != "connection refused" {
		t.Fatalf("expected fetch error, got %q", nsB.FetchError)
	}

	if nsB.CniStatus != "Fetch error" {
		t.Fatalf("expected 'Fetch error' status, got %q", nsB.CniStatus)
	}

	if nsB.CniTone != "danger" {
		t.Fatalf("expected danger tone, got %q", nsB.CniTone)
	}

	if !nsB.RouteMismatch {
		t.Fatalf("expected route mismatch for node-b")
	}
}

func boolPtr(v bool) *bool { return &v }

func TestBuildClusterSummaryEmpty(t *testing.T) {
	status := &ClusterStatusResponse{
		Nodes:    []*NodeStatusResponse{},
		Sites:    []SiteStatus{},
		Problems: []StatusProblem{},
	}

	summary := buildClusterSummary(status)
	if len(summary.NodeSummaries) != 0 {
		t.Fatalf("expected 0 node summaries, got %d", len(summary.NodeSummaries))
	}
}

func TestDeriveCniStatusAndTone(t *testing.T) {
	tests := []struct {
		name          string
		node          NodeStatusResponse
		routeMismatch bool
		wantStatus    string
		wantTone      string
	}{
		{
			name:       "fetch error",
			node:       NodeStatusResponse{FetchError: "timeout"},
			wantStatus: "Fetch error",
			wantTone:   "danger",
		},
		{
			name: "node errors",
			node: NodeStatusResponse{
				StatusSource: "push",
				NodeErrors:   []statusv1alpha1.NodeError{{Type: "x", Message: "bad"}},
			},
			wantStatus: "Errors",
			wantTone:   "danger",
		},
		{
			name: "unhealthy health check peers",
			node: NodeStatusResponse{
				StatusSource: "push",
				HealthCheck:  &statusv1alpha1.HealthCheckStatus{Healthy: false},
			},
			wantStatus: "Healthy",
			wantTone:   "success",
		},
		{
			name:          "route mismatch",
			node:          NodeStatusResponse{StatusSource: "push"},
			routeMismatch: true,
			wantStatus:    "Route mismatch",
			wantTone:      "warning",
		},
		{
			name:       "stale source",
			node:       NodeStatusResponse{StatusSource: "stale"},
			wantStatus: "Stale",
			wantTone:   "warning",
		},
		{
			name:       "error source",
			node:       NodeStatusResponse{StatusSource: "error"},
			wantStatus: "Stale",
			wantTone:   "warning",
		},
		{
			name:       "empty source",
			node:       NodeStatusResponse{},
			wantStatus: "Unknown",
			wantTone:   "warning",
		},
		{
			name:       "healthy",
			node:       NodeStatusResponse{StatusSource: "push"},
			wantStatus: "Healthy",
			wantTone:   "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, tone := deriveCniStatusAndTone(&tt.node, tt.routeMismatch)
			if status != tt.wantStatus {
				t.Errorf("status = %q, want %q", status, tt.wantStatus)
			}

			if tone != tt.wantTone {
				t.Errorf("tone = %q, want %q", tone, tt.wantTone)
			}
		})
	}
}

func TestClusterSummaryJSON(t *testing.T) {
	summary := &ClusterSummary{
		Seq:       1,
		NodeCount: 1,
		SiteCount: 1,
		NodeSummaries: []NodeSummary{
			{
				Name:      "node-a",
				CniStatus: "Healthy",
				CniTone:   "success",
				PeerCount: 3,
			},
		},
		Problems: []StatusProblem{},
	}

	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded ClusterSummary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.Seq != 1 {
		t.Fatalf("expected seq=1, got %d", decoded.Seq)
	}

	if len(decoded.NodeSummaries) != 1 || decoded.NodeSummaries[0].Name != "node-a" {
		t.Fatalf("expected 1 node summary, got %v", decoded.NodeSummaries)
	}
}

func TestWSClientMessageNodeName(t *testing.T) {
	raw := `{"type":"node_detail_request","nodeName":"aks-node-1"}`

	var msg WSClientMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if msg.Type != "node_detail_request" {
		t.Fatalf("expected type=node_detail_request, got %q", msg.Type)
	}

	if msg.NodeName != "aks-node-1" {
		t.Fatalf("expected nodeName=aks-node-1, got %q", msg.NodeName)
	}
}

func TestWSMessageTypes(t *testing.T) {
	// Verify the new message types round-trip through JSON
	types := []string{
		"cluster_summary",
		"node_detail_response",
		"node_detail_update",
	}
	for _, msgType := range types {
		msg := WSMessage{Type: msgType, Data: map[string]string{"test": "value"}}

		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("failed to marshal %s: %v", msgType, err)
		}

		var decoded WSMessage
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("failed to unmarshal %s: %v", msgType, err)
		}

		if decoded.Type != msgType {
			t.Fatalf("expected type=%s, got %q", msgType, decoded.Type)
		}
	}
}

func TestSendNodeDetailUpdates(t *testing.T) {
	b := NewWSBroadcaster(nil)

	status := &ClusterStatusResponse{
		Nodes: []*NodeStatusResponse{
			{NodeInfo: statusv1alpha1.NodeInfo{Name: "node-a"}},
			{NodeInfo: statusv1alpha1.NodeInfo{Name: "node-b"}},
		},
	}

	// Client with subscriptions
	c1 := &WSClient{
		send:                    make(chan []byte, 4),
		nodeDetailSubscriptions: map[string]bool{"node-a": true},
	}
	// Client without subscriptions
	c2 := &WSClient{
		send:                    make(chan []byte, 4),
		nodeDetailSubscriptions: make(map[string]bool),
	}
	clients := []*WSClient{c1, c2}

	// First broadcast (changedNodes == nil) -- should send all subscribed nodes
	b.sendNodeDetailUpdates(status, nil, clients, nil)

	if len(c1.send) != 1 {
		t.Fatalf("expected 1 message for c1, got %d", len(c1.send))
	}

	if len(c2.send) != 0 {
		t.Fatalf("expected 0 messages for c2, got %d", len(c2.send))
	}

	// Drain
	<-c1.send

	// Delta broadcast with only node-b changed
	changedNodes := map[string]bool{"node-b": true}
	b.sendNodeDetailUpdates(status, changedNodes, clients, nil)

	if len(c1.send) != 0 {
		t.Fatalf("expected 0 messages for c1 (node-a not changed), got %d", len(c1.send))
	}

	// Subscribe c1 to node-b and retry
	c1.nodeDetailSubscriptions["node-b"] = true

	b.sendNodeDetailUpdates(status, changedNodes, clients, nil)

	if len(c1.send) != 1 {
		t.Fatalf("expected 1 message for c1 (node-b changed and subscribed), got %d", len(c1.send))
	}

	// Verify message type
	rawMsg := <-c1.send

	var msg WSMessage
	if err := json.Unmarshal(rawMsg, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if msg.Type != "node_detail_update" {
		t.Fatalf("expected type=node_detail_update, got %q", msg.Type)
	}
}
