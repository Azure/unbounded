// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func measuredNodeStatus() *NodeStatusResponse {
	status := testNodeStatus("old")
	status.HealthCheck = &HealthCheckStatus{Healthy: true, Summary: "ok", PeerCount: 1, CheckedAt: testStatusTime}
	status.Peers[0].HealthCheck = &HealthCheckPeerStatus{Enabled: true, Status: "up", Uptime: "1m", RTT: "1ms"}

	return status
}

func TestCriticalStatusIgnoresOnlyMeasurements(t *testing.T) {
	measurements := map[string]func(*NodeStatusResponse){
		"timestamp": func(s *NodeStatusResponse) { s.Timestamp = s.Timestamp.Add(time.Second) },
		"checkedAt": func(s *NodeStatusResponse) { s.HealthCheck.CheckedAt = s.HealthCheck.CheckedAt.Add(time.Second) },
		"rx":        func(s *NodeStatusResponse) { s.Peers[0].Tunnel.RxBytes++ },
		"tx":        func(s *NodeStatusResponse) { s.Peers[0].Tunnel.TxBytes++ },
		"handshake": func(s *NodeStatusResponse) { s.Peers[0].Tunnel.LastHandshake = time.Time{} },
		"uptime":    func(s *NodeStatusResponse) { s.Peers[0].HealthCheck.Uptime = "2m" },
		"rtt":       func(s *NodeStatusResponse) { s.Peers[0].HealthCheck.RTT = "2ms" },
	}
	for name, change := range measurements {
		t.Run(name, func(t *testing.T) {
			prev, curr := measuredNodeStatus(), measuredNodeStatus()
			change(curr)

			before := nodeStatusToProto(curr)
			if !reflect.DeepEqual(stripPeerStats(prev), stripPeerStats(curr)) {
				t.Fatal("measurement triggered critical comparison")
			}

			if delta := typedStatusDelta(prev, criticalStatus(prev, curr), true, false); delta != nil {
				t.Fatalf("measurement leaked into critical delta: %v", delta)
			}

			if typedStatusDelta(prev, curr, true, true) == nil {
				t.Fatal("statistics refresh lost measurement")
			}

			if !proto.Equal(before, nodeStatusToProto(curr)) {
				t.Fatal("shared current snapshot mutated")
			}
		})
	}

	critical := map[string]func(*NodeStatusResponse){
		"nodeInfo":            func(s *NodeStatusResponse) { s.NodeInfo.SiteName = "new" },
		"healthy":             func(s *NodeStatusResponse) { s.HealthCheck.Healthy = false },
		"summary":             func(s *NodeStatusResponse) { s.HealthCheck.Summary = "failed" },
		"count":               func(s *NodeStatusResponse) { s.HealthCheck.PeerCount++ },
		"health removal":      func(s *NodeStatusResponse) { s.HealthCheck = nil },
		"peer status":         func(s *NodeStatusResponse) { s.Peers[0].HealthCheck.Status = "down" },
		"peer enabled":        func(s *NodeStatusResponse) { s.Peers[0].HealthCheck.Enabled = false },
		"peer health removal": func(s *NodeStatusResponse) { s.Peers[0].HealthCheck = nil },
		"endpoint":            func(s *NodeStatusResponse) { s.Peers[0].Tunnel.Endpoint = "new" },
		"error":               func(s *NodeStatusResponse) { s.NodeErrors = []NodeError{{Type: "test", Message: "failed"}} },
		"error clear":         func(s *NodeStatusResponse) { s.FetchError = "" },
		"bpf":                 func(s *NodeStatusResponse) { s.BpfEntries = []BpfEntry{{CIDR: "10.0.0.0/24"}} },
		"route":               func(s *NodeStatusResponse) { s.RoutingTable.ManagedRouteCount++ },
	}
	for name, change := range critical {
		t.Run(name, func(t *testing.T) {
			prev, curr := measuredNodeStatus(), measuredNodeStatus()
			change(curr)
			curr.Timestamp = curr.Timestamp.Add(time.Second)

			curr.Peers[0].Tunnel.RxBytes++
			if reflect.DeepEqual(stripPeerStats(prev), stripPeerStats(curr)) {
				t.Fatal("critical change filtered")
			}

			published := criticalStatus(prev, curr)

			delta := typedStatusDelta(prev, published, true, false)
			if delta == nil || slices.Contains(delta.UpdatedFields, "timestamp") || delta.PeerMeasurements != nil {
				t.Fatalf("invalid critical delta: %v", delta)
			}

			if published.Peers[0].Tunnel.RxBytes != prev.Peers[0].Tunnel.RxBytes {
				t.Fatal("critical change advanced statistics baseline")
			}
		})
	}
}

func TestTypedStatusDeltaCompactCompatibility(t *testing.T) {
	prev, curr := measuredNodeStatus(), measuredNodeStatus()
	curr.Peers[0].Tunnel.RxBytes = 0
	curr.Peers[0].Tunnel.TxBytes = 0
	curr.Peers[0].Tunnel.LastHandshake = time.Time{}
	curr.Peers[0].HealthCheck.Uptime = ""

	curr.Peers[0].HealthCheck.RTT = ""
	for _, compact := range []bool{false, true} {
		delta := typedStatusDelta(prev, curr, compact, false)
		if compact {
			if delta.PeerMeasurements == nil || len(delta.Peers) != 0 || !slices.Contains(delta.UpdatedFields, "peerMeasurements") {
				t.Fatalf("missing compact measurements: %v", delta)
			}

			m := delta.PeerMeasurements
			if m.RxBytes[0] != 0 || m.TxBytes[0] != 0 || m.LastHandshakeUnixNs[0] != 0 || m.Uptime[0] != "" || m.Rtt[0] != "" {
				t.Fatal("zero measurements did not clear")
			}
		} else if delta.PeerMeasurements != nil || len(delta.Peers) != 1 {
			t.Fatalf("old controller requires peers replacement: %v", delta)
		}

		data, err := proto.Marshal(delta)
		if err != nil {
			t.Fatal(err)
		}

		var decoded statusproto.NodeStatusDelta
		if err := proto.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}

		if !proto.Equal(delta, &decoded) {
			t.Fatal("delta did not round trip")
		}
	}

	for name, change := range map[string]func(*NodeStatusResponse){
		"added":      func(s *NodeStatusResponse) { p := s.Peers[0]; p.Name = "peer-b"; s.Peers = append(s.Peers, p) },
		"deleted":    func(s *NodeStatusResponse) { s.Peers = nil },
		"identity":   func(s *NodeStatusResponse) { s.Peers[0].Name = "other" },
		"public key": func(s *NodeStatusResponse) { s.Peers[0].Tunnel.PublicKey = "other" },
		"interface":  func(s *NodeStatusResponse) { s.Peers[0].Tunnel.Interface = "other" },
		"protocol":   func(s *NodeStatusResponse) { s.Peers[0].Tunnel.Protocol = "other" },
		"metadata":   func(s *NodeStatusResponse) { s.Peers[0].SiteName = "other" },
		"health":     func(s *NodeStatusResponse) { s.Peers[0].HealthCheck.Status = "down" },
		"unnamed":    func(s *NodeStatusResponse) { s.Peers[0].Name = "" },
	} {
		t.Run(name, func(t *testing.T) {
			next := measuredNodeStatus()
			change(next)

			delta := typedStatusDelta(prev, next, true, false)
			if delta.PeerMeasurements != nil || !slices.Contains(delta.UpdatedFields, "peers") {
				t.Fatalf("topology change requires replacement: %v", delta)
			}
		})
	}

	if typedStatusDelta(prev, prev, true, false) != nil {
		t.Fatal("no-op produced delta")
	}

	if typedStatusDelta(nil, curr, true, true) != nil {
		t.Fatal("first update must be full")
	}

	if got := typedStatusDelta(prev, prev, true, true); !slices.Equal(got.UpdatedFields, []string{"timestamp"}) {
		t.Fatalf("periodic freshness: %v", got)
	}
}

func TestTypedStatusDeltaFieldClearings(t *testing.T) {
	prev := measuredNodeStatus()
	prev.NodeErrors = []NodeError{{Type: "test"}}
	prev.LastPushTime = &prev.Timestamp
	prev.StatusSource = "push"
	prev.NodePodInfo = &statusv1alpha1.NodePodInfo{PodName: "old"}
	prev.BpfEntries = []BpfEntry{{CIDR: "10.0.0.0/24"}}
	prev.RoutingTable.ManagedRouteCount = 1
	curr := &NodeStatusResponse{}
	delta := typedStatusDelta(prev, curr, true, false)

	want := []string{"timestamp", "nodeInfo", "peers", "routingTable", "healthCheck", "nodeErrors", "bpfEntries", "fetchError", "lastPushTime", "statusSource", "nodePodInfo"}
	if !slices.Equal(delta.UpdatedFields, want) {
		t.Fatalf("clearings %v, want %v", delta.UpdatedFields, want)
	}
}

func TestTypedStatusDeltaReorderingAndDuplicateFallback(t *testing.T) {
	prev, curr := measuredNodeStatus(), measuredNodeStatus()
	other := prev.Peers[0]
	other.Name = "peer-b"
	prev.Peers = append(prev.Peers, other)

	curr.Peers = []WireGuardPeerStatus{other, curr.Peers[0]}
	if delta := typedStatusDelta(prev, curr, true, false); delta.PeerMeasurements != nil || len(delta.Peers) != 2 {
		t.Fatal("reordered base must be replaced")
	}

	sortStatusPeers(curr.Peers)

	if delta := typedStatusDelta(prev, curr, true, false); delta != nil {
		t.Fatal("canonical ordering must eliminate iteration-only changes")
	}

	prev.Peers[1] = prev.Peers[0]
	curr.Peers = append([]WireGuardPeerStatus(nil), prev.Peers...)

	curr.Peers[0].Tunnel.RxBytes++
	if delta := typedStatusDelta(prev, curr, true, false); delta.PeerMeasurements != nil || len(delta.Peers) != 2 {
		t.Fatal("duplicate identities must use legacy replacement")
	}
}

func TestWebSocketCriticalNoopStatsAndResync(t *testing.T) {
	for _, mode := range []string{"critical", "stats-resync", "full-refresh"} {
		t.Run(mode, func(t *testing.T) {
			received := make(chan *statusproto.NodeStatusMessage, 32)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}

				defer func() { _ = conn.Close(websocket.StatusNormalClosure, "done") }()

				var revision uint64

				for {
					_, data, err := conn.Read(ctx)
					if err != nil {
						return
					}

					var message statusproto.NodeStatusMessage
					if err := proto.Unmarshal(data, &message); err != nil {
						t.Errorf("decode: %v", err)
						return
					}

					select {
					case received <- &message:
					case <-ctx.Done():
						return
					}

					revision++

					ack := &statusproto.NodeStatusAck{Status: "ok", Revision: revision, PeerMeasurements: true}
					if mode == "stats-resync" && revision == 2 {
						ack.Status = "resync_required"
					}

					payload, err := proto.Marshal(ack)
					if err != nil {
						t.Errorf("encode ACK: %v", err)
						return
					}

					if err := conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
						return
					}
				}
			}))
			defer server.Close()

			cfg := &config{
				NodeName: "node-a", StatusWSEnabled: true,
				StatusWSURL: "ws" + strings.TrimPrefix(server.URL, "http"), StatusWSAPIServerMode: statusWSAPIServerModeNever,
				CriticalDeltaEvery: time.Hour, StatsDeltaEvery: time.Hour, FullSyncEvery: time.Hour,
			}

			switch mode {
			case "critical":
				cfg.CriticalDeltaEvery = 10 * time.Millisecond
			case "stats-resync":
				cfg.StatsDeltaEvery = 15 * time.Millisecond
			case "full-refresh":
				cfg.FullSyncEvery = 15 * time.Millisecond
			}

			health := blockedBootstrapHealthState()

			startStatusPublishers(ctx, cfg, health)
			defer health.stopStatusPublishers()

			next := func() *statusproto.NodeStatusMessage {
				t.Helper()

				select {
				case message := <-received:
					return message
				case <-time.After(3 * time.Second):
					t.Fatal("publisher did not send expected message")
					return nil
				}
			}
			if message := next(); message.Status == nil || message.Delta != nil {
				t.Fatal("first frame must be full")
			}

			if mode == "critical" {
				select {
				case message := <-received:
					t.Fatalf("timestamp-only critical publication: %v", message)
				case <-time.After(100 * time.Millisecond):
				}

				health.setCNIReady("cbr0", []string{"10.244.7.0/24"})

				if message := next(); !isCNIGuardClearingDelta(message) {
					t.Fatal("critical error clearing lost")
				}
			} else if mode == "stats-resync" {
				if message := next(); message.Delta == nil || !slices.Contains(message.Delta.UpdatedFields, "timestamp") {
					t.Fatal("statistics interval did not publish freshness")
				}

				if message := next(); message.Status == nil || message.Delta != nil {
					t.Fatal("statistics interval must honor resync with full status")
				}
			} else if message := next(); message.Status == nil || message.Delta != nil {
				t.Fatal("periodic full refresh lost")
			}

			cancel()
		})
	}
}

func TestStatusAckNegotiation(t *testing.T) {
	var state statusAckState
	if state.compact.Load() || state.revision.Load() != 0 {
		t.Fatal("new connection negotiated without ACK")
	}

	for _, tc := range []struct {
		name                   string
		data                   []byte
		compact, resync, valid bool
	}{
		{"new controller", mustStatusAck(t, &statusproto.NodeStatusAck{Status: "ok", Revision: 1, PeerMeasurements: true}), true, false, true},
		{"old controller", mustStatusAck(t, &statusproto.NodeStatusAck{Status: "ok", Revision: 2}), false, false, true},
		{"JSON", []byte(`{"type":"node_status_ack","data":{"revision":3}}`), false, false, true},
		{"no base", mustStatusAck(t, &statusproto.NodeStatusAck{Status: "ok", PeerMeasurements: true}), false, false, true},
		{"resync", mustStatusAck(t, &statusproto.NodeStatusAck{Status: "resync_required", Revision: 4, PeerMeasurements: true}), false, true, true},
		{"invalid", []byte("invalid"), false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.pending.Store(true)

			if state.accept(tc.data) != tc.valid || state.compact.Load() != tc.compact || state.resync.Load() != tc.resync {
				t.Fatal("incorrect ACK negotiation")
			}

			if state.pending.Load() == tc.valid {
				t.Fatal("invalid pending-message state")
			}
		})
	}

	state.resync.Store(false)

	if !state.accept(mustStatusAck(t, &statusproto.NodeStatusAck{Status: "ok", Revision: 5, PeerMeasurements: true})) || !state.compact.Load() {
		t.Fatal("full resync ACK did not renegotiate")
	}

	reconnected := &statusAckState{}
	if reconnected.compact.Load() || reconnected.revision.Load() != 0 {
		t.Fatal("capability leaked across connections")
	}
}

func mustStatusAck(t *testing.T, ack *statusproto.NodeStatusAck) []byte {
	t.Helper()

	data, err := proto.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func BenchmarkStatusDelta2000Peers(b *testing.B) {
	prev, curr := measuredNodeStatus(), measuredNodeStatus()

	prev.Peers, curr.Peers = nil, nil
	for i := range 2000 {
		peer := measuredNodeStatus().Peers[0]
		peer.Name = fmt.Sprintf("peer-%d", i)
		peer.Tunnel.AllowedIPs = []string{"10.244.0.0/24"}
		peer.PodCIDRGateways = []string{"10.244.0.1"}
		peer.RouteDistances = map[string]int{"10.244.0.0/24": 1}
		prev.Peers = append(prev.Peers, peer)
		peer.Tunnel.RxBytes++
		curr.Peers = append(curr.Peers, peer)
	}

	for _, mode := range []string{"legacy-json-full-peers", "typed-full-peers", "compact"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				var delta *statusproto.NodeStatusDelta

				if mode == "legacy-json-full-peers" {
					raw, err := computeStatusDelta(prev, curr)
					if err != nil {
						b.Fatal(err)
					}

					delta = nodeStatusDeltaToProto(raw)
				} else {
					delta = typedStatusDelta(prev, curr, mode == "compact", false)
				}

				data, err := proto.Marshal(delta)
				if err != nil {
					b.Fatal(err)
				}

				b.ReportMetric(float64(len(data)), "wire-B/op")
			}
		})
	}
}
