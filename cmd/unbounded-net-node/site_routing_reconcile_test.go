// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestUpdateWireGuardFromSlices_DirectMeshComponentMTU(t *testing.T) {
	sites, slices := newInformerWithObjects(), newInformerWithObjects()

	for i, name := range []string{"a", "b", "c"} {
		site := &unboundedv1alpha3.Site{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       unboundedv1alpha3.SiteSpec{TunnelProtocol: new(unboundednetv1alpha1.TunnelProtocolNone)},
		}
		if err := sites.GetStore().Add(toUnstructured(t, site)); err != nil {
			t.Fatal(err)
		}

		slice := &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: name}, SiteName: name,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name: name, WireGuardPublicKey: "key-" + name, PodCIDRs: []string{fmt.Sprintf("10.244.%d.0/24", i)},
				InternalIPs: []string{fmt.Sprintf("192.0.2.%d", i+1)},
			}},
		}
		if err := slices.GetStore().Add(toUnstructured(t, slice)); err != nil {
			t.Fatal(err)
		}
	}

	peerings := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SitePeering{
			ObjectMeta: metav1.ObjectMeta{Name: "a-b"},
			Spec:       unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"a", "b"}, TunnelMTU: ptrInt32(1200)},
		}),
		toUnstructured(t, &unboundednetv1alpha1.SitePeering{
			ObjectMeta: metav1.ObjectMeta{Name: "a-c"},
			Spec:       unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"a", "c"}, TunnelMTU: ptrInt32(1400)},
		}),
	)
	original := configureWireGuardFunc

	t.Cleanup(func() { configureWireGuardFunc = original })

	configureWireGuardFunc = func(context.Context, *config, string, []meshPeerInfo, []gatewayPeerInfo, string, map[string]bool, map[string]bool, map[string]bool, map[string]string, map[string]string, map[string]string, map[string]string, map[string]string, map[string]int, map[string]int, map[string]int, map[string]int, map[string]int, []unboundednetnetlink.DesiredRoute, map[string]bool, *wireGuardState, map[string]healthcheck.HealthCheckSettings) error {
		return nil
	}
	empty := newInformerWithObjects()

	for _, name := range []string{"a", "b", "c"} {
		t.Run(name, func(t *testing.T) {
			cfg := &config{NodeName: name, MTU: 1420, WireGuardPort: 51820}

			state := &wireGuardState{nodeName: name, clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})}
			if err := updateWireGuardFromSlices(t.Context(), nil, sites, slices, empty, empty, peerings, empty, empty,
				cfg, name, "private", "key-"+name, true, state); err != nil {
				t.Fatal(err)
			}

			if state.cniMTU != 1200 {
				t.Fatalf("Site %s fabric MTU=%d, want common 1200", name, state.cniMTU)
			}

			wantPeers := 1
			if name == "a" {
				wantPeers = 2
			}

			if len(state.peers) != wantPeers {
				t.Fatalf("Site %s gained transitive peers: %+v", name, state.peers)
			}

			for _, peer := range state.peers {
				if peer.TunnelMTU != 1200 || (name != "a" && peer.SiteName != "a") {
					t.Fatalf("Site %s peer is not a capped direct neighbor: %+v", name, peer)
				}
			}
		})
	}
}

func TestUpdateWireGuardFromSlices_LocalGatewayExclusions(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "local"}}

	siteInformer := newInformerWithObjects(toUnstructured(t, site))
	if err := siteInformer.GetStore().Add(toUnstructured(t, &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "origin"},
		Spec:       unboundedv1alpha3.SiteSpec{TunnelMTU: ptrInt32(1200)},
	})); err != nil {
		t.Fatal(err)
	}

	gatewayNodes := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.GatewayPoolNode{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway"},
		Status: unboundednetv1alpha1.GatewayNodeStatus{
			LastUpdated: metav1.Now(),
			Routes: map[string]unboundednetv1alpha1.GatewayNodeRoute{
				"192.168.0.0/16": {
					Type: "RoutedCidr",
					Paths: [][]unboundednetv1alpha1.GatewayNodePathHop{{
						{Type: "Site", Name: "origin"}, {Type: "GatewayPool", Name: "pool"},
					}},
				},
			},
		},
	}))
	poolInformer := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type: "Internal", RoutedCidrs: []string{"192.168.0.0/16"},
		},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
			Name: "gateway", SiteName: "remote", WireGuardPublicKey: "pub-gateway",
			InternalIPs: []string{"10.2.0.1"}, PodCIDRs: []string{"10.245.0.0/24"},
			GatewayWireguardPort: 51821,
		}}},
	}))
	assignments := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "assignment"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"local"}, GatewayPools: []string{"pool"},
		},
	}))
	empty := newInformerWithObjects()
	cfg := &config{NodeName: "self", WireGuardPort: 51820, MTU: 576, PreferredPrivateEncap: "WireGuard"}
	state := &wireGuardState{
		nodeName: "self", clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
	}

	calls := 0

	var gotGatewayPeers []gatewayPeerInfo

	original := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, _ []meshPeerInfo, gatewayPeers []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState, _ map[string]healthcheck.HealthCheckSettings) error {
		calls++
		gotGatewayPeers = gatewayPeers

		return nil
	}

	t.Cleanup(func() { configureWireGuardFunc = original })

	update := func() error {
		return updateWireGuardFromSlices(context.Background(), nil, siteInformer, empty, poolInformer, gatewayNodes,
			empty, assignments, empty, cfg, "local", "priv", "pub-self", true, state)
	}

	for i, tt := range []struct {
		name  string
		local []string
		want  []string
	}{
		{name: "baseline", want: []string{"192.168.0.0/16"}},
		{name: "partial exclusion", local: []string{"192.168.0.0/17"}, want: []string{"192.168.128.0/17"}},
		{name: "full exclusion", local: []string{"192.168.0.0/16"}},
		{name: "removed exclusion", want: []string{"192.168.0.0/16"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			site.Spec.LocalCIDRs = tt.local
			if err := siteInformer.GetStore().Update(toUnstructured(t, site)); err != nil {
				t.Fatal(err)
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != i+1 {
				t.Fatalf("configuration calls=%d, want %d", calls, i+1)
			}

			if len(gotGatewayPeers) != 1 || !slices.Equal(gotGatewayPeers[0].RoutedCidrs, tt.want) {
				t.Fatalf("gateway routes=%+v, want %v", gotGatewayPeers, tt.want)
			}

			wantTransitMTU := 1200
			if len(tt.want) == 0 {
				wantTransitMTU = 0
			}

			if gotGatewayPeers[0].TransitTunnelMTU != wantTransitMTU {
				t.Fatalf("retained path MTU=%d, want %d", gotGatewayPeers[0].TransitTunnelMTU, wantTransitMTU)
			}

			if gotGatewayPeers[0].WireGuardPublicKey != "pub-gateway" || !slices.Equal(gotGatewayPeers[0].PodCIDRs, []string{"10.245.0.0/24"}) {
				t.Fatal("local exclusions changed gateway identity")
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != i+1 {
				t.Fatal("unchanged exclusions caused another update")
			}
		})
	}

	gatewayObject, exists, err := gatewayNodes.GetStore().GetByKey("gateway")
	if err != nil || !exists {
		t.Fatalf("get gateway: exists=%v err=%v", exists, err)
	}

	if err := gatewayNodes.GetStore().Delete(gatewayObject); err != nil {
		t.Fatal(err)
	}

	if err := update(); err != nil {
		t.Fatal(err)
	}

	if len(gotGatewayPeers) != 0 || len(state.gatewayPeers) != 0 {
		t.Fatal("missing GatewayPoolNode retained a route destination")
	}

	if err := gatewayNodes.GetStore().Add(gatewayObject); err != nil {
		t.Fatal(err)
	}

	if err := update(); err != nil {
		t.Fatal(err)
	}

	if len(gotGatewayPeers) != 1 {
		t.Fatal("restored GatewayPoolNode did not restore route destination")
	}
}

func TestUpdateWireGuardFromSlices_GatewayRoutingChanges(t *testing.T) {
	for _, tt := range []struct {
		name          string
		change        func(local, remote *unboundedv1alpha3.Site, pool *unboundednetv1alpha1.GatewayPool)
		failConfigure bool
		wantChange    bool
	}{
		{
			name: "remote pod pools",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.PodCidrAssignments[0].CidrBlocks = []string{"10.246.0.0/16"}
			},
			wantChange: true,
		},
		{
			name: "remote node CIDRs",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.NodeCidrs = []string{"10.3.0.0/16"}
			},
			wantChange: true,
		},
		{
			name: "local node CIDRs",
			change: func(local, _ *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				local.Spec.NodeCidrs = []string{"10.4.0.0/16"}
			},
			wantChange: true,
		},
		{
			name: "local CIDR exclusions",
			change: func(local, _ *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				local.Spec.LocalCIDRs = []string{"192.168.1.0/24"}
			},
			wantChange: true,
		},
		{
			name: "gateway pools failure retries",
			change: func(_, _ *unboundedv1alpha3.Site, pool *unboundednetv1alpha1.GatewayPool) {
				pool.Spec.RoutedCidrs = []string{"10.241.0.0/16"}
			},
			failConfigure: true,
			wantChange:    true,
		},
		{
			name: "gateway Site inputs failure retries",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.NodeCidrs = []string{"10.3.0.0/16"}
			},
			failConfigure: true,
			wantChange:    true,
		},
		{
			name: "equivalent gateway pools",
			change: func(_, _ *unboundedv1alpha3.Site, pool *unboundednetv1alpha1.GatewayPool) {
				pool.Spec.RoutedCidrs = []string{"10.240.0.0/16", "10.240.0.0/16"}
			},
		},
		{
			name: "equivalent Site inputs",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.NodeCidrs = []string{"10.2.0.0/16", "10.2.0.0/16"}
				remote.Spec.PodCidrAssignments[0].CidrBlocks = []string{"10.245.0.0/16", "10.245.0.0/16"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local := &unboundedv1alpha3.Site{
				ObjectMeta: metav1.ObjectMeta{Name: "local"},
				Spec: unboundedv1alpha3.SiteSpec{
					NodeCidrs:          []string{"10.1.0.0/16"},
					PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{CidrBlocks: []string{"10.244.0.0/16"}}},
				},
			}

			remote := &unboundedv1alpha3.Site{
				ObjectMeta: metav1.ObjectMeta{Name: "remote"},
				Spec: unboundedv1alpha3.SiteSpec{
					NodeCidrs:          []string{"10.2.0.0/16"},
					PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{CidrBlocks: []string{"10.245.0.0/16"}}},
				},
			}
			pool := &unboundednetv1alpha1.GatewayPool{
				ObjectMeta: metav1.ObjectMeta{Name: "local-pool"},
				Spec:       unboundednetv1alpha1.GatewayPoolSpec{RoutedCidrs: []string{"10.240.0.0/16"}},
				Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
					Name: "self", SiteName: "local", WireGuardPublicKey: "pub-self", GatewayWireguardPort: 51821,
				}}},
			}
			siteInformer := newInformerWithObjects(toUnstructured(t, local), toUnstructured(t, remote))
			poolInformer := newInformerWithObjects(toUnstructured(t, pool))
			empty := newInformerWithObjects()
			cfg := &config{NodeName: "self", WireGuardPort: 51820, PreferredPrivateEncap: "WireGuard"}
			state := &wireGuardState{
				nodeName:  "self",
				clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
			}

			calls := 0

			var configureErr error

			original := configureWireGuardFunc
			configureWireGuardFunc = func(context.Context, *config, string, []meshPeerInfo, []gatewayPeerInfo, string, map[string]bool, map[string]bool, map[string]bool, map[string]string, map[string]string, map[string]string, map[string]string, map[string]string, map[string]int, map[string]int, map[string]int, map[string]int, map[string]int, []unboundednetnetlink.DesiredRoute, map[string]bool, *wireGuardState, map[string]healthcheck.HealthCheckSettings) error {
				calls++
				return configureErr
			}

			t.Cleanup(func() { configureWireGuardFunc = original })

			update := func() error {
				return updateWireGuardFromSlices(context.Background(), nil, siteInformer, empty, poolInformer, empty,
					empty, empty, empty, cfg, "local", "priv", "pub-self", true, state)
			}
			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != 1 {
				t.Fatalf("initial calls=%d, want 1", calls)
			}

			tt.change(local, remote, pool)

			for _, site := range []*unboundedv1alpha3.Site{local, remote} {
				if err := siteInformer.GetStore().Update(toUnstructured(t, site)); err != nil {
					t.Fatal(err)
				}
			}

			if err := poolInformer.GetStore().Update(toUnstructured(t, pool)); err != nil {
				t.Fatal(err)
			}

			wantCalls := 1

			if tt.failConfigure {
				configureErr = errors.New("configuration failed")
				if err := update(); !errors.Is(err, configureErr) {
					t.Fatalf("expected configuration failure, got %v", err)
				}

				if state.reconcileCount != 1 {
					t.Fatal("failed attempt committed routing state")
				}

				wantCalls++
				configureErr = nil
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if tt.wantChange {
				wantCalls++
			}

			if calls != wantCalls {
				t.Fatalf("configuration calls=%d, want %d", calls, wantCalls)
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != wantCalls {
				t.Fatal("stable routing inputs caused another update")
			}
		})
	}
}

// peeredGatewayFixture reconciles a gateway node in Site "local" beside Sites
// "remote" and "third", each with one node CIDR and one pod pool.
type peeredGatewayFixture struct {
	sites, pools, peerings, empty cache.SharedIndexInformer
	cfg                           *config
	state                         *wireGuardState
	calls                         int
}

func newPeeredGatewayFixture(t *testing.T) *peeredGatewayFixture {
	t.Helper()

	f := &peeredGatewayFixture{
		sites:    newInformerWithObjects(),
		peerings: newInformerWithObjects(),
		empty:    newInformerWithObjects(),
		cfg:      &config{NodeName: "self", WireGuardPort: 51820, PreferredPrivateEncap: "WireGuard"},
		state: &wireGuardState{
			nodeName:  "self",
			clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
		},
	}

	for name, cidrs := range map[string][2]string{
		"local":  {"10.1.0.0/16", "10.244.0.0/16"},
		"remote": {"10.2.0.0/16", "10.245.0.0/16"},
		"third":  {"10.3.0.0/16", "10.246.0.0/16"},
	} {
		site := &unboundedv1alpha3.Site{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: unboundedv1alpha3.SiteSpec{
				NodeCidrs:          []string{cidrs[0]},
				PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{CidrBlocks: []string{cidrs[1]}}},
			},
		}
		if err := f.sites.GetStore().Add(toUnstructured(t, site)); err != nil {
			t.Fatal(err)
		}
	}

	f.pools = newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "local-pool"},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
			Name: "self", SiteName: "local", WireGuardPublicKey: "pub-self", GatewayWireguardPort: 51821,
		}}},
	}))

	original := configureWireGuardFunc
	configureWireGuardFunc = func(context.Context, *config, string, []meshPeerInfo, []gatewayPeerInfo, string, map[string]bool, map[string]bool, map[string]bool, map[string]string, map[string]string, map[string]string, map[string]string, map[string]string, map[string]int, map[string]int, map[string]int, map[string]int, map[string]int, []unboundednetnetlink.DesiredRoute, map[string]bool, *wireGuardState, map[string]healthcheck.HealthCheckSettings) error {
		f.calls++
		return nil
	}

	t.Cleanup(func() { configureWireGuardFunc = original })

	return f
}

func (f *peeredGatewayFixture) setPeering(t *testing.T, spec unboundednetv1alpha1.SitePeeringSpec) {
	t.Helper()

	peering := &unboundednetv1alpha1.SitePeering{ObjectMeta: metav1.ObjectMeta{Name: "peering"}, Spec: spec}
	if err := f.peerings.GetStore().Add(toUnstructured(t, peering)); err != nil {
		t.Fatal(err)
	}
}

// gatewaySupernets reconciles once and returns the extra unbounded0 routes the
// gateway node computed.
func (f *peeredGatewayFixture) gatewaySupernets(t *testing.T) []string {
	t.Helper()

	if err := updateWireGuardFromSlices(context.Background(), nil, f.sites, f.empty, f.pools, f.empty,
		f.peerings, f.empty, f.empty, f.cfg, "local", "priv", "pub-self", true, f.state); err != nil {
		t.Fatal(err)
	}

	return f.state.siteRouting.gatewaySupernets
}

func TestUpdateWireGuardFromSlices_GatewaySkipsPeeredNodeCIDRs(t *testing.T) {
	for _, tt := range []struct {
		name      string
		peering   *unboundednetv1alpha1.SitePeeringSpec
		wantRoute bool
	}{
		{name: "no peering", wantRoute: true},
		{name: "meshNodes unset", peering: &unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"local", "remote"}}},
		{name: "meshNodes false", peering: &unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"local", "remote"}, MeshNodes: new(false)}},
		{name: "disabled peering", peering: &unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"local", "remote"}, Enabled: new(false)}, wantRoute: true},
		{name: "peering between other Sites", peering: &unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"remote", "third"}}, wantRoute: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newPeeredGatewayFixture(t)
			if tt.peering != nil {
				f.setPeering(t, *tt.peering)
			}

			routes := f.gatewaySupernets(t)
			if got := slices.Contains(routes, "10.2.0.0/16"); got != tt.wantRoute {
				t.Fatalf("remote node CIDR in gateway routes=%v, want %v: %v", got, tt.wantRoute, routes)
			}

			// The remote pod pool and the unpeered Site never depend on the peering.
			for _, cidr := range []string{"10.245.0.0/16", "10.3.0.0/16"} {
				if !slices.Contains(routes, cidr) {
					t.Fatalf("gateway route %s was lost: %v", cidr, routes)
				}
			}
		})
	}
}

func TestUpdateWireGuardFromSlices_GatewayFollowsSitePeeringChanges(t *testing.T) {
	f := newPeeredGatewayFixture(t)
	peering := unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"local", "remote"}}

	reconcile := func(step string, wantRoute bool, wantCalls int) {
		t.Helper()

		routes := f.gatewaySupernets(t)
		if got := slices.Contains(routes, "10.2.0.0/16"); got != wantRoute {
			t.Fatalf("%s: remote node CIDR in gateway routes=%v, want %v: %v", step, got, wantRoute, routes)
		}

		if f.calls != wantCalls {
			t.Fatalf("%s: configuration calls=%d, want %d", step, f.calls, wantCalls)
		}
	}

	f.setPeering(t, peering)
	reconcile("peered", false, 1)
	reconcile("unchanged", false, 1)

	if err := f.peerings.GetStore().Replace(nil, ""); err != nil {
		t.Fatal(err)
	}

	reconcile("peering deleted", true, 2)

	f.setPeering(t, peering)
	reconcile("peering restored", false, 3)
}
