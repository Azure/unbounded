// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded-kube/internal/net/status/proto"
)

func TestProtoToNodeStatus(t *testing.T) {
	ts := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	full := &statusproto.NodeStatusFull{
		TimestampUnixNs: ts.UnixNano(),
		NodeInfo: &statusproto.NodeInfo{
			Name:     "node-a",
			SiteName: "site-a",
		},
		Peers: []*statusproto.PeerStatus{
			{
				Name:     "peer-1",
				PeerType: "remote",
				Tunnel: &statusproto.PeerTunnelStatus{
					Interface: "wg0",
					RxBytes:   1000,
					TxBytes:   2000,
				},
				RouteDistances: map[string]int32{"10.0.0.0/8": 1},
				HealthCheck: &statusproto.HealthCheckPeerStatus{
					Enabled: true,
					Status:  "up",
					Rtt:     "5ms",
				},
			},
		},
		RoutingTable: &statusproto.RoutingTableInfo{
			ManagedRouteCount: 3,
			Routes: []*statusproto.RouteEntry{
				{
					Destination: "10.0.0.0/8",
					Family:      "IPv4",
					NextHops: []*statusproto.NextHop{
						{
							Device:   "wg0",
							Gateway:  "10.0.0.1",
							Expected: &statusproto.OptionalBool{Value: true},
							Present:  &statusproto.OptionalBool{Value: false},
							Info: &statusproto.NextHopInfo{
								ObjectName: "site-peer",
								ObjectType: "SiteNodeSlice",
								RouteType:  "pod-cidr",
							},
							RouteTypes: []*statusproto.RouteType{
								{Type: "unicast", Attributes: []string{"onlink"}},
							},
						},
					},
				},
			},
		},
		HealthCheck: &statusproto.HealthCheckStatus{
			Healthy:         true,
			Summary:         "all good",
			PeerCount:       1,
			CheckedAtUnixNs: ts.UnixNano(),
		},
		NodeErrors: []*statusproto.NodeError{
			{Type: "warning", Message: "test error"},
		},
		FetchError:   "fetch err",
		StatusSource: "push",
		NodePodInfo: &statusproto.NodePodInfo{
			PodName:         "pod-1",
			StartTimeUnixNs: ts.UnixNano(),
			Restarts:        2,
		},
		LastPushTimeUnixNs: ts.UnixNano(),
	}

	resp := protoToNodeStatus(full)

	if resp.NodeInfo.Name != "node-a" {
		t.Fatalf("expected node name node-a, got %q", resp.NodeInfo.Name)
	}

	if resp.NodeInfo.SiteName != "site-a" {
		t.Fatalf("expected site name site-a, got %q", resp.NodeInfo.SiteName)
	}

	if !resp.Timestamp.Equal(ts) {
		t.Fatalf("expected timestamp %v, got %v", ts, resp.Timestamp)
	}

	if len(resp.Peers) != 1 || resp.Peers[0].Name != "peer-1" {
		t.Fatalf("unexpected peers: %+v", resp.Peers)
	}

	if resp.Peers[0].Tunnel.RxBytes != 1000 {
		t.Fatalf("expected RxBytes 1000, got %d", resp.Peers[0].Tunnel.RxBytes)
	}

	if resp.Peers[0].RouteDistances["10.0.0.0/8"] != 1 {
		t.Fatalf("expected route distance 1, got %d", resp.Peers[0].RouteDistances["10.0.0.0/8"])
	}

	if resp.Peers[0].HealthCheck == nil || resp.Peers[0].HealthCheck.RTT != "5ms" {
		t.Fatalf("expected health check RTT 5ms, got %v", resp.Peers[0].HealthCheck)
	}

	if resp.RoutingTable.ManagedRouteCount != 3 {
		t.Fatalf("expected 3 managed routes, got %d", resp.RoutingTable.ManagedRouteCount)
	}

	if len(resp.RoutingTable.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(resp.RoutingTable.Routes))
	}

	nh := resp.RoutingTable.Routes[0].NextHops[0]
	if nh.Expected == nil || !*nh.Expected {
		t.Fatalf("expected Expected=true, got %v", nh.Expected)
	}

	if nh.Present == nil || *nh.Present {
		t.Fatalf("expected Present=false, got %v", nh.Present)
	}

	if nh.Info == nil || nh.Info.ObjectName != "site-peer" {
		t.Fatalf("expected info object name site-peer, got %v", nh.Info)
	}

	if len(nh.RouteTypes) != 1 || nh.RouteTypes[0].Type != "unicast" {
		t.Fatalf("unexpected route types: %+v", nh.RouteTypes)
	}

	if resp.HealthCheck == nil || !resp.HealthCheck.Healthy {
		t.Fatalf("expected healthy=true, got %v", resp.HealthCheck)
	}

	if len(resp.NodeErrors) != 1 {
		t.Fatalf("expected 1 node error, got %d", len(resp.NodeErrors))
	}

	if resp.FetchError != "fetch err" {
		t.Fatalf("expected fetch error, got %q", resp.FetchError)
	}

	if resp.StatusSource != "push" {
		t.Fatalf("expected source push, got %q", resp.StatusSource)
	}

	if resp.NodePodInfo == nil || resp.NodePodInfo.PodName != "pod-1" || resp.NodePodInfo.Restarts != 2 {
		t.Fatalf("unexpected node pod info: %+v", resp.NodePodInfo)
	}

	if resp.LastPushTime == nil || !resp.LastPushTime.Equal(ts) {
		t.Fatalf("expected last push time %v, got %v", ts, resp.LastPushTime)
	}
}

func TestProtoToNodeStatusNil(t *testing.T) {
	resp := protoToNodeStatus(nil)
	if resp.NodeInfo.Name != "" {
		t.Fatalf("expected empty node status for nil input, got %+v", resp)
	}
}

func TestProtoToParsedDelta(t *testing.T) {
	delta := &statusproto.NodeStatusDelta{
		UpdatedFields: []string{"nodeInfo", "peers", "healthCheck"},
		NodeInfo: &statusproto.NodeInfo{
			Name:     "node-a",
			SiteName: "site-b",
		},
		Peers: []*statusproto.PeerStatus{
			{Name: "peer-2", PeerType: "local"},
		},
		// healthCheck listed in updated_fields but nil -- means cleared.
	}

	pd := protoToParsedDelta(delta)
	if pd.nodeInfo == nil || pd.nodeInfo.SiteName != "site-b" {
		t.Fatalf("expected nodeInfo with site-b, got %v", pd.nodeInfo)
	}

	if len(pd.peers) != 1 || pd.peers[0].Name != "peer-2" {
		t.Fatalf("unexpected peers: %+v", pd.peers)
	}

	if !pd.nullFields["healthCheck"] {
		t.Fatalf("expected healthCheck to be marked as null/cleared")
	}

	if pd.routingTable != nil {
		t.Fatalf("expected routingTable to be nil (not in updated_fields)")
	}
}

func TestProtoToParsedDeltaEmptyPeers(t *testing.T) {
	delta := &statusproto.NodeStatusDelta{
		UpdatedFields: []string{"peers"},
		// Peers field is nil but listed in updated_fields -- means empty list.
	}

	pd := protoToParsedDelta(delta)
	if pd.peers == nil {
		t.Fatalf("expected non-nil empty peers slice")
	}

	if len(pd.peers) != 0 {
		t.Fatalf("expected empty peers, got %d", len(pd.peers))
	}
}

func TestHandleProtoWSMessage(t *testing.T) {
	health := &healthState{statusCache: NewNodeStatusCache()}

	t.Run("invalid proto", func(t *testing.T) {
		msgType, ack := handleProtoWSMessage(health, []byte("not-proto"), "ws")
		if msgType != "node_status_resync" || ack.Status != "resync_required" {
			t.Fatalf("expected resync on invalid proto, got type=%q ack=%+v", msgType, ack)
		}
	})

	t.Run("missing node name", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{Type: "node_status_full"}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_resync" || ack.Reason != "nodeName is required" {
			t.Fatalf("expected nodeName required, got type=%q ack=%+v", msgType, ack)
		}
	})

	t.Run("full success", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:     "node_status_full",
			NodeName: "node-proto",
			Status: &statusproto.NodeStatusFull{
				NodeInfo: &statusproto.NodeInfo{Name: "node-proto", SiteName: "site-proto"},
			},
		}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_ack" || ack.Status != "ok" || ack.Revision == 0 {
			t.Fatalf("expected full ack success, got type=%q ack=%+v", msgType, ack)
		}

		cached, ok := health.statusCache.Get("node-proto")
		if !ok || cached.Status.NodeInfo.SiteName != "site-proto" {
			t.Fatalf("expected cached status with site-proto, got %+v", cached)
		}
	})

	t.Run("full missing status", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:     "node_status_full",
			NodeName: "node-proto",
		}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_resync" || ack.Reason != "full message missing status" {
			t.Fatalf("expected missing status resync, got type=%q ack=%+v", msgType, ack)
		}
	})

	t.Run("delta success", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:         "node_status_delta",
			NodeName:     "node-proto",
			BaseRevision: 1,
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"},
				NodeInfo:      &statusproto.NodeInfo{Name: "node-proto", SiteName: "site-updated"},
			},
		}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_ack" || ack.Status != "ok" || ack.Revision < 2 {
			t.Fatalf("expected delta ack success, got type=%q ack=%+v", msgType, ack)
		}

		cached, ok := health.statusCache.Get("node-proto")
		if !ok || cached.Status.NodeInfo.SiteName != "site-updated" {
			t.Fatalf("expected updated site name, got %+v", cached)
		}
	})

	t.Run("delta conflict", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:         "node_status_delta",
			NodeName:     "node-proto",
			BaseRevision: 999,
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"},
				NodeInfo:      &statusproto.NodeInfo{Name: "node-proto", SiteName: "conflict"},
			},
		}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_resync" || ack.Reason != "base revision mismatch" {
			t.Fatalf("expected conflict resync, got type=%q ack=%+v", msgType, ack)
		}
	})

	t.Run("delta missing delta", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:     "node_status_delta",
			NodeName: "node-proto",
		}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_resync" || ack.Reason != "delta message missing delta" {
			t.Fatalf("expected missing delta resync, got type=%q ack=%+v", msgType, ack)
		}
	})

	t.Run("unsupported type", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:     "node_status_unknown",
			NodeName: "node-proto",
		}
		data, _ := proto.Marshal(msg)

		msgType, ack := handleProtoWSMessage(health, data, "ws")
		if msgType != "node_status_resync" || ack.Reason != "unsupported message type" {
			t.Fatalf("expected unsupported type resync, got type=%q ack=%+v", msgType, ack)
		}
	})
}

func TestHandleProtoPushRequest(t *testing.T) {
	health := &healthState{statusCache: NewNodeStatusCache()}

	t.Run("invalid proto", func(t *testing.T) {
		_, code, err := handleProtoPushRequest(health, []byte("bad"), "push")
		if err == nil || code != 400 {
			t.Fatalf("expected 400 on invalid proto, got code=%d err=%v", code, err)
		}
	})

	t.Run("full success", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:     "node_status_full",
			NodeName: "node-push",
			Status: &statusproto.NodeStatusFull{
				NodeInfo: &statusproto.NodeInfo{Name: "node-push", SiteName: "site-push"},
			},
		}
		data, _ := proto.Marshal(msg)

		ack, code, err := handleProtoPushRequest(health, data, "push")
		if err != nil || code != 200 {
			t.Fatalf("expected 200, got code=%d err=%v", code, err)
		}

		if ack.Status != "ok" || ack.Revision == 0 {
			t.Fatalf("unexpected ack: %+v", ack)
		}
	})

	t.Run("delta success", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:         "node_status_delta",
			NodeName:     "node-push",
			BaseRevision: 1,
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"},
				NodeInfo:      &statusproto.NodeInfo{Name: "node-push", SiteName: "site-updated"},
			},
		}
		data, _ := proto.Marshal(msg)

		ack, code, err := handleProtoPushRequest(health, data, "push")
		if err != nil || code != 200 {
			t.Fatalf("expected 200, got code=%d err=%v", code, err)
		}

		if ack.Status != "ok" || ack.Revision < 2 {
			t.Fatalf("unexpected ack: %+v", ack)
		}
	})

	t.Run("delta conflict", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:         "node_status_delta",
			NodeName:     "node-push",
			BaseRevision: 999,
			Delta: &statusproto.NodeStatusDelta{
				UpdatedFields: []string{"nodeInfo"},
				NodeInfo:      &statusproto.NodeInfo{Name: "node-push", SiteName: "conflict"},
			},
		}
		data, _ := proto.Marshal(msg)

		ack, code, err := handleProtoPushRequest(health, data, "push")
		if err != nil {
			t.Fatalf("expected no error on conflict, got %v", err)
		}

		if code != http.StatusTooManyRequests || ack.Status != "resync_required" {
			t.Fatalf("expected 429 resync, got code=%d ack=%+v", code, ack)
		}
	})
}

func TestMarshalProtoAck(t *testing.T) {
	ack := NodeStatusPushAck{Status: "ok", Revision: 42, Reason: "test"}

	data, err := marshalProtoAck("node_status_ack", ack)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var pbAck statusproto.NodeStatusAck
	if err := proto.Unmarshal(data, &pbAck); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if pbAck.Status != "ok" || pbAck.Revision != 42 || pbAck.Reason != "test" {
		t.Fatalf("unexpected ack: status=%s revision=%d reason=%s", pbAck.Status, pbAck.Revision, pbAck.Reason)
	}
}

func TestExtractNodeNameFromProtoMessage(t *testing.T) {
	t.Run("from node_name field", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:     "node_status_full",
			NodeName: "node-x",
		}

		data, _ := proto.Marshal(msg)
		if got := extractNodeNameFromProtoMessage(data); got != "node-x" {
			t.Fatalf("expected node-x, got %q", got)
		}
	})

	t.Run("from status nodeInfo", func(t *testing.T) {
		msg := &statusproto.NodeStatusMessage{
			Type:   "node_status_full",
			Status: &statusproto.NodeStatusFull{NodeInfo: &statusproto.NodeInfo{Name: "node-y"}},
		}

		data, _ := proto.Marshal(msg)
		if got := extractNodeNameFromProtoMessage(data); got != "node-y" {
			t.Fatalf("expected node-y, got %q", got)
		}
	})

	t.Run("invalid data", func(t *testing.T) {
		if got := extractNodeNameFromProtoMessage([]byte("bad")); got != "" {
			t.Fatalf("expected empty, got %q", got)
		}
	})
}

func TestIsProtobufContentType(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"application/x-protobuf", true},
		{"application/protobuf", true},
		{"application/x-protobuf; charset=binary", true},
		{"application/json", false},
		{"", false},
	}
	for _, tt := range tests {
		r, _ := http.NewRequest("POST", "/", nil)
		r.Header.Set("Content-Type", tt.ct)

		if got := isProtobufContentType(r); got != tt.want {
			t.Errorf("isProtobufContentType(%q) = %v, want %v", tt.ct, got, tt.want)
		}
	}
}
