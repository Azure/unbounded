// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net"
	"testing"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

type fakeCNIBridgeLinkManager struct {
	exists         bool
	ensureErr      error
	ensurePortsErr error
	ensurePodsErr  error
	ensureMTU      int
	ensurePortsMTU int
	ensurePodsMTU  int
	netnsDir       string
	cache          *unboundednetnetlink.NetlinkCache
}

func TestGatewayLocalForwardingMTUCaps(t *testing.T) {
	cfg := &config{MTU: 1420, TunnelIPFamily: "IPv4"}
	pools := map[string]int{"local": 1200, "remote": 1400, "unrelated": 900}

	localMTU := localGatewayPoolTunnelMTU([]string{"local"}, pools)
	if localMTU != 1200 {
		t.Fatalf("local pool MTU=%d, want 1200", localMTU)
	}

	mesh := []meshPeerInfo{{SiteName: "same"}}
	gateways := []gatewayPeerInfo{{PoolName: "remote", SiteName: "other"}}
	resolvePeerTunnelMTUs(cfg, mesh, gateways, "same", true, localMTU, nil, nil,
		map[string]int{"same": 1420}, nil, nil, nil, pools, nil)

	if mesh[0].TunnelMTU != 1200 || gateways[0].TunnelMTU != 1200 {
		t.Fatalf("local pool cap missing: mesh=%d gateway=%d", mesh[0].TunnelMTU, gateways[0].TunnelMTU)
	}

	if got := resolveCNIConfigMTU(cfg.MTU, resolveTunnelMTU(1420, localMTU), nil, nil, 1500); got != 1200 {
		t.Fatalf("idle gateway fabric MTU=%d, want 1200", got)
	}

	for _, gateway := range []bool{false, true} {
		got := resolveMeshPeerTunnelMTU(1420, meshPeerInfo{SiteName: "same"}, "same",
			map[string]int{"same": 1420}, nil, map[string]int{"same": 1100}, gateway)

		want := 1420
		if gateway {
			want = 1100
		}

		if got != want {
			t.Fatalf("same-Site assignment MTU gateway=%v: got=%d want=%d", gateway, got, want)
		}
	}
}

func TestWorkerGatewayFirstHopPeeringMTU(t *testing.T) {
	for _, tc := range []struct {
		name        string
		site        string
		gateway     bool
		peeringCaps map[string]int
		want        int
	}{
		{name: "worker mesh edge", site: "site-b", peeringCaps: map[string]int{"site-b": 1200}, want: 1200},
		{name: "unrelated peering", site: "site-b", peeringCaps: map[string]int{"site-c": 900}, want: 1250},
		{name: "same Site", site: "site-a", peeringCaps: map[string]int{"site-a": 1200}, want: 1250},
		{name: "pool transit is not mesh", site: "site-b", gateway: true, peeringCaps: map[string]int{"site-b": 1200}, want: 1250},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config{MTU: 1250, TunnelIPFamily: "IPv4"}
			peers := []gatewayPeerInfo{{SiteName: tc.site, PoolName: "pool-g"}}
			resolvePeerTunnelMTUs(cfg, nil, peers, "site-a", tc.gateway, 0, nil, nil,
				nil, tc.peeringCaps, nil, nil, nil, nil)

			if peers[0].TunnelMTU != tc.want {
				t.Fatalf("gateway peer MTU=%d, want %d", peers[0].TunnelMTU, tc.want)
			}

			if got := resolveCNIConfigMTU(cfg.MTU, 0, nil, peers, 1500); got != tc.want {
				t.Fatalf("CNI MTU=%d, want %d", got, tc.want)
			}
		})
	}
}

func (f *fakeCNIBridgeLinkManager) Exists() bool {
	return f.exists
}

func (f *fakeCNIBridgeLinkManager) EnsureMTUWithCache(cache *unboundednetnetlink.NetlinkCache, mtu int) error {
	f.cache = cache
	f.ensureMTU = mtu

	return f.ensureErr
}

func (f *fakeCNIBridgeLinkManager) EnsureBridgePortMTUs(mtu int) error {
	f.ensurePortsMTU = mtu

	return f.ensurePortsErr
}

func (f *fakeCNIBridgeLinkManager) EnsureBridgePodMTUs(netnsDir string, mtu int) error {
	f.netnsDir = netnsDir
	f.ensurePodsMTU = mtu

	return f.ensurePodsErr
}

func TestEnsureCNIBridgeMTU(t *testing.T) {
	oldNewLinkManager := newCNIBridgeLinkManagerFn

	t.Cleanup(func() {
		newCNIBridgeLinkManagerFn = oldNewLinkManager
	})

	cache := &unboundednetnetlink.NetlinkCache{}

	tests := []struct {
		name       string
		bridgeName string
		mtu        int
		exists     bool
		ensureErr  error
		portsErr   error
		podsErr    error
		wantCreate bool
		wantEnsure bool
		wantPorts  bool
		wantPods   bool
		wantErr    bool
		reconcile  bool
	}{
		{name: "zero MTU", bridgeName: "cbr0", mtu: 0},
		{name: "empty bridge name", mtu: 1420},
		{name: "bridge not created yet", bridgeName: "cbr0", mtu: 1420, wantCreate: true},
		{name: "existing bridge", bridgeName: "cbr0", mtu: 1420, exists: true, wantCreate: true, wantEnsure: true, wantPorts: true},
		{name: "existing bridge and pods", bridgeName: "cbr0", mtu: 1420, exists: true, wantCreate: true, wantEnsure: true, wantPorts: true, wantPods: true, reconcile: true},
		{name: "netlink error", bridgeName: "cbr0", mtu: 1420, exists: true, ensureErr: errors.New("set MTU"), wantCreate: true, wantEnsure: true, wantErr: true},
		{name: "bridge port error", bridgeName: "cbr0", mtu: 1420, exists: true, portsErr: errors.New("set port MTU"), wantCreate: true, wantEnsure: true, wantPorts: true, wantErr: true},
		{name: "pod interface error", bridgeName: "cbr0", mtu: 1420, exists: true, podsErr: errors.New("set pod MTU"), wantCreate: true, wantEnsure: true, wantPorts: true, wantPods: true, wantErr: true, reconcile: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &fakeCNIBridgeLinkManager{
				exists:         tt.exists,
				ensureErr:      tt.ensureErr,
				ensurePortsErr: tt.portsErr,
				ensurePodsErr:  tt.podsErr,
			}
			created := false
			newCNIBridgeLinkManagerFn = func(bridgeName string) cniBridgeLinkManager {
				created = true

				if bridgeName != tt.bridgeName {
					t.Fatalf("bridge name = %q, want %q", bridgeName, tt.bridgeName)
				}

				return manager
			}

			err := ensureCNIBridgeMTU(tt.bridgeName, tt.mtu, cache, tt.reconcile)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ensureCNIBridgeMTU() error = %v, want error %t", err, tt.wantErr)
			}

			if created != tt.wantCreate {
				t.Fatalf("link manager created = %t, want %t", created, tt.wantCreate)
			}

			ensured := manager.ensureMTU != 0
			if ensured != tt.wantEnsure {
				t.Fatalf("MTU ensured = %t, want %t", ensured, tt.wantEnsure)
			}

			portsEnsured := manager.ensurePortsMTU != 0
			if portsEnsured != tt.wantPorts {
				t.Fatalf("bridge port MTUs ensured = %t, want %t", portsEnsured, tt.wantPorts)
			}

			podsEnsured := manager.ensurePodsMTU != 0
			if podsEnsured != tt.wantPods {
				t.Fatalf("pod interface MTUs ensured = %t, want %t", podsEnsured, tt.wantPods)
			}

			if tt.wantEnsure {
				if manager.ensureMTU != tt.mtu {
					t.Fatalf("ensured MTU = %d, want %d", manager.ensureMTU, tt.mtu)
				}

				if manager.cache != cache {
					t.Fatal("netlink cache was not passed to link manager")
				}
			}

			if tt.wantPorts && manager.ensurePortsMTU != tt.mtu {
				t.Fatalf("ensured bridge port MTU = %d, want %d", manager.ensurePortsMTU, tt.mtu)
			}

			if tt.wantPods {
				if manager.ensurePodsMTU != tt.mtu {
					t.Fatalf("ensured pod interface MTU = %d, want %d", manager.ensurePodsMTU, tt.mtu)
				}

				if manager.netnsDir != hostProcDir {
					t.Fatalf("proc directory = %q, want %q", manager.netnsDir, hostProcDir)
				}
			}
		})
	}
}

func TestResolveInitialCNIConfigMTU(t *testing.T) {
	if got := resolveInitialCNIConfigMTU(0, 0, 1500); got != 1420 {
		t.Fatalf("auto MTU = %d, want 1420", got)
	}

	if got := resolveInitialCNIConfigMTU(0, 1400, 1500); got != 1400 {
		t.Fatalf("site-limited MTU = %d, want 1400", got)
	}

	if got := resolveInitialCNIConfigMTU(1380, 1400, 1500); got != 1380 {
		t.Fatalf("configured MTU = %d, want 1380", got)
	}

	if got := resolveInitialCNIConfigMTU(0, 0, 0); got != 0 {
		t.Fatalf("unresolved MTU = %d, want 0 for caller fallback", got)
	}
}

func TestResolveCNIConfigMTUUsesLowestPeer(t *testing.T) {
	meshPeers := []meshPeerInfo{{TunnelMTU: 1420}, {TunnelMTU: 1200}}
	gatewayPeers := []gatewayPeerInfo{{TunnelMTU: 1350}}

	if got := resolveCNIConfigMTU(0, 0, meshPeers, gatewayPeers, 1500); got != 1200 {
		t.Fatalf("CNI MTU = %d, want 1200", got)
	}
}

func TestTunnelProtocolMTUOverhead(t *testing.T) {
	tests := map[string]int{
		string(unboundednetv1alpha1.TunnelProtocolWireGuard): 80,
		string(unboundednetv1alpha1.TunnelProtocolGENEVE):    58,
		string(unboundednetv1alpha1.TunnelProtocolVXLAN):     50,
		string(unboundednetv1alpha1.TunnelProtocolIPIP):      20,
		string(unboundednetv1alpha1.TunnelProtocolNone):      0,
	}

	for protocol, want := range tests {
		if got := tunnelProtocolMTUOverhead(protocol); got != want {
			t.Fatalf("%s overhead = %d, want %d", protocol, got, want)
		}
	}
}

func TestRouteTunnelMTUUsesLowestMatchingPeer(t *testing.T) {
	_, prefix, err := net.ParseCIDR("10.244.0.0/16")
	if err != nil {
		t.Fatal(err)
	}

	meshPeers := []meshPeerInfo{
		{PodCIDRs: []string{"10.244.1.0/24"}, TunnelMTU: 1420},
		{PodCIDRs: []string{"10.244.2.0/24"}, TunnelMTU: 1200},
	}

	if got := routeTunnelMTU(prefix, meshPeers, nil); got != 1200 {
		t.Fatalf("route MTU = %d, want 1200", got)
	}
}

func TestManagedTunnelInterfaceDetection(t *testing.T) {
	cfg := &config{
		WireGuardInterfacePrefix: "wg",
		GeneveInterfaceName:      "geneve0",
		VXLANInterfaceName:       "vxlan0",
		IPIPInterfaceName:        "ipip0",
	}

	for _, iface := range []string{"unbounded0", "wg51820", "geneve0", "vxlan0", "ipip0"} {
		if !isManagedTunnelInterface(cfg, iface) {
			t.Fatalf("expected %s to be managed", iface)
		}
	}

	if isManagedTunnelInterface(cfg, "tailscale0") {
		t.Fatal("tailscale0 must be treated as an underlay route")
	}
}
