// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

// driftFakeWireGuardClient is an in-memory WireGuard device that applies the
// peer endpoint, allowed IPs and keepalive written by WireGuardManager.
type driftFakeWireGuardClient struct {
	device    wgtypes.Device
	deviceErr error
	writes    int
}

func (f *driftFakeWireGuardClient) Device(string) (*wgtypes.Device, error) {
	if f.deviceErr != nil {
		return nil, f.deviceErr
	}

	device := f.device
	device.Peers = slices.Clone(f.device.Peers)

	return &device, nil
}

func (f *driftFakeWireGuardClient) ConfigureDevice(_ string, cfg wgtypes.Config) error {
	f.writes++

	if cfg.PrivateKey != nil {
		f.device.PrivateKey = *cfg.PrivateKey
	}

	if cfg.ListenPort != nil {
		f.device.ListenPort = *cfg.ListenPort
	}

	for _, pc := range cfg.Peers {
		idx := slices.IndexFunc(f.device.Peers, func(p wgtypes.Peer) bool { return p.PublicKey == pc.PublicKey })
		if idx < 0 {
			f.device.Peers = append(f.device.Peers, wgtypes.Peer{PublicKey: pc.PublicKey})
			idx = len(f.device.Peers) - 1
		}

		peer := &f.device.Peers[idx]
		if pc.Endpoint != nil {
			peer.Endpoint = pc.Endpoint
		}

		peer.AllowedIPs = slices.Clone(pc.AllowedIPs)
		if pc.PersistentKeepaliveInterval != nil {
			peer.PersistentKeepaliveInterval = *pc.PersistentKeepaliveInterval
		}
	}

	return nil
}

func (f *driftFakeWireGuardClient) Close() error { return nil }

func (f *driftFakeWireGuardClient) endpoint(t *testing.T, key string) string {
	t.Helper()

	for _, p := range f.device.Peers {
		if p.PublicKey.String() == key {
			if p.Endpoint == nil {
				return ""
			}

			return p.Endpoint.String()
		}
	}

	t.Fatalf("peer %s not found", key)

	return ""
}

func (f *driftFakeWireGuardClient) setEndpoint(t *testing.T, key, endpoint string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", endpoint)
	if err != nil {
		t.Fatal(err)
	}

	for i := range f.device.Peers {
		if f.device.Peers[i].PublicKey.String() == key {
			f.device.Peers[i].Endpoint = addr
			return
		}
	}

	t.Fatalf("peer %s not found", key)
}

func driftTestKey(t *testing.T) wgtypes.Key {
	t.Helper()

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}

	return key
}

// newDriftTestManager returns a manager for iface, configured with peers
// through a fake client.
func newDriftTestManager(t *testing.T, iface string, port int, peers ...unboundednetnetlink.WireGuardPeer) (*unboundednetnetlink.WireGuardManager, *driftFakeWireGuardClient) {
	t.Helper()

	client := &driftFakeWireGuardClient{}
	manager := unboundednetnetlink.NewWireGuardManagerWithClient(iface, client)

	if err := manager.Configure(driftTestKey(t).String(), port, peers); err != nil {
		t.Fatal(err)
	}

	client.writes = 0

	return manager, client
}

// TestUnchangedReconcileRepairsRoamedGatewayEndpoint reproduces the incident:
// a worker's per-gateway interface has its gateway endpoint roamed from the
// transit port to the gateway's old mesh port while the desired state stays the
// same. The unchanged heartbeat must restore it without a full reconfigure and
// must leave the learned endpoint of a peer configured without one alone.
func TestUnchangedReconcileRepairsRoamedGatewayEndpoint(t *testing.T) {
	self := &unboundednetv1alpha1.GatewayPoolNode{ObjectMeta: metav1.ObjectMeta{Name: "self"}}
	client := fakeGatewayDynamic(toUnstructured(t, self))
	gatewayNodes := newInformerWithObjects(toUnstructured(t, self))
	sites := newInformerWithObjects(toUnstructured(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "local"}}))
	pools := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{Type: "Internal"},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
			Name: "self", SiteName: "local", WireGuardPublicKey: "pub-self", GatewayWireguardPort: 51821,
		}}},
	}))
	empty := newInformerWithObjects()

	gatewayKey := driftTestKey(t).PublicKey().String()
	natPeerKey := driftTestKey(t).PublicKey().String()
	gwManager, gwClient := newDriftTestManager(t, "wg51821", 51821, unboundednetnetlink.WireGuardPeer{
		PublicKey: gatewayKey, Endpoint: "1.2.3.4:51821", AllowedIPs: []string{"10.0.0.1"}, PersistentKeepalive: 25,
	})
	meshManager, meshClient := newDriftTestManager(t, "wg51820", 51820, unboundednetnetlink.WireGuardPeer{
		PublicKey: natPeerKey, AllowedIPs: []string{"10.0.0.2"}, PersistentKeepalive: 25,
	})

	state := &wireGuardState{
		nodeName: "self", clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
		cniConfigWritten: true, nodePodCIDRs: []string{"10.244.1.0/24"},
		wireguardManager:         meshManager,
		gatewayWireguardManagers: map[string]*unboundednetnetlink.WireGuardManager{"wg51821": gwManager},
	}
	cfg := newCNISafetyTestConfig(t)
	cfg.NodeName, cfg.WireGuardPort, cfg.MTU = "self", 51820, 1200

	originalConfigure, originalBridge, originalVerify := configureWireGuardFunc, ensureCNIBridgeMTUFunc, verifyActiveTunnelMTUsFunc

	t.Cleanup(func() {
		configureWireGuardFunc, ensureCNIBridgeMTUFunc, verifyActiveTunnelMTUsFunc = originalConfigure, originalBridge, originalVerify
	})

	configureCalls := 0
	configureWireGuardFunc = func(context.Context, *config, string, []meshPeerInfo, []gatewayPeerInfo, string, map[string]bool, map[string]bool, map[string]bool, map[string]string, map[string]string, map[string]string, map[string]string, map[string]string, map[string]int, map[string]int, map[string]int, map[string]int, map[string]int, []unboundednetnetlink.DesiredRoute, map[string]bool, *wireGuardState, map[string]healthcheck.HealthCheckSettings) error {
		configureCalls++
		return nil
	}
	ensureCNIBridgeMTUFunc = func(string, int, *unboundednetnetlink.NetlinkCache, bool) error { return nil }
	verifyActiveTunnelMTUsFunc = func(*config, int, []meshPeerInfo, []gatewayPeerInfo) error { return nil }

	update := func() error {
		return updateWireGuardFromSlices(t.Context(), client, sites, empty, pools, gatewayNodes, empty, empty, empty,
			cfg, "local", "private", "pub-self", true, state)
	}

	if err := update(); err != nil {
		t.Fatal(err)
	}

	if configureCalls != 1 || state.dataplaneDirty {
		t.Fatalf("expected one full configuration, got calls=%d dirty=%v", configureCalls, state.dataplaneDirty)
	}

	// No drift: the heartbeat reads the devices but does not write them.
	if err := update(); err != nil {
		t.Fatal(err)
	}

	if gwClient.writes != 0 || meshClient.writes != 0 {
		t.Fatalf("heartbeat without drift wrote WireGuard config: gateway=%d mesh=%d", gwClient.writes, meshClient.writes)
	}

	gwClient.setEndpoint(t, gatewayKey, "1.2.3.4:51820")
	meshClient.setEndpoint(t, natPeerKey, "5.6.7.8:40000")

	if err := update(); err != nil {
		t.Fatal(err)
	}

	if configureCalls != 1 {
		t.Fatalf("drift repair went through a full reconfigure: calls=%d", configureCalls)
	}

	if got := gwClient.endpoint(t, gatewayKey); got != "1.2.3.4:51821" {
		t.Fatalf("gateway endpoint after heartbeat = %s, want 1.2.3.4:51821", got)
	}

	if got := meshClient.endpoint(t, natPeerKey); got != "5.6.7.8:40000" || meshClient.writes != 0 {
		t.Fatalf("learned endpoint of a peer without a desired endpoint was changed: %s writes=%d", got, meshClient.writes)
	}

	// A device that cannot be read is reported, and the next pass runs a full
	// configuration, which recreates missing interfaces.
	gwClient.deviceErr = errors.New("no such device")

	if err := update(); err == nil || !state.dataplaneDirty {
		t.Fatalf("expected drift check failure to mark the dataplane dirty, err=%v dirty=%v", err, state.dataplaneDirty)
	}

	gwClient.deviceErr = nil

	if err := update(); err != nil || configureCalls != 2 || state.dataplaneDirty {
		t.Fatalf("expected a full configuration after the failed check, err=%v calls=%d dirty=%v", err, configureCalls, state.dataplaneDirty)
	}
}

func TestRepairWireGuardDriftChecksEveryManagedInterface(t *testing.T) {
	gatewayA := driftTestKey(t).PublicKey().String()
	gatewayB := driftTestKey(t).PublicKey().String()
	meshPeer := driftTestKey(t).PublicKey().String()

	meshManager, meshClient := newDriftTestManager(t, "wg51820", 51820, unboundednetnetlink.WireGuardPeer{
		PublicKey: meshPeer, Endpoint: "10.0.0.3:51820", AllowedIPs: []string{"10.0.0.3"},
	})
	managerA, clientA := newDriftTestManager(t, "wg51821", 51821, unboundednetnetlink.WireGuardPeer{
		PublicKey: gatewayA, Endpoint: "1.2.3.4:51821", AllowedIPs: []string{"10.0.0.1"},
	})
	managerB, clientB := newDriftTestManager(t, "wg51823", 51823, unboundednetnetlink.WireGuardPeer{
		PublicKey: gatewayB, Endpoint: "1.2.3.5:51823", AllowedIPs: []string{"10.0.0.2"},
	})

	state := &wireGuardState{
		wireguardManager: meshManager,
		gatewayWireguardManagers: map[string]*unboundednetnetlink.WireGuardManager{
			"wg51821": managerA,
			"wg51823": managerB,
			"wg51899": nil,
		},
	}

	meshClient.setEndpoint(t, meshPeer, "10.0.0.3:40000")
	clientB.setEndpoint(t, gatewayB, "1.2.3.5:51820")

	clientA.deviceErr = errors.New("no such device")

	err := repairWireGuardDrift(state)
	if err == nil {
		t.Fatal("expected the unreadable interface to be reported")
	}

	if got := meshClient.endpoint(t, meshPeer); got != "10.0.0.3:51820" {
		t.Fatalf("mesh endpoint = %s, want 10.0.0.3:51820", got)
	}

	if got := clientB.endpoint(t, gatewayB); got != "1.2.3.5:51823" {
		t.Fatalf("one failing interface stopped the repair of the others: endpoint = %s", got)
	}

	clientA.deviceErr = nil

	if err := repairWireGuardDrift(state); err != nil {
		t.Fatal(err)
	}

	if err := repairWireGuardDrift(&wireGuardState{}); err != nil {
		t.Fatalf("state without managers: %v", err)
	}
}
