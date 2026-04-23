// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestNodeStatusToProto(t *testing.T) {
	now := time.Now()
	pushTime := now.Add(-time.Minute)
	updatedAt := now.Add(-2 * time.Minute)
	expected := true
	present := false
	status := &NodeStatusResponse{
		Timestamp: now,
		NodeInfo: NodeInfo{
			Name:        "node-1",
			SiteName:    "site-a",
			IsGateway:   true,
			PodCIDRs:    []string{"10.0.0.0/24"},
			InternalIPs: []string{"192.168.1.1"},
			ExternalIPs: []string{"1.2.3.4"},
			BuildInfo: &BuildInfo{
				Version:   "v1.0.0",
				Commit:    "abc123",
				BuildTime: "2024-01-01",
			},
			WireGuard: &WireGuardStatusInfo{
				Interface:  "wg0",
				PublicKey:  "pubkey123",
				ListenPort: 51820,
				PeerCount:  3,
			},
			K8sReady:     "True",
			ProviderID:   "azure://sub/rg/vm",
			OSImage:      "Ubuntu 22.04",
			Kernel:       "5.15.0",
			Kubelet:      "v1.28.0",
			Arch:         "amd64",
			NodeOS:       "linux",
			K8sLabels:    map[string]string{"role": "worker"},
			K8sUpdatedAt: &updatedAt,
		},
		Peers: []statusv1alpha1.PeerStatus{
			{
				Name:              "peer-1",
				PeerType:          "mesh",
				SiteName:          "site-a",
				PodCIDRGateways:   []string{"10.0.1.0/24"},
				SkipPodCIDRRoutes: true,
				RouteDistances:    map[string]int{"10.0.0.0/16": 100},
				Tunnel: PeerTunnelStatus{
					Protocol:      "wireguard",
					Interface:     "wg0",
					PublicKey:     "peerpubkey",
					Endpoint:      "1.2.3.4:51820",
					AllowedIPs:    []string{"10.0.1.0/24"},
					RxBytes:       1024,
					TxBytes:       2048,
					LastHandshake: now.Add(-30 * time.Second),
				},
				RouteDestinations: []string{"10.0.1.0/24"},
				HealthCheck: &HealthCheckPeerStatus{
					Enabled: true,
					Status:  "up",
					Uptime:  "1h30m",
					RTT:     "2.5ms",
				},
			},
		},
		RoutingTable: RoutingTableInfo{
			Routes: []RouteEntry{
				{
					Destination: "10.0.0.0/16",
					Family:      "IPv4",
					Table:       254,
					NextHops: []NextHop{
						{
							Gateway:          "10.0.0.1",
							Device:           "wg0",
							Distance:         100,
							Weight:           1,
							MTU:              1420,
							Expected:         &expected,
							Present:          &present,
							PeerDestinations: []string{"10.0.1.0/24"},
							RouteTypes: []RouteType{
								{Type: "BGP", Attributes: []string{"local-pref=100"}},
							},
							Info: &NextHopInfo{
								ObjectName: "site-a",
								ObjectType: "Site",
								RouteType:  "mesh",
							},
						},
					},
				},
			},
			ManagedRouteCount: 5,
			PendingRouteCount: 1,
		},
		HealthCheck: &HealthCheckStatus{
			Healthy:   true,
			Summary:   "all peers healthy",
			PeerCount: 3,
			CheckedAt: now,
		},
		NodeErrors: []NodeError{
			{Type: "config", Message: "something wrong"},
		},
		FetchError:   "",
		LastPushTime: &pushTime,
		StatusSource: "local",
		NodePodInfo: &statusv1alpha1.NodePodInfo{
			PodName:   "unbounded-net-node-abc",
			StartTime: now.Add(-time.Hour),
			Restarts:  2,
		},
	}

	pb := nodeStatusToProto(status)
	if pb == nil {
		t.Fatal("expected non-nil protobuf message")
	}

	// Verify round-trip: proto.Marshal then proto.Unmarshal
	data, err := proto.Marshal(&statusproto.NodeStatusMessage{
		Type:     "node_status_full",
		NodeName: "node-1",
		Status:   pb,
	})
	if err != nil {
		t.Fatalf("proto.Marshal failed: %v", err)
	}

	var roundTrip statusproto.NodeStatusMessage
	if err := proto.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("proto.Unmarshal failed: %v", err)
	}

	if roundTrip.Type != "node_status_full" {
		t.Errorf("type = %q, want %q", roundTrip.Type, "node_status_full")
	}

	if roundTrip.NodeName != "node-1" {
		t.Errorf("nodeName = %q, want %q", roundTrip.NodeName, "node-1")
	}

	full := roundTrip.Status
	if full == nil {
		t.Fatal("expected non-nil status in round-tripped message")
	}

	if full.TimestampUnixNs != now.UnixNano() {
		t.Errorf("timestamp = %d, want %d", full.TimestampUnixNs, now.UnixNano())
	}

	if full.NodeInfo.Name != "node-1" {
		t.Errorf("nodeInfo.name = %q, want %q", full.NodeInfo.Name, "node-1")
	}

	if !full.NodeInfo.IsGateway {
		t.Error("expected isGateway = true")
	}

	if full.NodeInfo.WireGuard.ListenPort != 51820 {
		t.Errorf("wireGuard.listenPort = %d, want %d", full.NodeInfo.WireGuard.ListenPort, 51820)
	}

	if len(full.Peers) != 1 {
		t.Fatalf("peers count = %d, want 1", len(full.Peers))
	}

	if full.Peers[0].Tunnel.RxBytes != 1024 {
		t.Errorf("peer tunnel rxBytes = %d, want 1024", full.Peers[0].Tunnel.RxBytes)
	}

	if full.Peers[0].RouteDistances["10.0.0.0/16"] != 100 {
		t.Errorf("peer routeDistances = %v", full.Peers[0].RouteDistances)
	}

	if full.RoutingTable.ManagedRouteCount != 5 {
		t.Errorf("routingTable.managedRouteCount = %d, want 5", full.RoutingTable.ManagedRouteCount)
	}

	if len(full.RoutingTable.Routes) != 1 {
		t.Fatalf("routes count = %d, want 1", len(full.RoutingTable.Routes))
	}

	nh := full.RoutingTable.Routes[0].NextHops[0]
	if nh.Expected == nil || !nh.Expected.Value {
		t.Errorf("nextHop.expected = %v, want true", nh.Expected)
	}

	if nh.Present == nil || nh.Present.Value {
		t.Errorf("nextHop.present = %v, want false", nh.Present)
	}

	if nh.Info.ObjectName != "site-a" {
		t.Errorf("nextHop.info.objectName = %q, want %q", nh.Info.ObjectName, "site-a")
	}

	if !full.HealthCheck.Healthy {
		t.Error("expected healthCheck.healthy = true")
	}

	if full.LastPushTimeUnixNs != pushTime.UnixNano() {
		t.Errorf("lastPushTimeUnixNs = %d, want %d", full.LastPushTimeUnixNs, pushTime.UnixNano())
	}

	if full.NodePodInfo.PodName != "unbounded-net-node-abc" {
		t.Errorf("nodePodInfo.podName = %q, want %q", full.NodePodInfo.PodName, "unbounded-net-node-abc")
	}
}

func TestNodeStatusToProtoNil(t *testing.T) {
	if pb := nodeStatusToProto(nil); pb != nil {
		t.Errorf("expected nil for nil input, got %v", pb)
	}
}

func TestNodeStatusDeltaToProto(t *testing.T) {
	delta := map[string]json.RawMessage{
		"nodeInfo": json.RawMessage(`{"name":"node-1","siteName":"site-a","isGateway":false,"podCIDRs":["10.0.0.0/24"]}`),
		"peers":    json.RawMessage(`[{"name":"peer-1","peerType":"mesh","tunnel":{"interface":"wg0"}}]`),
	}

	pb := nodeStatusDeltaToProto(delta)
	if pb == nil {
		t.Fatal("expected non-nil protobuf delta")
	}

	if len(pb.UpdatedFields) != 2 {
		t.Errorf("updatedFields count = %d, want 2", len(pb.UpdatedFields))
	}

	if pb.NodeInfo == nil {
		t.Fatal("expected non-nil nodeInfo in delta")
	}

	if pb.NodeInfo.Name != "node-1" {
		t.Errorf("nodeInfo.name = %q, want %q", pb.NodeInfo.Name, "node-1")
	}

	if len(pb.Peers) != 1 {
		t.Fatalf("peers count = %d, want 1", len(pb.Peers))
	}

	if pb.Peers[0].Name != "peer-1" {
		t.Errorf("peers[0].name = %q, want %q", pb.Peers[0].Name, "peer-1")
	}

	// Verify round-trip through proto
	data, err := proto.Marshal(&statusproto.NodeStatusMessage{
		Type:         "node_status_delta",
		NodeName:     "node-1",
		BaseRevision: 42,
		Delta:        pb,
	})
	if err != nil {
		t.Fatalf("proto.Marshal failed: %v", err)
	}

	var roundTrip statusproto.NodeStatusMessage
	if err := proto.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("proto.Unmarshal failed: %v", err)
	}

	if roundTrip.Type != "node_status_delta" {
		t.Errorf("type = %q, want %q", roundTrip.Type, "node_status_delta")
	}

	if roundTrip.BaseRevision != 42 {
		t.Errorf("baseRevision = %d, want 42", roundTrip.BaseRevision)
	}
}

func TestNodeStatusDeltaToProtoEmpty(t *testing.T) {
	if pb := nodeStatusDeltaToProto(nil); pb != nil {
		t.Errorf("expected nil for nil delta, got %v", pb)
	}

	if pb := nodeStatusDeltaToProto(map[string]json.RawMessage{}); pb != nil {
		t.Errorf("expected nil for empty delta, got %v", pb)
	}
}
