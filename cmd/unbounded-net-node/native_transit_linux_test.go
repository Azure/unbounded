//go:build linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net"
	"runtime"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestNativeTransitInterfaceRules(t *testing.T) {
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.Close() }()

	isolated, err := netns.New()
	if errors.Is(err, unix.EPERM) {
		t.Skip("requires network namespace privileges")
	}

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = isolated.Close() }()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatal(err)
		}
	}()

	fabric := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "fabric0"}}
	if err := netlink.LinkAdd(fabric); err != nil {
		t.Fatal(err)
	}

	if err := netlink.LinkSetUp(fabric); err != nil {
		t.Fatal(err)
	}

	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: fabric.Index,
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
	}); err != nil {
		t.Fatal(err)
	}

	forward, err := unboundednetnetlink.NewForwardManager()
	if err != nil {
		t.Fatal(err)
	}

	forward.EnsureInterface("fabric0")
	forward.EnsureInterface("oldfabric0")
	forward.EnsureInterface("tunnel0")

	forward, err = unboundednetnetlink.NewForwardManager()
	if err != nil {
		t.Fatal(err)
	}
	defer forward.Cleanup()

	forward.EnsureInterface("tunnel0")

	if err := forward.ReconcileNativeInterface("fabric0", []string{"invalid"}); err == nil {
		t.Fatal("invalid native prefix accepted")
	}

	notrack, err := unboundednetnetlink.NewNotrackManager()
	if err != nil {
		t.Fatal(err)
	}
	defer notrack.Cleanup()

	if err := notrack.ReconcileCIDRs(
		[]string{"10.246.1.0/24", "fd46:0:0:1::/64"},
		[]string{"192.0.2.0/24", "2001:db8::/64"},
		[]string{"10.246.0.0/16", "fd46::/48"},
	); err != nil {
		t.Fatal(err)
	}

	checkRule := func(ipt *iptables.IPTables, want bool, table, chain string, rule ...string) {
		t.Helper()

		got, err := ipt.Exists(table, chain, rule...)
		if err != nil || got != want {
			t.Fatalf("%s/%s %v: exists=%t, want=%t, error=%v", table, chain, rule, got, want, err)
		}
	}

	for _, tc := range []struct {
		name       string
		mesh       []meshPeerInfo
		assignment []gatewayPeerInfo
		gateway    bool
		forward    bool
		notrack    bool
	}{
		{"native assignment gateway", nil, []gatewayPeerInfo{{TunnelProtocol: "None", RoutedCidrs: []string{"10.246.1.0/24", "fd46:0:0:1::/64"}}}, true, true, true},
		{"gateway demotion", nil, []gatewayPeerInfo{{TunnelProtocol: "None", RoutedCidrs: []string{"10.246.1.0/24", "fd46:0:0:1::/64"}}}, false, true, false},
		{"native mesh gateway", []meshPeerInfo{{TunnelProtocol: "None", PodCIDRs: []string{"10.246.1.0/24", "fd46:0:0:1::/64"}}}, nil, true, true, true},
		{"native withdrawal", nil, nil, true, false, false},
	} {
		if err := reapplyRPFilterOnActiveTunnels(&config{}, tc.mesh, tc.assignment, nil, forward, tc.gateway, notrack, nil); err != nil {
			t.Fatal(err)
		}

		for _, protocol := range []iptables.Protocol{iptables.ProtocolIPv4, iptables.ProtocolIPv6} {
			ipt, err := iptables.NewWithProtocol(protocol)
			if err != nil {
				t.Fatal(err)
			}

			t.Log(tc.name, protocol)
			checkRule(ipt, false, "filter", "FORWARD",
				"-i", "fabric0", "-m", "comment", "--comment", "unbounded-net: forward between managed tunnels", "-j", "UNBOUNDED-FORWARD")
			checkRule(ipt, false, "filter", "UNBOUNDED-FORWARD",
				"-o", "fabric0", "-m", "comment", "--comment", "unbounded-net: forward between managed tunnels", "-j", "ACCEPT")
			checkRule(ipt, false, "filter", "FORWARD",
				"-i", "oldfabric0", "-m", "comment", "--comment", "unbounded-net: forward between managed tunnels", "-j", "UNBOUNDED-FORWARD")
			checkRule(ipt, false, "filter", "UNBOUNDED-FORWARD",
				"-o", "oldfabric0", "-m", "comment", "--comment", "unbounded-net: forward between managed tunnels", "-j", "ACCEPT")
			checkRule(ipt, true, "filter", "FORWARD",
				"-i", "tunnel0", "-m", "comment", "--comment", "unbounded-net: forward between managed tunnels", "-j", "UNBOUNDED-FORWARD")
			checkRule(ipt, true, "filter", "UNBOUNDED-FORWARD",
				"-o", "tunnel0", "-m", "comment", "--comment", "unbounded-net: forward between managed tunnels", "-j", "ACCEPT")
			checkRule(ipt, tc.notrack, "raw", "PREROUTING",
				"-i", "fabric0", "-m", "comment", "--comment", "unbounded-net: skip conntrack for tunnel transit", "-j", "UNBOUNDED-NOTRACK")
			checkRule(ipt, true, "raw", "UNBOUNDED-NOTRACK",
				"-m", "addrtype", "--dst-type", "LOCAL", "-m", "comment", "--comment", "unbounded-net: skip conntrack for tunnel transit", "-j", "RETURN")

			pod, node, supernet := "10.246.1.0/24", "192.0.2.0/24", "10.246.0.0/16"
			if protocol == iptables.ProtocolIPv6 {
				pod, node, supernet = "fd46:0:0:1::/64", "2001:db8::/64", "fd46::/48"
			}

			checkRule(ipt, tc.forward, "filter", "FORWARD", "-i", "fabric0", "-s", pod,
				"-m", "comment", "--comment", "unbounded-net: forward native cluster traffic", "-j", "UNBOUNDED-FORWARD")
			checkRule(ipt, tc.forward, "filter", "UNBOUNDED-FORWARD", "-o", "fabric0", "-d", pod,
				"-m", "comment", "--comment", "unbounded-net: forward native cluster traffic", "-j", "ACCEPT")

			for _, cidr := range []string{pod, node} {
				checkRule(ipt, true, "raw", "UNBOUNDED-NOTRACK", "-d", cidr,
					"-m", "comment", "--comment", "unbounded-net: skip conntrack for tunnel transit", "-j", "RETURN")
			}

			checkRule(ipt, true, "raw", "UNBOUNDED-NOTRACK", "-d", supernet,
				"-m", "comment", "--comment", "unbounded-net: skip conntrack for tunnel transit", "-j", "CT", "--notrack")
		}
	}

	checkAssignmentListenerReuse(t)
}

func checkAssignmentListenerReuse(t *testing.T) {
	t.Helper()
	t.Log("reusing a remote gateway interface as an assignment listener")

	privateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	workerKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	const iface = "wg51830"

	server := summaryRouteFixture()
	server.state.gatewayLinkManagers = make(map[string]*unboundednetnetlink.LinkManager)
	server.state.gatewayNames = map[string]string{iface: "old-gateway/transit/old"}
	server.state.gatewaySiteNames = map[string]string{iface: "old"}
	server.state.gatewayHealthEndpoints = map[string]string{iface: "10.245.0.1"}
	server.state.gatewayRoutes = map[string][]string{iface: {"10.245.0.0/16"}}
	server.state.gatewayRouteDistances = map[string]map[string]int{iface: {"10.245.0.0/16": 2}}
	server.state.gatewaySiteCIDRs = map[string][]string{iface: {"10.245.0.0/16"}}
	server.state.gatewayPodCIDRs = map[string][]string{iface: {"10.245.0.0/24"}}
	server.state.gatewayPeerHealthCheckEnabled = map[string]bool{iface: true}
	server.state.peers = []meshPeerInfo{{
		Name: "worker/assignment/pool", SiteName: "remote", TransitPoolName: "pool",
		TransitWireguardPort: 51830, TunnelProtocol: "WireGuard", WireGuardPublicKey: workerKey.PublicKey().String(),
		PodCIDRs: []string{"10.246.1.0/24"},
	}}

	if err := configureAssignmentWireGuard(server.cfg, server.state, iface, 51830, privateKey.String(),
		[]unboundednetnetlink.WireGuardPeer{{PublicKey: workerKey.PublicKey().String(), AllowedIPs: []string{"10.246.1.0/24"}}},
		[]string{"10.246.0.1/32"}, 1400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.state.gatewayWireGuardManager(iface).Close() }()

	var peers []WireGuardPeerStatus

	server.inspectNodePeers(func(peer WireGuardPeerStatus) { peers = append(peers, peer) })

	if len(peers) != 1 || peers[0].Name != "worker" || peers[0].PeerID != "worker/assignment/pool" ||
		peers[0].Tunnel.Interface != iface {
		t.Fatalf("reused listener retained a remote gateway or lost assignment diagnostics: %+v", peers)
	}

	if len(server.state.gatewayNames)+len(server.state.gatewaySiteNames)+len(server.state.gatewayHealthEndpoints)+
		len(server.state.gatewayRoutes)+len(server.state.gatewayRouteDistances)+len(server.state.gatewaySiteCIDRs)+
		len(server.state.gatewayPodCIDRs)+len(server.state.gatewayPeerHealthCheckEnabled) != 0 {
		t.Fatal("assignment listener retained remote gateway metadata")
	}
}

func TestAssignmentReturnRoutesAfterRestart(t *testing.T) {
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.Close() }()

	isolated, err := netns.New()
	if errors.Is(err, unix.EPERM) {
		t.Skip("requires network namespace privileges")
	}

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = isolated.Close() }()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatal(err)
		}
	}()

	owned := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: assignmentReturnDeviceName}}

	foreign := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "other0"}}
	for _, link := range []netlink.Link{owned, foreign} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}

		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}

	table := assignmentReturnTableBase + 1
	for _, fixture := range []struct {
		cidr, device    string
		protocol, table int
	}{
		{"10.246.1.0/24", owned.Name, unix.RTPROT_STATIC, table},
		{"fd46:1::/64", owned.Name, unix.RTPROT_STATIC, table},
		{"10.246.2.0/24", owned.Name, unix.RTPROT_STATIC, table},
		{"fd46:2::/64", owned.Name, unix.RTPROT_STATIC, table},
		{"10.247.0.0/24", owned.Name, unix.RTPROT_BOOT, table},
		{"10.248.0.0/24", foreign.Name, unix.RTPROT_STATIC, table},
		{"10.249.0.0/24", owned.Name, unix.RTPROT_STATIC, table + 1},
	} {
		link, err := netlink.LinkByName(fixture.device)
		if err != nil {
			t.Fatal(err)
		}

		_, prefix, _ := net.ParseCIDR(fixture.cidr)
		if err := netlink.RouteReplace(&netlink.Route{
			LinkIndex: link.Attrs().Index, Dst: prefix, Table: fixture.table,
			Protocol: netlink.RouteProtocol(fixture.protocol), Scope: netlink.SCOPE_LINK,
		}); err != nil {
			t.Fatal(err)
		}
	}

	state := &wireGuardState{}

	routes, err := prepareAssignmentReturnRoutes(&config{TunnelDataplaneMapSize: 64}, state, []string{"10.246.1.0/24", "fd46:1::/64"}, 1400)
	if state.assignmentTunnelMap != nil {
		defer func() { _ = state.assignmentTunnelMap.Close() }()
	}

	if err != nil || len(routes) != 2 {
		t.Fatalf("restart preparation failed: %v, %+v", err, routes)
	}

	kernelRoutes, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}

	present := make(map[string]bool)

	for _, route := range kernelRoutes {
		if route.Dst != nil {
			present[route.Dst.String()] = true
		}
	}

	if len(present) != 4 || !present["10.246.1.0/24"] || !present["fd46:1::/64"] ||
		!present["10.247.0.0/24"] || !present["10.248.0.0/24"] {
		t.Fatalf("restart retained withdrawn routes or removed unrelated routes: %+v", kernelRoutes)
	}

	otherTable, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table + 1}, netlink.RT_FILTER_TABLE)
	if err != nil || len(otherTable) != 1 {
		t.Fatalf("restart changed an unrelated routing table: %v, %+v", err, otherTable)
	}
}
