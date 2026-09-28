// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/proto"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func measurementTestStatus(count int) *statusproto.NodeStatusFull {
	status := &statusproto.NodeStatusFull{NodeInfo: &statusproto.NodeInfo{Name: "node-a"}}
	for i := range count {
		status.Peers = append(status.Peers, &statusproto.PeerStatus{
			Name: fmt.Sprintf("peer-%d", i), PeerType: "site", SiteName: "site-a",
			PodCidrGateways: []string{"10.244.0.1"}, SkipPodCidrRoutes: true,
			RouteDistances: map[string]int32{"10.244.0.0/24": 1}, RouteDestinations: []string{"10.244.0.0/24"},
			Tunnel: &statusproto.PeerTunnelStatus{
				Interface: "wg0", Protocol: "wireguard", PublicKey: fmt.Sprintf("key-%d", i),
				Endpoint: "10.224.0.1:51820", AllowedIps: []string{"10.244.0.0/24"},
				RxBytes: 100, TxBytes: 200, LastHandshakeUnixNs: 123456789,
			},
			HealthCheck: &statusproto.HealthCheckPeerStatus{Enabled: true, Status: "up", Uptime: "1m", Rtt: "1ms"},
		})
	}

	return status
}

func measurementMessage(t *testing.T, status NodeStatusResponse, rev uint64) *statusproto.NodeStatusMessage {
	t.Helper()

	pb, err := netstatus.PeerMeasurementsToProto(status.Peers)
	if err != nil {
		t.Fatal(err)
	}

	return &statusproto.NodeStatusMessage{
		Type: "node_status_delta", NodeName: "node-a", BaseRevision: rev,
		Delta: &statusproto.NodeStatusDelta{UpdatedFields: []string{"peerMeasurements"}, PeerMeasurements: pb},
	}
}

func applyMeasurementMessage(t *testing.T, health *healthState, message *statusproto.NodeStatusMessage) NodeStatusPushAck {
	t.Helper()

	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := decodeProtoWSMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	_, ack := handleProtoWSMessage(health, decoded, "ws")

	return ack
}

func TestPeerMeasurementsCacheOwnsDecodedFrameData(t *testing.T) {
	cache := NewNodeStatusCache()
	health := &healthState{statusCache: cache}

	var buffer []byte

	applyReusedFrame := func(message *statusproto.NodeStatusMessage) NodeStatusPushAck {
		t.Helper()

		var err error

		buffer, err = proto.MarshalOptions{}.MarshalAppend(buffer[:0], message)
		if err != nil {
			t.Fatal(err)
		}

		decoded, err := decodeProtoWSMessage(buffer)
		if err != nil {
			t.Fatal(err)
		}

		_, ack := handleProtoWSMessage(health, decoded, "ws")
		if ack.Status != "ok" {
			t.Fatalf("apply reused frame: %+v", ack)
		}

		for i := range buffer {
			buffer[i] = 0xa5
		}

		return ack
	}

	full := measurementTestStatus(2)
	wantFull := protoToNodeStatus(full)
	ack := applyReusedFrame(&statusproto.NodeStatusMessage{
		Type: "node_status_full", NodeName: "node-a", Status: full,
	})
	oldSnapshot := cache.entries["node-a"].Status

	if !reflect.DeepEqual(*oldSnapshot, wantFull) {
		t.Fatal("full status retained overwritten frame data")
	}

	message := measurementMessage(t, wantFull, ack.Revision)
	measurements := message.Delta.PeerMeasurements
	wantNext := protoToNodeStatus(proto.Clone(full).(*statusproto.NodeStatusFull))

	for i := range wantNext.Peers {
		measurements.RxBytes[i], measurements.TxBytes[i] = 321, 654
		measurements.LastHandshakeUnixNs[i] = 987654321
		measurements.Uptime[i], measurements.Rtt[i] = "2h", "7ms"
		wantNext.Peers[i].Tunnel.RxBytes, wantNext.Peers[i].Tunnel.TxBytes = 321, 654
		wantNext.Peers[i].Tunnel.LastHandshake = time.Unix(0, 987654321)
		wantNext.Peers[i].HealthCheck.Uptime, wantNext.Peers[i].HealthCheck.RTT = "2h", "7ms"
	}

	applyReusedFrame(message)

	if !reflect.DeepEqual(*cache.entries["node-a"].Status, wantNext) {
		t.Fatal("compact update retained overwritten frame data or lost static metadata")
	}

	if !reflect.DeepEqual(*oldSnapshot, wantFull) {
		t.Fatal("reusing frame storage or applying measurements mutated the old snapshot")
	}

	identity := cache.entries["node-a"].peerIdentity
	compactSnapshot := cache.entries["node-a"].Status
	message.BaseRevision = cache.entries["node-a"].Revision
	applyReusedFrame(message)

	if cache.entries["node-a"].peerIdentity != identity ||
		!reflect.DeepEqual(*cache.entries["node-a"].Status, wantNext) ||
		!reflect.DeepEqual(*compactSnapshot, wantNext) {
		t.Fatal("warm identity memo retained frame data or mutated a compact snapshot")
	}
}

func TestPeerMeasurementsApplyAndClearImmutable(t *testing.T) {
	for _, count := range []int{0, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			status := protoToNodeStatus(measurementTestStatus(count))
			cache := NewNodeStatusCache()
			rev := cache.StoreFull("node-a", status, "ws")
			old := cache.entries["node-a"]

			before, err := json.Marshal(old.Status)
			if err != nil {
				t.Fatal(err)
			}

			message := measurementMessage(t, status, rev)

			m := message.Delta.PeerMeasurements
			for i := range count {
				m.RxBytes[i], m.TxBytes[i], m.LastHandshakeUnixNs[i] = 0, 0, 0
				m.Uptime[i], m.Rtt[i] = "", ""
			}

			counter := peerMeasurementUpdatesTotal.WithLabelValues("applied")
			start := testutil.ToFloat64(counter)

			ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, message)
			if ack.Status != "ok" || ack.Revision != rev+1 {
				t.Fatalf("ack: %+v", ack)
			}

			if testutil.ToFloat64(counter) != start+1 {
				t.Fatal("missing applied metric")
			}

			next := cache.entries["node-a"].Status
			for i, peer := range next.Peers {
				if !netstatus.PeerMetadataEqual(status.Peers[i], peer) {
					t.Fatal("static metadata lost")
				}

				if peer.Tunnel.RxBytes != 0 || peer.Tunnel.TxBytes != 0 || !peer.Tunnel.LastHandshake.IsZero() ||
					peer.HealthCheck.Uptime != "" || peer.HealthCheck.RTT != "" {
					t.Fatal("measurements not cleared")
				}

				if peer.HealthCheck == old.Status.Peers[i].HealthCheck {
					t.Fatal("health pointer was reused")
				}
			}

			after, err := json.Marshal(old.Status)
			if err != nil {
				t.Fatal(err)
			}

			if string(before) != string(after) || cache.entries["node-a"] == old {
				t.Fatal("cached snapshot mutated")
			}
		})
	}
}

func TestPeerMeasurementsRejectWithoutMutation(t *testing.T) {
	tests := map[string]func(*statusproto.NodeStatusMessage, *NodeStatusResponse){
		"zero revision":  func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.BaseRevision = 0 },
		"stale revision": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.BaseRevision++ },
		"unknown identity": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.PeerMeasurements.IdentityDigest[0] ^= 1
		},
		"missing identity": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.PeerMeasurements.IdentityDigest = nil
		},
		"count": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Delta.PeerMeasurements.PeerCount++ },
		"rx":    func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Delta.PeerMeasurements.RxBytes = nil },
		"tx":    func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Delta.PeerMeasurements.TxBytes = nil },
		"handshake": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.PeerMeasurements.LastHandshakeUnixNs = nil
		},
		"uptime": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Delta.PeerMeasurements.Uptime = nil },
		"rtt": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.PeerMeasurements.Rtt = append(m.Delta.PeerMeasurements.Rtt, "")
		},
		"reordered": func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) {
			s.Peers[0], s.Peers[1] = s.Peers[1], s.Peers[0]
		},
		"deleted": func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers = s.Peers[:1] },
		"added": func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) {
			s.Peers = append(s.Peers, WireGuardPeerStatus{Name: "new"})
		},
		"duplicate":       func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers[1] = s.Peers[0] },
		"renamed":         func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers[1].Name = "other" },
		"key":             func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers[1].Tunnel.PublicKey = "other" },
		"interface":       func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers[1].Tunnel.Interface = "other" },
		"protocol":        func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers[1].Tunnel.Protocol = "other" },
		"absent health":   func(_ *statusproto.NodeStatusMessage, s *NodeStatusResponse) { s.Peers[1].HealthCheck = nil },
		"missing mask":    func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Delta.UpdatedFields = nil },
		"missing payload": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Delta.PeerMeasurements = nil },
		"duplicate mask": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.UpdatedFields = append(m.Delta.UpdatedFields, "peerMeasurements")
		},
		"peers mask conflict": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.UpdatedFields = append(m.Delta.UpdatedFields, "peers")
		},
		"peers payload conflict": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Delta.Peers = []*statusproto.PeerStatus{{Name: "other"}}
		},
		"full conflict": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) { m.Status = measurementTestStatus(2) },
		"full type conflict": func(m *statusproto.NodeStatusMessage, _ *NodeStatusResponse) {
			m.Type = "node_status_full"
			m.Status = measurementTestStatus(2)
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			status := protoToNodeStatus(measurementTestStatus(2))
			message := measurementMessage(t, status, 1)
			change(message, &status)
			message.Delta.UpdatedFields = append(message.Delta.UpdatedFields, "fetchError")
			message.Delta.FetchError = "must not apply"
			cache := NewNodeStatusCache()
			cache.StoreFull("node-a", status, "ws")
			old := cache.entries["node-a"]

			before, err := json.Marshal(old.Status)
			if err != nil {
				t.Fatal(err)
			}

			ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, message)
			if ack.Status != "resync_required" {
				t.Fatalf("accepted malformed batch: %+v", ack)
			}

			after, err := json.Marshal(old.Status)
			if err != nil {
				t.Fatal(err)
			}

			if old != cache.entries["node-a"] || string(before) != string(after) {
				t.Fatal("partially mutated cache")
			}
		})
	}

	t.Run("missing cache", func(t *testing.T) {
		status := protoToNodeStatus(measurementTestStatus(1))
		cache := NewNodeStatusCache()
		counter := peerMeasurementUpdatesTotal.WithLabelValues("resync")
		before := testutil.ToFloat64(counter)

		ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, measurementMessage(t, status, 1))
		if ack.Status != "resync_required" || cache.Len() != 0 || testutil.ToFloat64(counter) != before+1 {
			t.Fatalf("missing-base behavior: %+v", ack)
		}
	})
}

func TestPeerMeasurementsLegacyAndFullResync(t *testing.T) {
	cache := NewNodeStatusCache()
	health := &healthState{statusCache: cache}
	full := measurementTestStatus(2)

	ack := applyMeasurementMessage(t, health, &statusproto.NodeStatusMessage{Type: "node_status_full", NodeName: "node-a", Status: full})
	if ack.Status != "ok" {
		t.Fatal(ack)
	}

	full.Peers[0].Tunnel.RxBytes++

	ack = applyMeasurementMessage(t, health, &statusproto.NodeStatusMessage{
		Type: "node_status_delta", NodeName: "node-a", BaseRevision: ack.Revision,
		Delta: &statusproto.NodeStatusDelta{UpdatedFields: []string{"peers"}, Peers: full.Peers},
	})
	if ack.Status != "ok" || cache.entries["node-a"].Status.Peers[0].Tunnel.RxBytes != 101 {
		t.Fatal("old protobuf node failed")
	}

	rev, conflict, err := cache.ApplyDelta("node-a", ack.Revision, map[string]json.RawMessage{"peers": []byte("[]")}, "ws")
	if err != nil || conflict || len(cache.entries["node-a"].Status.Peers) != 0 {
		t.Fatal("legacy JSON clear failed")
	}

	stale := measurementMessage(t, protoToNodeStatus(full), rev)
	if got := applyMeasurementMessage(t, health, stale); got.Status != "resync_required" {
		t.Fatal("missing peers accepted")
	}

	ack = applyMeasurementMessage(t, health, &statusproto.NodeStatusMessage{Type: "node_status_full", NodeName: "node-a", Status: full})
	if got := applyMeasurementMessage(t, health, measurementMessage(t, *cache.entries["node-a"].Status, ack.Revision)); got.Status != "ok" {
		t.Fatal("full resync did not restore compact path")
	}

	data, err := marshalProtoAck("node_status_ack", ack)
	if err != nil {
		t.Fatal(err)
	}

	var pbAck statusproto.NodeStatusAck
	if err := proto.Unmarshal(data, &pbAck); err != nil {
		t.Fatal(err)
	}

	if !pbAck.PeerMeasurements || pbAck.Revision != ack.Revision {
		t.Fatal("capability ACK missing")
	}
}

func TestTypedProtoDeltaAllFieldsAndClearings(t *testing.T) {
	cache := NewNodeStatusCache()
	cache.StoreFull("node-a", NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}}, "ws")
	delta := &statusproto.NodeStatusDelta{
		UpdatedFields:   []string{"timestamp", "nodeInfo", "peers", "routingTable", "healthCheck", "nodeErrors", "bpfEntries", "fetchError", "lastPushTime", "statusSource", "nodePodInfo"},
		TimestampUnixNs: 123, NodeInfo: &statusproto.NodeInfo{Name: "node-a", SiteName: "new"},
		Peers: measurementTestStatus(1).Peers, RoutingTable: &statusproto.RoutingTableInfo{ManagedRouteCount: 2},
		HealthCheck: &statusproto.HealthCheckStatus{Healthy: true, CheckedAtUnixNs: 456},
		NodeErrors:  []*statusproto.NodeError{{Type: "test", Message: "error"}},
		BpfEntries:  []*statusproto.BpfEntry{{Cidr: "10.0.0.0/24"}},
		FetchError:  "failed", LastPushTimeUnixNs: 789, StatusSource: "push", NodePodInfo: &statusproto.NodePodInfo{PodName: "pod"},
	}

	rev, conflict, err := cache.ApplyParsedDelta("node-a", 1, protoToParsedDelta(delta), "ws")
	if err != nil || conflict {
		t.Fatalf("apply: %v %v", err, conflict)
	}

	got := cache.entries["node-a"].Status
	if !got.Timestamp.Equal(time.Unix(0, 123)) || got.NodeInfo.SiteName != "new" || len(got.Peers) != 1 ||
		got.RoutingTable.ManagedRouteCount != 2 || got.HealthCheck == nil || !got.HealthCheck.CheckedAt.Equal(time.Unix(0, 456)) ||
		len(got.NodeErrors) != 1 || len(got.BpfEntries) != 1 || got.FetchError != "failed" ||
		got.LastPushTime == nil || !got.LastPushTime.Equal(time.Unix(0, 789)) || got.StatusSource != "push" || got.NodePodInfo.PodName != "pod" {
		t.Fatalf("typed fields not applied: %+v", got)
	}

	clearDelta := &statusproto.NodeStatusDelta{UpdatedFields: delta.UpdatedFields}

	_, conflict, err = cache.ApplyParsedDelta("node-a", rev, protoToParsedDelta(clearDelta), "ws")
	if err != nil || conflict {
		t.Fatal("clear failed")
	}

	got = cache.entries["node-a"].Status
	if !got.Timestamp.IsZero() || got.NodeInfo.Name != "node-a" || got.NodeInfo.SiteName != "" || len(got.Peers) != 0 ||
		!reflect.DeepEqual(got.RoutingTable, RoutingTableInfo{}) || got.HealthCheck != nil || len(got.NodeErrors) != 0 ||
		len(got.BpfEntries) != 0 || got.FetchError != "" || got.LastPushTime != nil || got.StatusSource != "" || got.NodePodInfo != nil {
		t.Fatalf("typed fields not cleared: %+v", got)
	}
}

func BenchmarkApplyPeerDelta2000(b *testing.B) {
	full := measurementTestStatus(2000)
	status := protoToNodeStatus(full)

	measurements, err := netstatus.PeerMeasurementsToProto(status.Peers)
	if err != nil {
		b.Fatal(err)
	}

	for _, compact := range []bool{false, true} {
		name := "full-peers"
		delta := &statusproto.NodeStatusDelta{UpdatedFields: []string{"peers"}, Peers: full.Peers}

		if compact {
			name = "compact"
			delta = &statusproto.NodeStatusDelta{UpdatedFields: []string{"peerMeasurements"}, PeerMeasurements: measurements}
		}

		b.Run(name, func(b *testing.B) {
			data, err := proto.Marshal(delta)
			if err != nil {
				b.Fatal(err)
			}

			cache := NewNodeStatusCache()
			rev := cache.StoreFull("node-a", status, "ws")

			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				var decoded statusproto.NodeStatusDelta
				if err := proto.Unmarshal(data, &decoded); err != nil {
					b.Fatal(err)
				}

				next, conflict, err := cache.ApplyParsedDelta("node-a", rev, protoToParsedDelta(&decoded), "ws")
				if err != nil || conflict {
					b.Fatalf("apply: %v %v", err, conflict)
				}

				rev = next
			}

			b.ReportMetric(float64(len(data)), "wire-B/op")
		})
	}
}
