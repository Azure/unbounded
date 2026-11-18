// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"google.golang.org/protobuf/proto"

	"github.com/Azure/unbounded/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func TestSummaryPeerHealthy(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		health    *HealthCheckPeerStatus
		handshake time.Time
		want      bool
	}{
		{"up", &HealthCheckPeerStatus{Enabled: true, Status: "up"}, time.Time{}, true},
		{"Up", &HealthCheckPeerStatus{Enabled: true, Status: "Up"}, time.Time{}, true},
		{"uppercase is not counted", &HealthCheckPeerStatus{Enabled: true, Status: "UP"}, now, false},
		{"enabled unknown", &HealthCheckPeerStatus{Enabled: true}, now, false},
		{"enabled down", &HealthCheckPeerStatus{Enabled: true, Status: "down"}, now, false},
		{"disabled uses handshake", &HealthCheckPeerStatus{Status: "down"}, now, true},
		{"missing health uses handshake", nil, now.Add(-time.Minute), true},
		{"missing handshake", nil, time.Time{}, false},
		{"boundary", nil, now.Add(-3 * time.Minute), false},
		{"future handshake", nil, now.Add(time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := WireGuardPeerStatus{HealthCheck: tc.health, Tunnel: PeerTunnelStatus{LastHandshake: tc.handshake}}
			if got := netstatus.PeerHealthyForOverview(&peer, now); got != tc.want {
				t.Fatalf("healthy=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestRelationshipPeerDiagnostics(t *testing.T) {
	for _, protocol := range []string{"WireGuard", "GENEVE", "VXLAN", "IPIP", "None"} {
		t.Run(protocol, func(t *testing.T) {
			server := summaryRouteFixture()
			server.cfg.GeneveInterfaceName = "geneve0"
			server.cfg.VXLANInterfaceName = "vxlan0"
			server.cfg.IPIPInterfaceName = "ipip0"

			manager, err := healthcheck.NewManager("local", 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Stop()

			workerKey, otherKey, gatewayKey := wgtypes.Key{1}, wgtypes.Key{2}, wgtypes.Key{3}
			server.state.healthCheckManager = manager
			server.state.meshPeerHealthCheckEnabled = map[string]bool{
				"worker/assignment/pool": true, "worker/assignment/other-pool": true, "other/assignment/pool": true,
			}
			server.state.peers = []meshPeerInfo{
				{Name: "worker", SiteName: "remote", TunnelProtocol: protocol, WireGuardPublicKey: workerKey.String(), InternalIPs: []string{"192.0.2.2"}, PodCIDRs: []string{"10.42.1.0/24"}},
				{Name: "worker/assignment/pool", SiteName: "remote", TransitPoolName: "pool", TransitWireguardPort: 51830, TunnelProtocol: protocol, WireGuardPublicKey: workerKey.String(), InternalIPs: []string{"192.0.2.2"}, PodCIDRs: []string{"10.42.1.0/24"}},
				{Name: "worker/assignment/other-pool", SiteName: "remote", TransitPoolName: "other-pool", TransitWireguardPort: 51830, TunnelProtocol: protocol, WireGuardPublicKey: workerKey.String(), InternalIPs: []string{"192.0.2.2"}, PodCIDRs: []string{"10.42.1.0/24"}},
				{Name: "other/assignment/pool", SiteName: "remote", TransitPoolName: "pool", TransitWireguardPort: 51830, TunnelProtocol: protocol, WireGuardPublicKey: otherKey.String(), InternalIPs: []string{"192.0.2.3"}, PodCIDRs: []string{"10.42.2.0/24"}},
			}
			server.state.gatewayPeers = []gatewayPeerInfo{{
				Name: "gateway/transit/pool", SiteName: "hub", TunnelProtocol: protocol,
				InternalIPs: []string{"192.0.2.4"}, PodCIDRs: []string{"10.42.3.0/24"},
			}}

			for _, name := range []string{"worker/assignment/pool", "worker/assignment/other-pool", "other/assignment/pool", "gateway/transit/pool"} {
				hostname, _, _ := strings.Cut(name, "/")

				octet := map[string]int{"worker": 1, "other": 2, "gateway": 3}[hostname]
				if err := manager.AddScopedPeer(name, hostname, net.ParseIP(fmt.Sprintf("10.42.%d.1", octet)), healthcheck.DefaultSettings()); err != nil {
					t.Fatal(err)
				}
			}

			ifaceName := tunnelInterfaceName(server.cfg, protocol)
			if protocol == "None" {
				if name, _, err := unboundednetnetlink.DetectDefaultRouteInterfaceFromCache(server.state.netlinkCache); err == nil {
					ifaceName = name
				}
			}

			mesh, assignment, gateway := &unboundednetnetlink.WireGuardManager{}, &unboundednetnetlink.WireGuardManager{}, &unboundednetnetlink.WireGuardManager{}
			if protocol == "WireGuard" {
				server.state.wireguardManager = mesh
				server.state.gatewayWireguardManagers = map[string]*unboundednetnetlink.WireGuardManager{"wg51830": assignment, "wg51840": gateway}
				server.state.gatewayNames = map[string]string{"wg51840": "gateway/transit/pool"}
				server.state.gatewaySiteNames = map[string]string{"wg51840": "hub"}
				server.state.gatewayPodCIDRs = map[string][]string{"wg51840": {"10.42.3.0/24"}}
				server.state.gatewayPeerHealthCheckEnabled = map[string]bool{"wg51840": true}
			} else {
				server.state.gatewayPeerHealthCheckEnabled = map[string]bool{ifaceName: true}
			}

			handshake := testStatusTime
			server.wireGuardDevice = func(current *unboundednetnetlink.WireGuardManager) (*wgtypes.Device, error) {
				var peers []wgtypes.Peer

				switch current {
				case mesh:
					peers = []wgtypes.Peer{{PublicKey: workerKey, ReceiveBytes: 101}}
				case assignment:
					peers = []wgtypes.Peer{
						{PublicKey: otherKey, ReceiveBytes: 202},
						{PublicKey: wgtypes.Key{9}, ReceiveBytes: 999},
						{
							PublicKey: workerKey, ReceiveBytes: 303, TransmitBytes: 404, LastHandshakeTime: handshake,
							Endpoint:   &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 51830},
							AllowedIPs: []net.IPNet{{IP: net.ParseIP("10.42.1.0"), Mask: net.CIDRMask(24, 32)}},
						},
					}
				case gateway:
					peers = []wgtypes.Peer{{PublicKey: gatewayKey, ReceiveBytes: 505}}
				default:
					t.Fatal("queried an unexpected WireGuard device")
				}

				return &wgtypes.Device{Peers: peers}, nil
			}

			var peers []WireGuardPeerStatus

			facts := server.inspectNodePeers(func(peer WireGuardPeerStatus) { peers = append(peers, peer) })
			if len(peers) != 5 || facts.HealthCheck == nil || facts.HealthCheck.PeerCount != 4 {
				t.Fatalf("relationships missing from diagnostics: peers=%+v, health=%+v", peers, facts.HealthCheck)
			}

			if _, err := netstatus.PeerIdentityDigest(peers); err != nil {
				t.Fatalf("relationship metadata is ambiguous for status deltas: %v", err)
			}

			ordinary, assigned, other, transit := 0, 0, 0, 0

			for _, peer := range peers {
				if strings.Contains(peer.Name, "/") {
					t.Fatalf("relationship identifier exposed as node name: %q", peer.Name)
				}

				if peer.Tunnel.Protocol != protocol {
					t.Fatalf("protocol lost: %+v", peer)
				}

				switch peer.Name {
				case "worker":
					if peer.HealthCheck == nil {
						ordinary++

						if protocol == "WireGuard" && (peer.Tunnel.Interface != "wg51820" || peer.Tunnel.RxBytes != 101) {
							t.Fatalf("assignment replaced ordinary mesh diagnostics: %+v", peer)
						}
					} else {
						assigned++

						if protocol == "WireGuard" && (peer.Tunnel.Interface != "wg51830" || peer.Tunnel.RxBytes != 303 || peer.Tunnel.TxBytes != 404 ||
							peer.Tunnel.LastHandshake != handshake || peer.Tunnel.Endpoint != "192.0.2.2:51830" || !reflect.DeepEqual(peer.Tunnel.AllowedIPs, []string{"10.42.1.0/24"})) {
							t.Fatalf("assignment metrics came from the wrong link or key: %+v", peer)
						}
					}
				case "other":
					other++
				case "gateway":
					transit++

					if peer.SiteName != "hub" || peer.PeerType != "gateway" {
						t.Fatalf("gateway metadata lost: %+v", peer)
					}
				default:
					t.Fatalf("unknown WireGuard peer leaked into assignment diagnostics: %+v", peer)
				}

				if peer.Name != "worker" || peer.HealthCheck != nil {
					if peer.HealthCheck == nil || !peer.HealthCheck.Enabled || peer.HealthCheck.Uptime == "" {
						t.Fatalf("scoped health lookup failed for public node identity: %+v", peer)
					}
				}
			}

			if ordinary != 1 || assigned != 2 || other != 1 || transit != 1 {
				t.Fatalf("ordinary and relationship entries were merged: %+v", peers)
			}
		})
	}
}

func TestStatusDuringGatewayManagerPublication(t *testing.T) {
	server := summaryRouteFixture()
	server.state.peers = []meshPeerInfo{{
		Name: "worker/assignment/pool", SiteName: "remote", TransitPoolName: "pool",
		TransitWireguardPort: 51830, TunnelProtocol: "WireGuard", WireGuardPublicKey: wgtypes.Key{1}.String(),
	}}
	server.wireGuardDevice = func(*unboundednetnetlink.WireGuardManager) (*wgtypes.Device, error) {
		return &wgtypes.Device{}, nil
	}

	var writers sync.WaitGroup
	writers.Go(func() {
		for range 1000 {
			server.state.storeGatewayWireGuardManager("wg51830", &unboundednetnetlink.WireGuardManager{})
			server.state.removeGatewayInterfaceState("wg51830")
		}
	})

	for range 1000 {
		server.inspectNodePeers(func(WireGuardPeerStatus) {})
	}

	writers.Wait()
}

func TestNodeSummaryParityAndNoBPF(t *testing.T) {
	for _, deviceFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "device success", true: "device failure"}[deviceFailure], func(t *testing.T) {
			s := summaryRouteFixture()
			s.state.siteName = "local"
			s.state.nodePodCIDRs = []string{"10.42.0.0/24"}
			s.state.nodeInternalIPs = []string{"192.0.2.1"}
			s.state.nodeExternalIPs = []string{"198.51.100.1"}
			s.state.nodeErrors = []NodeError{{Type: "test", Message: "failure"}}
			s.state.wireguardManager = &unboundednetnetlink.WireGuardManager{}

			manager, err := healthcheck.NewManager("local", 0, nil)
			if err != nil {
				t.Fatal(err)
			}

			if err := manager.AddPeer("down", net.ParseIP("10.42.1.1"), healthcheck.DefaultSettings()); err != nil {
				t.Fatal(err)
			}

			s.state.healthCheckManager = manager
			s.state.meshPeerHealthCheckEnabled = map[string]bool{"down": true, "missing": true}
			s.state.peers = []meshPeerInfo{
				{Name: "down", WireGuardPublicKey: "down", TunnelProtocol: "GENEVE", InternalIPs: []string{"192.0.2.2"}, PodCIDRs: []string{"10.42.1.0/24"}},
				{Name: "missing", WireGuardPublicKey: "missing", TunnelProtocol: "VXLAN", InternalIPs: []string{"192.0.2.3"}, PodCIDRs: []string{"10.42.2.0/24"}},
				{Name: "no-address", TunnelProtocol: "IPIP"},
			}
			s.state.gatewayPeers = []gatewayPeerInfo{
				{Name: "gateway", TunnelProtocol: "IPIP", InternalIPs: []string{"192.0.2.4"}, PodCIDRs: []string{"10.42.3.0/24"}},
			}
			s.state.gatewayHealthEndpoints = map[string]string{"wg51821": "10.42.3.1"}
			s.state.gatewayNames = map[string]string{"wg51821": "gateway"}
			s.state.gatewayWireguardManagers = map[string]*unboundednetnetlink.WireGuardManager{"wg51821": {}}
			s.state.linkStatsMonitor = &linkStatsMonitor{warnings: []string{
				"interface /wg51820: rx_errors +24", "interface /gn0: rx_errors +24", "interface /eth0: tx_errors +2",
			}}
			s.wireGuardDevice = func(*unboundednetnetlink.WireGuardManager) (*wgtypes.Device, error) {
				if deviceFailure {
					return nil, errors.New("device unavailable")
				}

				return &wgtypes.Device{ListenPort: 51820, Peers: []wgtypes.Peer{
					{LastHandshakeTime: time.Now().Add(-time.Minute)},
				}}, nil
			}
			s.netlinkOps.(*fakeNetlinkOps).mainRoutes = map[int][]netlink.Route{
				netlink.FAMILY_V4: {summaryRoute("10.42.0.0/16", 2, 0, 0)},
			}
			bpfCalls := 0
			s.bpfCollector = func() []BpfEntry { bpfCalls++; return nil }

			summary := s.getNodeSummary()
			if bpfCalls != 0 || !s.routingTableCachedAt.IsZero() || len(s.routingTableCache.Routes) != 0 {
				t.Fatal("summary collected BPF or populated the full route cache")
			}

			full := s.getNodeStatus()

			if bpfCalls != 1 {
				t.Fatal("legacy full collection no longer collects BPF")
			}

			if !reflect.DeepEqual(summary.NodeInfo, full.NodeInfo) || !reflect.DeepEqual(summary.NodeErrors, full.NodeErrors) {
				t.Fatalf("metadata/errors differ: summary=%+v full=%+v", summary, full)
			}

			legacy := netstatus.OverviewFromStatus(full, time.Now())
			if summary.PeerCount != legacy.PeerCount || summary.HealthyPeers != legacy.HealthyPeers ||
				summary.RouteCount != legacy.RouteCount || summary.RouteMismatch != legacy.RouteMismatch ||
				summary.RouteMismatchCount != legacy.RouteMismatchCount || summary.UnhealthyPeerLinks != legacy.UnhealthyPeerLinks ||
				summary.UsesIPIP != legacy.UsesIPIP {
				t.Fatalf("counts differ: summary=%+v full peers=%+v routes=%+v", summary, full.Peers, full.RoutingTable)
			}

			wantPeers, wantHealthyPeers := 4, 2
			if deviceFailure {
				wantPeers, wantHealthyPeers = 3, 0
			}

			if summary.PeerCount != wantPeers || summary.HealthyPeers != wantHealthyPeers || summary.RouteMismatch {
				t.Fatalf("unexpected observed counts/mismatch: %+v", summary)
			}

			if summary.HealthCheck == nil || summary.HealthCheck.Healthy != full.HealthCheck.Healthy ||
				summary.HealthCheck.PeerCount != full.HealthCheck.PeerCount || summary.HealthCheck.Summary != full.HealthCheck.Summary {
				t.Fatalf("health aggregates differ: %v vs %v", summary.HealthCheck, full.HealthCheck)
			}

			assertSummaryHasNoDetails(t, summary)
		})
	}
}

func assertSummaryHasNoDetails(t *testing.T, summary *NodeStatusOverview) {
	t.Helper()

	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"peers", "routingTable", "bpfEntries", "peerMeasurements"} {
		if _, exists := fields[name]; exists {
			t.Fatalf("summary contains detail field %q", name)
		}
	}
}

func TestSummaryBootstrapRecoveryAndLocalRouting(t *testing.T) {
	h := blockedBootstrapHealthState()
	h.transientErrors = []NodeError{{Type: "transport", Message: "failed"}, {Type: "expired", Message: "old", Timestamp: time.Now().Add(-2 * time.Minute)}}
	mux := newHealthMux(h)

	for _, path := range []string{"/status/summary", "/status", "/status/json"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: code=%d headers=%v", path, recorder.Code, recorder.Header())
		}

		var summary NodeStatusOverview
		if err := json.Unmarshal(recorder.Body.Bytes(), &summary); err != nil {
			t.Fatal(err)
		}

		if summary.NodeInfo.Name != "node-a" || len(summary.NodeErrors) != 2 || summary.NodeErrors[1].Type != configPodCIDRGuard {
			t.Fatalf("%s: lost bootstrap identity/errors: %+v", path, summary)
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &fields); err != nil {
			t.Fatal(err)
		}

		_, hasPeers := fields["peers"]
		if hasPeers == (path == "/status/summary") {
			t.Fatalf("%s: endpoint returned wrong representation", path)
		}
	}

	h.setCNIReady("cbr0", []string{"10.244.7.0/24"})

	summary := h.getSummarySnapshot()
	if len(summary.NodeErrors) != 1 || summary.NodeErrors[0].Type != "transport" {
		t.Fatalf("guard did not recover: %+v", summary.NodeErrors)
	}

	summary.NodeInfo.PodCIDRs[0] = "mutated"
	if h.getSummarySnapshot().NodeInfo.PodCIDRs[0] == "mutated" {
		t.Fatal("bootstrap CIDRs alias shared state")
	}

	s := summaryRouteFixture()
	s.state.nodeErrors = []NodeError{{Type: configPodCIDRGuard, Message: "obsolete guard"}}
	h.setStatusServer(s)

	if got := h.getSummarySnapshot(); got.NodeInfo.Name != "local" || len(got.NodeErrors) != 1 || got.NodeErrors[0].Type != "transport" {
		t.Fatalf("initialized summary lost recovery/transport state: %+v", got)
	}
}

func TestSummaryHealthyAggregate(t *testing.T) {
	s := summaryRouteFixture()

	manager, err := healthcheck.NewManager("local", 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	s.state.healthCheckManager = manager

	summary := s.getNodeSummary()
	if summary.HealthCheck == nil || !summary.HealthCheck.Healthy || summary.HealthCheck.PeerCount != 0 ||
		summary.HealthCheck.Summary != "all peers healthy" || summary.HealthCheck.CheckedAt.IsZero() {
		t.Fatalf("lost healthy aggregate: %+v", summary.HealthCheck)
	}
}

func TestSummaryConcurrentBootstrapState(t *testing.T) {
	h := blockedBootstrapHealthState()

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 20 {
				h.beginManagedCNI("cbr0")
				h.getSummarySnapshot()
				h.setCNIReady("cbr0", []string{"10.244.7.0/24"})
			}
		})
	}

	wg.Wait()
}

func TestNodeSummaryToProto(t *testing.T) {
	if nodeSummaryToProto(nil) != nil {
		t.Fatal("nil summary converted")
	}

	now := time.Now()
	summary := &NodeStatusOverview{
		Timestamp: now, NodeInfo: NodeInfo{Name: "node", K8sReady: "Unknown"},
		PeerCount: 10, HealthyPeers: 4, RouteCount: 12, RouteMismatch: true,
		RouteMismatchCount: 3, UnhealthyPeerLinks: 2, UsesIPIP: true,
		FetchError: "unavailable", StatusSource: "error", LastPushTime: &now,
		NodeErrors:  []NodeError{{Type: "failure", Message: "failed"}},
		HealthCheck: &HealthCheckStatus{Healthy: false, Summary: "unhealthy"},
	}
	encoded := nodeSummaryToProto(summary)

	data, err := proto.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}

	var decoded statusproto.NodeStatusOverview
	if err := proto.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if !proto.Equal(encoded, &decoded) || decoded.PeerCount != 10 || decoded.HealthyPeers != 4 || decoded.RouteCount != 12 ||
		decoded.RouteMismatchCount != 3 || decoded.UnhealthyPeerLinks != 2 || !decoded.UsesIpip ||
		!decoded.RouteMismatch || decoded.FetchError != "unavailable" || decoded.NodeInfo.K8SReady != "Unknown" ||
		decoded.LastPushTimeUnixNs != now.UnixNano() || decoded.StatusSource != "error" || len(decoded.NodeErrors) != 1 {
		t.Fatalf("summary conversion lost facts: %v", &decoded)
	}
}

func BenchmarkNodeSummaryCollection(b *testing.B) {
	for _, full := range []bool{false, true} {
		b.Run(map[bool]string{false: "summary", true: "full"}[full], func(b *testing.B) {
			s := summaryRouteFixture()
			s.bpfCollector = func() []BpfEntry { return nil }

			s.netlinkOps.(*fakeNetlinkOps).mainRoutes = map[int][]netlink.Route{
				netlink.FAMILY_V4: {summaryRoute("10.0.0.0/8", 2, 0, 0)},
			}
			for i := range 2000 {
				s.state.peers = append(s.state.peers, meshPeerInfo{
					Name: fmt.Sprintf("peer-%d", i), WireGuardPublicKey: fmt.Sprintf("key-%d", i),
					TunnelProtocol: "GENEVE", InternalIPs: []string{"192.0.2.2"},
					PodCIDRs: []string{fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)},
				})
			}

			b.ReportAllocs()

			for b.Loop() {
				if full {
					s.getNodeStatus()
				} else {
					s.getNodeSummary()
				}
			}
		})
	}
}
