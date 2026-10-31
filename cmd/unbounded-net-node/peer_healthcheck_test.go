// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestRegisterHealthProfilesAllTunnelModes(t *testing.T) {
	for _, protocol := range []string{"WireGuard", "GENEVE", "VXLAN", "IPIP", "None"} {
		for _, tc := range []struct {
			name                                                                                   string
			gatewayPeer, gatewayNode                                                               bool
			override, site, peering, assignmentSite, assignmentLocal, assignmentRemote, pool, want string
		}{
			{name: "mesh default", site: "site", want: "site"},
			{name: "mesh peering", site: "site", peering: "peering", want: "peering"},
			{name: "mesh assignment", site: "site", peering: "peering", assignmentSite: "assignment", want: "assignment"},
			{name: "mesh gateway role", gatewayNode: true, site: "site", peering: "peering", assignmentSite: "assignment", want: "assignment"},
			{name: "mesh explicit pool", override: "pool", assignmentSite: "assignment", want: "pool"},
			{name: "mesh explicit pool peering", override: "pool-peering", assignmentSite: "assignment", want: "pool-peering"},
			{name: "mesh disabled assignment", site: "site", assignmentSite: disabledHealthCheckProfile},
			{name: "mesh disabled override", override: disabledHealthCheckProfile, site: "site"},
			{name: "gateway assignment", gatewayPeer: true, assignmentLocal: "assignment", assignmentRemote: "remote", pool: "pool", want: "assignment"},
			{name: "gateway pool", gatewayPeer: true, gatewayNode: true, assignmentRemote: "remote", pool: "pool", want: "pool"},
			{name: "gateway remote assignment fallback", gatewayPeer: true, gatewayNode: true, assignmentRemote: "remote", want: "remote"},
			{name: "gateway explicit", gatewayPeer: true, override: "pool-peering", assignmentLocal: "assignment", want: "pool-peering"},
			{name: "gateway disabled assignment", gatewayPeer: true, site: "site", assignmentLocal: disabledHealthCheckProfile},
			{name: "gateway disabled pool", gatewayPeer: true, gatewayNode: true, site: "site", pool: disabledHealthCheckProfile, assignmentRemote: "remote"},
			{name: "gateway disabled override", gatewayPeer: true, override: disabledHealthCheckProfile, site: "site", assignmentLocal: "assignment"},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				manager, err := healthcheck.NewManager("local", 0, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer manager.Stop()

				profiles := make(map[string]healthcheck.HealthCheckSettings)

				for i, name := range []string{"site", "peering", "assignment", "remote", "pool", "pool-peering"} {
					_, profile := healthCheckProfileFromSettings(&unboundednetv1alpha1.HealthCheckSettings{
						TransmitInterval: ptrIntOrString(intstr.FromInt(1000 + i)),
					}, name)
					profiles[name] = profile
				}

				state := &wireGuardState{
					healthCheckManager: manager, healthFlapMaxBackoff: 300 * time.Second,
					healthCheckProfiles:        map[string]healthcheck.HealthCheckSettings{"site": {TransmitInterval: time.Millisecond}},
					meshPeerHealthCheckEnabled: make(map[string]bool), gatewayPeerHealthCheckEnabled: make(map[string]bool),
				}

				var (
					mesh     []meshPeerInfo
					gateways []gatewayPeerInfo
				)
				if tc.gatewayPeer {
					gateways = []gatewayPeerInfo{{Name: "peer", SiteName: "remote", PoolName: "pool", PodCIDRs: []string{"10.244.1.0/24"}, HealthCheckProfileName: tc.override, TunnelProtocol: protocol}}
				} else {
					mesh = []meshPeerInfo{{Name: "peer", SiteName: "remote", WireGuardPublicKey: "pub", PodCIDRs: []string{"10.244.1.0/24"}, HealthCheckProfileName: tc.override, TunnelProtocol: protocol}}
				}

				desired, err := registerPeersWithHealthCheck(mesh, gateways, "local", tc.gatewayNode,
					map[string]string{"local": tc.site, "remote": tc.site}, map[string]string{"remote": tc.peering},
					map[string]string{"remote": tc.assignmentSite}, map[string]string{"local|pool": tc.assignmentLocal, "remote|pool": tc.assignmentRemote},
					map[string]string{"pool": tc.pool}, profiles, state, func(gatewayPeerInfo) string { return "iface" }, protocol != "WireGuard")
				if err != nil {
					t.Fatal(err)
				}

				if tc.want == "" {
					if desired["peer"] || len(manager.GetAllPeerStatuses()) != 0 || state.meshPeerHealthCheckEnabled["pub"] || state.gatewayPeerHealthCheckEnabled["iface"] {
						t.Fatal("disabled profile fell through to enabled lower-precedence profile")
					}

					return
				}

				want := profiles[tc.want]
				want.MaxBackoff = 300 * time.Second

				got, err := manager.GetPeerSettings("peer")
				if err != nil || got != want || !desired["peer"] {
					t.Fatalf("got %+v, %v; want %+v", got, err, want)
				}

				if got.ReceiveInterval != 15*time.Second {
					t.Fatal("partial profile lost 15s receive default")
				}
			})
		}
	}
}

func TestRegisterHealthProfileFallbackAndMissing(t *testing.T) {
	for _, siteFallback := range []bool{false, true} {
		manager, err := healthcheck.NewManager("local", 0, nil)
		if err != nil {
			t.Fatal(err)
		}

		state := &wireGuardState{healthCheckManager: manager, meshPeerHealthCheckEnabled: make(map[string]bool), gatewayPeerHealthCheckEnabled: make(map[string]bool)}
		gateways := []gatewayPeerInfo{{Name: "peer", PoolName: "pool", PodCIDRs: []string{"10.244.1.0/24"}}}

		desired, err := registerPeersWithHealthCheck(nil, gateways, "local", false, map[string]string{"local": "site"}, nil, nil, nil, nil,
			map[string]healthcheck.HealthCheckSettings{"site": healthcheck.DefaultSettings()}, state, func(gatewayPeerInfo) string { return "iface" }, siteFallback)
		if err != nil || desired["peer"] != siteFallback {
			t.Fatalf("site fallback %t: desired=%v err=%v", siteFallback, desired, err)
		}

		gateways[0].HealthCheckProfileName = "missing"

		desired, err = registerPeersWithHealthCheck(nil, gateways, "local", false, nil, nil, nil, nil, nil, nil, state, func(gatewayPeerInfo) string { return "iface" }, siteFallback)
		if !errors.Is(err, errRegisterHealthChecks) || !desired["peer"] {
			t.Fatal("missing fresh profile must fail without deleting existing session")
		}

		manager.Stop()
	}
}

func TestHealthProfilePartialUpdateResetsUnspecifiedValues(t *testing.T) {
	manager, err := healthcheck.NewManager("local", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()

	state := &wireGuardState{healthCheckManager: manager, healthFlapMaxBackoff: 240 * time.Second, meshPeerHealthCheckEnabled: make(map[string]bool)}
	peer := meshPeerInfo{Name: "peer", SiteName: "local", WireGuardPublicKey: "pub", PodCIDRs: []string{"10.244.1.0/24"}}
	profile := healthcheck.DefaultSettings()
	profile.TransmitInterval, profile.ReceiveInterval = time.Second, time.Second
	profiles := map[string]healthcheck.HealthCheckSettings{"site": profile}
	register := func() {
		t.Helper()

		if _, err := registerPeersWithHealthCheck([]meshPeerInfo{peer}, nil, "local", false, map[string]string{"local": "site"}, nil, nil, nil, nil, profiles, state, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	register()

	_, profiles["site"] = healthCheckProfileFromSettings(&unboundednetv1alpha1.HealthCheckSettings{
		TransmitInterval: ptrIntOrString(intstr.FromString("60s")),
	}, "site")

	register()

	got, err := manager.GetPeerSettings("peer")
	if err != nil || got.TransmitInterval != 60*time.Second || got.ReceiveInterval != 15*time.Second || got.MaxBackoff != 240*time.Second {
		t.Fatalf("partial update reused stale values: %+v %v", got, err)
	}
}

func TestDisabledAssignmentBlocksLowerPrecedence(t *testing.T) {
	assignment := unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "assignment"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"local", "remote"}, GatewayPools: []string{"pool"},
			HealthCheckSettings: &unboundednetv1alpha1.HealthCheckSettings{Enabled: ptrBool(false)},
		},
	}
	profiles := make(map[string]healthcheck.HealthCheckSettings)
	pools, sites := make(map[string]string), make(map[string]string)
	mergeAssignmentHealthCheckState(assignment, "local", nil, profiles, nil, pools, make(map[string]string), sites, make(map[string]string))

	if len(profiles) != 0 || pools["local|pool"] != disabledHealthCheckProfile || sites["remote"] != disabledHealthCheckProfile {
		t.Fatalf("disabled association was discarded: pools=%v sites=%v", pools, sites)
	}
}

func TestReconciliationPassesFreshHealthProfilesBeforeStateCommit(t *testing.T) {
	manager, err := healthcheck.NewManager("node-self", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()

	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site"},
		Spec: unboundedv1alpha3.SiteSpec{HealthCheckSettings: &unboundednetv1alpha1.HealthCheckSettings{
			TransmitInterval: ptrIntOrString(intstr.FromString("60s")),
		}},
	}
	siteInformer := newInformerWithObjects(toUnstructured(t, site))
	slices := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice"}, SiteName: "site",
		Nodes: []unboundednetv1alpha1.NodeInfo{{Name: "peer", WireGuardPublicKey: "pub-peer", InternalIPs: []string{"10.0.0.2"}, PodCIDRs: []string{"10.244.1.0/24"}}},
	}))
	stale := healthcheck.DefaultSettings()
	stale.TransmitInterval = time.Second
	state := &wireGuardState{
		clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-self"}}),
		nodeName:  "node-self", healthCheckManager: manager,
		healthCheckProfiles: map[string]healthcheck.HealthCheckSettings{"s-site": stale},
	}

	original := configureWireGuardFunc

	defer func() { configureWireGuardFunc = original }()

	called := false
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, gateways []gatewayPeerInfo, siteName string, _, _, _ map[string]bool,
		siteNames, peeringNames, assignmentSites, assignmentPools, poolNames map[string]string, _, _, _, _, _ map[string]int,
		_ []unboundednetnetlink.DesiredRoute, _ map[string]bool, s *wireGuardState, profiles map[string]healthcheck.HealthCheckSettings,
	) error {
		called = true

		if s.healthCheckProfiles["s-site"] != stale {
			t.Fatal("test must observe uncommitted state")
		}

		if profiles["s-site"].TransmitInterval != 60*time.Second {
			t.Fatal("fresh profile was not passed")
		}

		_, err := registerPeersWithHealthCheck(peers, gateways, siteName, false, siteNames, peeringNames, assignmentSites, assignmentPools, poolNames, profiles, s, func(gatewayPeerInfo) string { return "" }, false)

		return err
	}
	empty := newInformerWithObjects()

	err = updateWireGuardFromSlices(context.Background(), nil, siteInformer, slices, empty, empty, empty, empty, empty,
		&config{NodeName: "node-self", WireGuardPort: 51820}, "site", "private", "pub-self", true, state)
	if err != nil || !called {
		t.Fatalf("reconciliation: called=%t err=%v", called, err)
	}

	got, err := manager.GetPeerSettings("peer")
	if err != nil || got.TransmitInterval != 60*time.Second || got.ReceiveInterval != 15*time.Second {
		t.Fatalf("registration used stale state: %+v %v", got, err)
	}
}

func TestGatewayHealthTarget(t *testing.T) {
	routed := []string{"10.32.130.0/23"}
	v4Overlay := []string{"192.168.20.0/25"}

	cases := map[string]struct {
		peer       gatewayPeerInfo
		local      []string
		wantIP     string
		wantViaDev string
	}{
		"overlay address from pod CIDR": {
			peer:   gatewayPeerInfo{InternalIPs: []string{"10.0.0.4"}, PodCIDRs: []string{"10.244.1.0/24"}},
			wantIP: "10.244.1.1",
		},
		"unmanaged gateway probed at its IPv4 address through the tunnel": {
			peer:       gatewayPeerInfo{InternalIPs: []string{"fd00::4", "10.0.0.4"}, RoutedCidrs: routed, UnmanagedCNISite: true},
			local:      v4Overlay,
			wantIP:     "10.0.0.4",
			wantViaDev: unbounded0DeviceName,
		},
		"unmanaged IPv6-only gateway is not probed": {
			peer:  gatewayPeerInfo{InternalIPs: []string{"fd00::4"}, RoutedCidrs: routed, UnmanagedCNISite: true},
			local: v4Overlay,
		},
		"no IPv4 overlay address on this node to probe from": {
			peer:  gatewayPeerInfo{InternalIPs: []string{"10.0.0.4"}, RoutedCidrs: routed, UnmanagedCNISite: true},
			local: []string{"fd10::/64"},
		},
		"managed gateway still waiting for its pod CIDR": {
			peer:  gatewayPeerInfo{InternalIPs: []string{"10.0.0.4"}, RoutedCidrs: routed},
			local: v4Overlay,
		},
		"unmanaged gateway over WireGuard": {
			peer:  gatewayPeerInfo{InternalIPs: []string{"10.0.0.4"}, RoutedCidrs: routed, UnmanagedCNISite: true, TunnelProtocol: "WireGuard"},
			local: v4Overlay,
		},
		"unmanaged gateway without routed CIDRs": {
			peer:  gatewayPeerInfo{InternalIPs: []string{"10.0.0.4"}, UnmanagedCNISite: true},
			local: v4Overlay,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ip, via := gatewayHealthTarget(tc.peer, tc.local)
			if ip != tc.wantIP || via != tc.wantViaDev {
				t.Fatalf("gatewayHealthTarget() = (%q, %q), want (%q, %q)", ip, via, tc.wantIP, tc.wantViaDev)
			}
		})
	}
}

// TestGatewayProbePins checks that the probe target gets exactly one BPF
// entry: pinned when localCidrs exclude it, and not added a second time when
// gatewayBPFPrefixes already has it (two nexthops would be health-filtered).
func TestGatewayProbePins(t *testing.T) {
	peer := gatewayPeerInfo{InternalIPs: []string{"10.0.0.4"}, RoutedCidrs: []string{"10.32.130.0/23"}, UnmanagedCNISite: true}
	local := []string{"192.168.20.0/25"}

	excluded := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/27")}
	if got := gatewayProbePins(peer, excluded, local); len(got) != 1 || got[0] != "10.0.0.4/32" {
		t.Fatalf("pins with the gateway IP in localCidrs = %v, want [10.0.0.4/32]", got)
	}

	if got := gatewayProbePins(peer, nil, local); len(got) != 0 {
		t.Fatalf("pins without localCidrs = %v, want none (gatewayBPFPrefixes has the /32)", got)
	}

	managed := peer
	managed.UnmanagedCNISite = false

	if got := gatewayProbePins(managed, excluded, local); len(got) != 0 {
		t.Fatalf("pins for a managed gateway = %v, want none", got)
	}
}

// TestRegisterUnmanagedGatewayHealthCheck covers a gateway in a Site with
// manageCniPlugin=false: it has no pod CIDR, and used to be skipped, so its
// BPF entries were never health-checked or failed over.
func TestRegisterUnmanagedGatewayHealthCheck(t *testing.T) {
	manager, err := healthcheck.NewManager("local", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop()

	state := &wireGuardState{
		healthCheckManager:            manager,
		meshPeerHealthCheckEnabled:    make(map[string]bool),
		gatewayPeerHealthCheckEnabled: make(map[string]bool),
		nodePodCIDRs:                  []string{"192.168.20.0/25"},
	}
	gateways := []gatewayPeerInfo{{
		Name:             "gw",
		PoolName:         "pool",
		InternalIPs:      []string{"10.0.0.4"},
		RoutedCidrs:      []string{"10.32.130.0/23"},
		UnmanagedCNISite: true,
	}}

	desired, err := registerPeersWithHealthCheck(nil, gateways, "remote", false, map[string]string{"remote": "site"}, nil, nil, nil, nil,
		map[string]healthcheck.HealthCheckSettings{"site": healthcheck.DefaultSettings()}, state,
		func(gatewayPeerInfo) string { return "geneve0" }, true)
	if err != nil {
		t.Fatal(err)
	}

	if !desired["gw"] {
		t.Fatalf("unmanaged gateway was not registered for health checks: %v", desired)
	}

	if _, err := manager.GetPeerSettings("gw"); err != nil {
		t.Fatalf("no health check session for the unmanaged gateway: %v", err)
	}
}
