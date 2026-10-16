// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestGatewayPoolPeeringProtocolResolution(t *testing.T) {
	for _, tt := range []struct {
		name          string
		protocol      string
		poolProtocol  string
		site          string
		poolType      string
		localExternal bool
		networkPeered bool
		isGateway     bool
		samePool      bool
		want          string
	}{
		{name: "explicit WireGuard", protocol: "WireGuard", want: "WireGuard", isGateway: true},
		{name: "explicit IPIP", protocol: "IPIP", want: "IPIP", isGateway: true},
		{name: "explicit GENEVE", protocol: "GENEVE", want: "GENEVE", isGateway: true},
		{name: "explicit VXLAN", protocol: "VXLAN", want: "VXLAN", isGateway: true},
		{name: "explicit None", protocol: "None", want: "None", isGateway: true},
		{name: "explicit protocol overrides public default", protocol: "None", site: "remote", poolType: "External", want: "None", isGateway: true},
		{name: "Auto does not inherit same-site override", protocol: "Auto", want: "GENEVE", isGateway: true},
		{name: "Auto internal cross-site", protocol: "Auto", site: "remote", want: "GENEVE", isGateway: true},
		{name: "Auto external remote pool", protocol: "Auto", site: "remote", poolType: "External", want: "WireGuard", isGateway: true},
		{name: "Auto external local pool", protocol: "Auto", site: "remote", localExternal: true, want: "WireGuard", isGateway: true},
		{name: "Auto network-peered uses internal endpoint", protocol: "Auto", site: "remote", poolType: "External", networkPeered: true, want: "GENEVE", isGateway: true},
		{name: "unset preserves pool fallback", want: "IPIP", isGateway: true},
		{name: "unset preserves Site fallback", poolProtocol: "Auto", want: "WireGuard", isGateway: true},
		{name: "ordinary site-to-gateway ignores peering", protocol: "None", want: "VXLAN"},
		{name: "same-pool gateway ignores peering", protocol: "None", want: "IPIP", isGateway: true, samePool: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			siteName := tt.site
			if siteName == "" {
				siteName = "local"
			}

			gateways := []gatewayPeerInfo{{
				SiteName:              siteName,
				PoolName:              "remote-pool",
				PoolType:              tt.poolType,
				PeeringTunnelProtocol: tt.protocol,
			}}
			mesh := []meshPeerInfo{{SiteName: "local"}}

			localPools := []string{"local-pool"}
			if tt.samePool {
				localPools = append(localPools, "remote-pool")
			}

			poolProtocol := tt.poolProtocol
			if poolProtocol == "" {
				poolProtocol = "IPIP"
			}

			resolveTunnelProtocolsOnPeers(mesh, gateways, "local", nil,
				map[string]bool{"remote": tt.networkPeered}, tt.isGateway, tt.localExternal,
				localPools, "GENEVE", "WireGuard",
				map[string]string{"local": "WireGuard"}, nil,
				map[string]string{"local|remote-pool": "VXLAN"},
				map[string]string{"remote-pool": poolProtocol})

			if gateways[0].TunnelProtocol != tt.want {
				t.Fatalf("protocol=%q, want %q", gateways[0].TunnelProtocol, tt.want)
			}

			if mesh[0].TunnelProtocol != "WireGuard" {
				t.Fatalf("peering changed mesh protocol to %q", mesh[0].TunnelProtocol)
			}
		})
	}
}

func TestPinnedGatewayPoolWaitsForPublishedIdentity(t *testing.T) {
	fixture := newPoolPeeringProtocolFixture(t, true)
	fixture.state.isGatewayNode = true
	fixture.state.adoptedAssignment = nodeAssignment{
		site:        "local",
		gatewayPool: "not-ready",
		nodeUID:     "node-uid",
	}

	err := fixture.update()
	if err == nil || !strings.Contains(err.Error(), "has not published this node identity and settings yet") {
		t.Fatalf("error = %v", err)
	}

	if fixture.configureCalls != 0 {
		t.Fatalf("configuration ran before assigned GatewayPool readiness: %d calls", fixture.configureCalls)
	}
}

func TestPinnedGatewayPoolAcknowledgmentWaitsForTaint(t *testing.T) {
	fixture := newPoolPeeringProtocolFixture(t, true)
	fixture.state.isGatewayNode = true
	fixture.state.adoptedAssignment = nodeAssignment{
		site:        "local",
		gatewayPool: "local-a",
		nodeUID:     "node-uid",
	}
	client := fixture.state.clientset.(*fake.Clientset)
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("taint denied")
	})

	err := fixture.update()
	if err == nil || !strings.Contains(err.Error(), "taint gateway node") {
		t.Fatalf("error = %v", err)
	}

	if fixture.state.gatewayTaintApplied {
		t.Fatal("failed taint was recorded as applied")
	}

	if fixture.configureCalls != 0 {
		t.Fatalf("configuration ran before gateway taint adoption: %d calls", fixture.configureCalls)
	}
}

type poolPeeringProtocolFixture struct {
	peerings       cache.SharedIndexInformer
	pools          cache.SharedIndexInformer
	state          *wireGuardState
	update         func() error
	configureCalls int
	configureErr   error
	gateways       []gatewayPeerInfo
	mesh           []meshPeerInfo
}

func newPoolPeeringProtocolFixture(t *testing.T, gateway bool) *poolPeeringProtocolFixture {
	t.Helper()

	const siteName = "local"

	siteInformer := newInformerWithObjects(toUnstructured(t, &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: siteName},
		Spec:       unboundedv1alpha3.SiteSpec{TunnelProtocol: new(unboundednetv1alpha1.TunnelProtocolNone)},
	}))
	self := unboundednetv1alpha1.GatewayNodeInfo{
		Name:                 "self",
		SiteName:             siteName,
		WireGuardPublicKey:   "pub-self",
		GatewayWireguardPort: 51821,
		InternalIPs:          []string{"10.0.0.1"},
		PodCIDRs:             []string{"10.244.1.0/24"},
	}
	samePool := self
	samePool.Name = "same-pool"
	samePool.WireGuardPublicKey = "pub-same"
	samePool.InternalIPs = []string{"10.0.0.2"}
	samePool.PodCIDRs = []string{"10.244.2.0/24"}
	remote := self
	remote.Name = "remote"
	remote.WireGuardPublicKey = "pub-remote"
	remote.InternalIPs = []string{"10.0.0.3"}
	remote.PodCIDRs = []string{"10.244.3.0/24"}

	f := &poolPeeringProtocolFixture{
		peerings: newInformerWithObjects(),
		pools:    newInformerWithObjects(),
		state: &wireGuardState{
			nodeName:  "self",
			clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
		},
	}

	for _, name := range []string{"local-a", "local-z", "remote-pool"} {
		var nodes []unboundednetv1alpha1.GatewayNodeInfo
		if name == "local-a" {
			nodes = append(nodes, samePool)
		}

		if gateway {
			nodes = append(nodes, self)
		}

		if name == "remote-pool" {
			nodes = []unboundednetv1alpha1.GatewayNodeInfo{remote}
		}

		pool := &unboundednetv1alpha1.GatewayPool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: unboundednetv1alpha1.GatewayPoolSpec{
				Type:           "Internal",
				TunnelProtocol: new(unboundednetv1alpha1.TunnelProtocolNone),
			},
			Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: nodes},
		}
		if err := f.pools.GetStore().Add(toUnstructured(t, pool)); err != nil {
			t.Fatal(err)
		}
	}

	assignmentInformer := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "local-to-remote"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites:          []string{siteName},
			GatewayPools:   []string{"remote-pool"},
			TunnelProtocol: new(unboundednetv1alpha1.TunnelProtocolNone),
		},
	}))
	empty := newInformerWithObjects()
	cfg := &config{NodeName: "self", WireGuardPort: 51820, WireGuardInterfacePrefix: "gpp-test-wg", MTU: 576, PreferredPrivateEncap: "WireGuard"}
	f.update = func() error {
		return updateWireGuardFromSlices(context.Background(), nil, siteInformer, empty, f.pools,
			empty, empty, assignmentInformer, f.peerings, cfg, siteName, "priv", "pub-self", true, f.state)
	}

	original := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, mesh []meshPeerInfo, gateways []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState, _ map[string]healthcheck.HealthCheckSettings) error {
		f.configureCalls++

		f.gateways = append([]gatewayPeerInfo(nil), gateways...)
		f.mesh = append([]meshPeerInfo(nil), mesh...)

		return f.configureErr
	}

	t.Cleanup(func() { configureWireGuardFunc = original })

	return f
}

func (f *poolPeeringProtocolFixture) putPeering(t *testing.T, name, localPool string, protocol *unboundednetv1alpha1.TunnelProtocol, enabled bool) {
	t.Helper()

	peering := &unboundednetv1alpha1.GatewayPoolPeering{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{
			GatewayPools:   []string{localPool, "remote-pool"},
			TunnelProtocol: protocol,
			Enabled:        new(enabled),
		},
	}
	if err := f.peerings.GetStore().Update(toUnstructured(t, peering)); err != nil {
		t.Fatal(err)
	}
}

func (f *poolPeeringProtocolFixture) checkGateway(t *testing.T, protocol, override string) {
	t.Helper()

	if len(f.gateways) != 1 || f.gateways[0].TunnelProtocol != protocol || f.gateways[0].PeeringTunnelProtocol != override {
		t.Fatalf("gateway configuration=%+v, want protocol=%q override=%q", f.gateways, protocol, override)
	}

	for _, peer := range f.mesh {
		if peer.TunnelProtocol != "None" {
			t.Fatalf("cross-pool override leaked to same-pool mesh peer: %+v", peer)
		}
	}
}

func TestGatewayAssignmentMTUDoesNotLeakFromWorkerAssignments(t *testing.T) {
	f := newPoolPeeringProtocolFixture(t, true)
	f.state.adoptedAssignment = nodeAssignment{site: "local", gatewayPool: "local-a"}
	sites := newInformerWithObjects(
		toUnstructured(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "local"}}),
		toUnstructured(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-b"}}),
	)
	assignments := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-only"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Sites: []string{"local", "site-b"}, GatewayPools: []string{"remote-pool"}, TunnelMTU: ptrInt32(1100),
			},
		}),
		toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
			ObjectMeta: metav1.ObjectMeta{Name: "gateway-facing"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Sites: []string{"site-b"}, GatewayPools: []string{"local-a"}, TunnelMTU: ptrInt32(1400),
			},
		}),
	)
	empty := newInformerWithObjects()

	cfg := &config{NodeName: "self", WireGuardPort: 51820, WireGuardInterfacePrefix: "gpp-test-wg", MTU: 576}
	if err := updateWireGuardFromSlices(t.Context(), nil, sites, empty, f.pools, empty, empty, assignments, empty,
		cfg, "local", "priv", "pub-self", true, f.state); err != nil {
		t.Fatal(err)
	}

	if got := f.state.assignmentSiteTunnelMTUs["site-b"]; got != 1400 {
		t.Fatalf("gateway-to-Site cap=%d, want 1400, ignoring worker assignment via unrelated pool", got)
	}

	if got := f.state.assignmentPoolTunnelMTUs["remote-pool"]; got != 0 {
		t.Fatalf("worker-side pool cap leaked into gateway path: %d", got)
	}
}

func TestGatewayPoolPeeringProtocolUpdates(t *testing.T) {
	f := newPoolPeeringProtocolFixture(t, true)
	for i, step := range []struct {
		protocol *unboundednetv1alpha1.TunnelProtocol
		want     string
		override string
	}{
		{want: "None"},
		{protocol: new(unboundednetv1alpha1.TunnelProtocolWireGuard), want: "WireGuard", override: "WireGuard"},
		{protocol: new(unboundednetv1alpha1.TunnelProtocolAuto), want: "WireGuard", override: "Auto"},
		{want: "None"},
		{protocol: new(unboundednetv1alpha1.TunnelProtocolNone), want: "None", override: "None"},
	} {
		before := append([]gatewayPeerInfo(nil), f.state.gatewayPeers...)
		f.putPeering(t, "peering", "local-z", step.protocol, true)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, step.want, step.override)

		if f.configureCalls != i+1 || f.state.reconcileCount != i+1 {
			t.Fatalf("step %d: calls=%d count=%d, want %d", i, f.configureCalls, f.state.reconcileCount, i+1)
		}

		if len(before) > 0 {
			after := append([]gatewayPeerInfo(nil), f.state.gatewayPeers...)
			for _, peers := range [][]gatewayPeerInfo{before, after} {
				peers[0].TunnelProtocol = ""
				peers[0].PeeringTunnelProtocol = ""
				peers[0].TunnelMTU = 0
			}

			if !gatewayPeersEqual(before, after) {
				t.Fatalf("protocol-only update changed gateway identity or routes: before=%+v after=%+v", before, after)
			}
		}

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		if f.configureCalls != i+1 || f.state.reconcileCount != i+1 {
			t.Fatalf("step %d: unchanged peering did not skip reconciliation: calls=%d count=%d", i, f.configureCalls, f.state.reconcileCount)
		}
	}
}

func TestGatewayPoolPeeringProtocolConflictsAndScope(t *testing.T) {
	t.Run("multiple local pools and deterministic conflict winner", func(t *testing.T) {
		f := newPoolPeeringProtocolFixture(t, true)
		f.putPeering(t, "z-later", "local-z", new(unboundednetv1alpha1.TunnelProtocolNone), true)
		f.putPeering(t, "a-first", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), true)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "WireGuard", "WireGuard")
		f.putPeering(t, "a-first", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), false)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "None", "None")
		f.putPeering(t, "a-first", "local-z", nil, true)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "None", "None")
		f.putPeering(t, "a-first", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), true)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "WireGuard", "WireGuard")

		winner, exists, err := f.peerings.GetStore().GetByKey("a-first")
		if err != nil || !exists {
			t.Fatalf("get winning peering: exists=%t err=%v", exists, err)
		}

		if err := f.peerings.GetStore().Delete(winner); err != nil {
			t.Fatal(err)
		}

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "None", "None")
	})

	t.Run("disabled peering establishes no gateway link", func(t *testing.T) {
		f := newPoolPeeringProtocolFixture(t, true)
		f.putPeering(t, "disabled", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), false)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		if len(f.gateways) != 0 {
			t.Fatalf("disabled peering created gateway peers: %+v", f.gateways)
		}
	})

	t.Run("site assignment does not inherit pool peering", func(t *testing.T) {
		f := newPoolPeeringProtocolFixture(t, false)
		f.putPeering(t, "peering", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), true)

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "None", "")
	})

	t.Run("gateway sharing a local pool is not overridden", func(t *testing.T) {
		f := newPoolPeeringProtocolFixture(t, true)
		f.putPeering(t, "peering", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), true)

		sharedPool := &unboundednetv1alpha1.GatewayPool{
			ObjectMeta: metav1.ObjectMeta{Name: "local-shared"},
			Spec:       unboundednetv1alpha1.GatewayPoolSpec{Type: "Internal"},
			Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{
				{Name: "self", WireGuardPublicKey: "pub-self"},
				{Name: "remote", WireGuardPublicKey: "pub-remote"},
			}},
		}
		if err := f.pools.GetStore().Add(toUnstructured(t, sharedPool)); err != nil {
			t.Fatal(err)
		}

		if err := f.update(); err != nil {
			t.Fatal(err)
		}

		f.checkGateway(t, "None", "")
	})
}

func TestGatewayPoolPeeringProtocolFailureRetries(t *testing.T) {
	f := newPoolPeeringProtocolFixture(t, true)
	f.putPeering(t, "peering", "local-z", nil, true)

	if err := f.update(); err != nil {
		t.Fatal(err)
	}

	f.putPeering(t, "peering", "local-z", new(unboundednetv1alpha1.TunnelProtocolWireGuard), true)

	f.configureErr = errors.New("configuration failed")
	if err := f.update(); !errors.Is(err, f.configureErr) {
		t.Fatalf("expected configuration error, got %v", err)
	}

	if f.state.gatewayPeers[0].PeeringTunnelProtocol != "" || f.state.reconcileCount != 1 {
		t.Fatal("failed update committed the peering protocol")
	}

	f.configureErr = nil
	if err := f.update(); err != nil {
		t.Fatal(err)
	}

	f.checkGateway(t, "WireGuard", "WireGuard")

	if f.configureCalls != 3 || f.state.reconcileCount != 2 {
		t.Fatalf("failed peering update was not retried: calls=%d count=%d", f.configureCalls, f.state.reconcileCount)
	}
}
