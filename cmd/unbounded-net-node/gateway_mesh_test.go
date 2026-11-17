// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1 "github.com/Azure/unbounded/api/net/v1alpha1"
	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
	"github.com/Azure/unbounded/internal/net/healthcheck"
	netlinkpkg "github.com/Azure/unbounded/internal/net/netlink"
)

func TestGatewayBPFRouteDistances(t *testing.T) {
	t.Parallel()

	entries := make(map[string]ebpfpkg.TunnelEndpoint)
	peer := gatewayPeerInfo{
		Name: "gateway",
		RouteDistances: map[string]int{
			"10.0.0.0/8":   4,
			"10.20.0.0/16": 2,
		},
	}
	addGatewayBPFEntries(entries, []string{"10.20.1.0/24", "10.20.1.5/32", "10.30.0.0/16", "192.0.2.0/24"},
		peer, net.ParseIP("192.0.2.5"), 0, 7, ebpfpkg.TunnelProtoWireGuard)

	for prefix, distance := range map[string]int{"10.20.1.0/24": 2, "10.20.1.5/32": 2, "10.30.0.0/16": 4, "192.0.2.0/24": 1} {
		if got := entries[prefix].Nexthops[0].RouteDistance; got != distance {
			t.Errorf("%s: got distance %d, want %d", prefix, got, distance)
		}
	}
}

func TestDirectBPFRouteAddressFamilies(t *testing.T) {
	t.Parallel()

	for _, internalIPs := range [][]string{{"192.0.2.2", "2001:db8::2"}, {"192.0.2.2"}, {"2001:db8::2"}} {
		for _, kind := range []string{"mesh", "assignment", "gateway"} {
			t.Run(fmt.Sprintf("%v/%s", internalIPs, kind), func(t *testing.T) {
				entries := make(map[string]ebpfpkg.TunnelEndpoint)

				prefixes := []string{"10.246.1.0/24", "fd46:0:0:1::/64"}
				if kind == "gateway" {
					addGatewayBPFEntries(entries, prefixes,
						gatewayPeerInfo{Name: "peer", InternalIPs: internalIPs, RouteDistances: map[string]int{prefixes[0]: 2, prefixes[1]: 3}},
						net.ParseIP("192.0.2.2"), 0, 7, ebpfpkg.TunnelProtoNone)
				} else {
					peer := meshPeerInfo{Name: "peer", InternalIPs: internalIPs, PodCIDRs: prefixes}
					if kind == "assignment" {
						peer.TransitPoolName = "pool"
					}

					direct, assignment := make(map[string]ebpfpkg.TunnelEndpoint), make(map[string]ebpfpkg.TunnelEndpoint)
					addMeshPeerBPFEntries(entries, direct, assignment, peer, net.ParseIP("192.0.2.2"), 0, 7, ebpfpkg.TunnelProtoNone)

					entries = direct
					if kind == "assignment" {
						entries = assignment
					}
				}

				for index, family := range []string{"IPv4", "IPv6"} {
					want := selectUnderlayIP(internalIPs, family)

					next := entries[prefixes[index]].Nexthops
					if want == nil {
						if len(next) != 0 {
							t.Fatalf("programmed %s without a matching underlay: %+v", family, next)
						}

						continue
					}

					if len(next) != 1 || !next[0].RemoteIP.Equal(want) {
						t.Fatalf("%s nexthop: got %+v, want %s", family, next, want)
					}

					if kind == "gateway" && next[0].RouteDistance != index+2 {
						t.Fatalf("lost route distance: %+v", next)
					}
				}
			})
		}
	}
}

func TestGatewayPromotionPreservesFullSiteMesh(t *testing.T) {
	for _, gatewaysPerSite := range []int{0, 1, 2} {
		for localNode := range 6 {
			t.Run(fmt.Sprintf("gateways-%d/node-%d", gatewaysPerSite, localNode), func(t *testing.T) {
				var sites, slices, pools []*unstructured.Unstructured

				for site := range 3 {
					name := fmt.Sprintf("site-%d", site)
					sites = append(sites, toUnstructured(t, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}))
					slice := netv1.SiteNodeSlice{ObjectMeta: metav1.ObjectMeta{Name: name}, SiteName: name}
					pool := netv1.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: netv1.GatewayPoolSpec{Type: "Internal"}}

					for node := range 2 {
						id := 2*site + node
						info := netv1.NodeInfo{
							Name: fmt.Sprintf("node-%d", id), WireGuardPublicKey: fmt.Sprintf("key-%d", id),
							InternalIPs: []string{fmt.Sprintf("10.0.%d.%d", site, node+10)},
							PodCIDRs:    []string{fmt.Sprintf("10.244.%d.0/24", id)},
						}

						slice.Nodes = append(slice.Nodes, info)
						if node < gatewaysPerSite {
							pool.Status.Nodes = append(pool.Status.Nodes, netv1.GatewayNodeInfo{
								Name: info.Name, SiteName: name, WireGuardPublicKey: info.WireGuardPublicKey,
								InternalIPs: info.InternalIPs, PodCIDRs: info.PodCIDRs, GatewayWireguardPort: int32(51821 + id),
							})
						}
					}

					slices = append(slices, toUnstructured(t, &slice))
					pools = append(pools, toUnstructured(t, &pool))
				}

				name := fmt.Sprintf("node-%d", localNode)
				state := &wireGuardState{
					nodeName: name, gatewayTaintApplied: true,
					clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}),
				}
				cfg := &config{NodeName: name, WireGuardPort: 51820, PreferredPrivateEncap: "WireGuard"}
				empty := newInformerWithObjects()
				peerings := newInformerWithObjects(toUnstructured(t, &netv1.SitePeering{
					ObjectMeta: metav1.ObjectMeta{Name: "full-mesh"}, Spec: netv1.SitePeeringSpec{Sites: []string{"site-0", "site-1", "site-2"}},
				}))

				var got []meshPeerInfo

				original := configureWireGuardFunc
				configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, gateways []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []netlinkpkg.DesiredRoute, _ map[string]bool, _ *wireGuardState, _ map[string]healthcheck.HealthCheckSettings) error {
					got = peers

					if len(gateways) != 0 {
						t.Fatalf("unassigned gateway identities created transit peers: %v", gateways)
					}

					return nil
				}

				t.Cleanup(func() { configureWireGuardFunc = original })

				if err := updateWireGuardFromSlices(context.Background(), nil, newInformerWithObjects(sites...), newInformerWithObjects(slices...), newInformerWithObjects(pools...), empty, peerings, empty, empty,
					cfg, fmt.Sprintf("site-%d", localNode/2), "private", fmt.Sprintf("key-%d", localNode), true, state); err != nil {
					t.Fatal(err)
				}

				if len(got) != 5 {
					t.Fatalf("gateway selection changed mesh membership: got %d peers: %+v", len(got), got)
				}

				for _, peer := range got {
					if peer.Name == name || peer.TransitPoolName != "" || peer.TunnelProtocol != "WireGuard" {
						t.Fatalf("ordinary slice peer changed purpose: %+v", peer)
					}
				}
			})
		}
	}
}

func TestDirectMeshPrefixWinsAcrossProtocolPhases(t *testing.T) {
	for _, directFirst := range []bool{false, true} {
		state := &wireGuardState{
			pendingBPFEntries:       make(map[string]ebpfpkg.TunnelEndpoint),
			pendingDirectBPFEntries: make(map[string]ebpfpkg.TunnelEndpoint),
		}
		direct := meshPeerInfo{Name: "gateway-mesh", PodCIDRs: []string{"10.244.1.0/24"}, InternalIPs: []string{"10.0.0.10"}}
		transit := gatewayPeerInfo{RoutedCidrs: []string{"10.244.1.0/24", "10.244.1.128/25", "10.245.0.0/16"}, PodCIDRs: []string{"10.246.1.0/24"}}

		addDirect := func() {
			addMeshPeerBPFEntries(state.pendingBPFEntries, state.pendingDirectBPFEntries, nil, direct, net.ParseIP("10.0.0.10"), 0, 42, ebpfpkg.TunnelProtoWireGuard)
		}
		if directFirst {
			addDirect()
		}

		for _, name := range []string{"gateway-transit-a", "gateway-transit-b"} {
			addPeerBPFEntries(state.pendingBPFEntries, gatewayBPFPrefixes(transit, nil), net.ParseIP("10.0.0.20"), 1, 43, ebpfpkg.TunnelProtoVXLAN, name, nil)
		}

		if !directFirst {
			addDirect()
		}

		entries := takePendingBPFEntries(state)
		if got := entries["10.244.1.0/24"].Nexthops; len(got) != 1 || got[0].PeerName != direct.Name || got[0].IfIndex != 42 {
			t.Fatalf("direct destination became transit ECMP: %+v", got)
		}

		if len(entries["10.245.0.0/16"].Nexthops) != 2 {
			t.Fatal("legitimate transit ECMP was lost")
		}

		if _, exists := entries["10.244.1.128/25"]; exists {
			t.Fatal("a more-specific transit advertisement overrode direct PodCIDR ownership")
		}

		if _, exists := entries["10.246.1.0/24"]; exists {
			t.Fatal("gateway own PodCIDR injected without slice membership")
		}

		if len(takePendingBPFEntries(state)) != 0 {
			t.Fatal("withdrawn topology retained pending entries")
		}
	}
}

func TestGatewayHostPinRequiresTransitAdvertisement(t *testing.T) {
	peer := gatewayPeerInfo{
		InternalIPs: []string{"10.0.0.10", "10.1.0.10"},
		PodCIDRs:    []string{"10.244.0.0/24"},
		RoutedCidrs: []string{"10.0.0.0/24"},
	}

	got := gatewayBPFPrefixes(peer, nil)
	if !reflect.DeepEqual(got, []string{"10.0.0.0/24", "10.0.0.10/32", "10.244.0.1/32"}) {
		t.Fatalf("gateway identity created unadvertised reachability: %v", got)
	}
}

func TestGatewayHealthWithoutOwnSiteAdvertisement(t *testing.T) {
	t.Parallel()

	peer := gatewayPeerInfo{
		Name: "gateway/transit/pool", InternalIPs: []string{"192.0.2.10", "2001:db8::10"},
		PodCIDRs: []string{"10.244.0.0/24", "fd44::/64"},
	}

	for _, advertised := range [][]string{{"10.245.0.0/16"}, {"10.244.0.0/24", "10.245.0.0/16"}, {"10.245.0.0/16"}} {
		peer.RoutedCidrs = advertised
		prefixes := gatewayBPFPrefixes(peer, nil)
		allowed := pruneCoveredAllowedCIDRs(prefixes)
		routes := collectSupernets(&wireGuardState{}, nil, []gatewayPeerInfo{peer}, []gatewayPeerInfo{peer}, nil, false)
		entries := make(map[string]ebpfpkg.TunnelEndpoint)
		addGatewayBPFEntries(entries, prefixes, peer, net.ParseIP("192.0.2.10"), 0, 7, ebpfpkg.TunnelProtoWireGuard)

		for _, cidr := range []string{"10.244.0.1/32", "fd44::1/128"} {
			endpoint := entries[cidr]
			if len(endpoint.Nexthops) != 1 || endpoint.Nexthops[0].PeerName != "" || !endpoint.Nexthops[0].Healthy {
				t.Fatalf("health probe depends on transit session health: %s: %+v", cidr, endpoint)
			}

			healthIP, _, _ := net.ParseCIDR(cidr)
			authorized := false

			for _, value := range allowed {
				_, prefix, err := net.ParseCIDR(value)
				if err == nil && prefix.Contains(healthIP) {
					authorized = true
				}
			}

			if !authorized {
				t.Fatalf("health endpoint %s missing from WireGuard AllowedIPs: %v", cidr, allowed)
			}

			covered := false

			for value := range routes {
				_, prefix, err := net.ParseCIDR(value)
				if err == nil && prefix.Contains(healthIP) {
					covered = true
				}
			}

			if !covered {
				t.Fatalf("health endpoint %s has no attraction route: %v", cidr, routes)
			}
		}

		if len(advertised) == 1 {
			if _, exists := entries["10.244.0.0/24"]; exists || routes["10.244.0.0/24"] {
				t.Fatal("health reachability restored the gateway's whole PodCIDR")
			}
		}

		if _, exists := entries["10.245.0.0/16"]; !exists {
			t.Fatal("health-only reachability removed other transit advertisements")
		}
	}
}

func TestAssignmentReturnMapPreservesDownlinkProtocols(t *testing.T) {
	for _, protocol := range []uint32{ebpfpkg.TunnelProtoWireGuard, ebpfpkg.TunnelProtoVXLAN, ebpfpkg.TunnelProtoGENEVE} {
		t.Run(fmt.Sprint(protocol), func(t *testing.T) {
			entries := make(map[string]ebpfpkg.TunnelEndpoint)
			directEntries := make(map[string]ebpfpkg.TunnelEndpoint)
			assignmentEntries := make(map[string]ebpfpkg.TunnelEndpoint)
			peer := meshPeerInfo{Name: "worker", PodCIDRs: []string{"10.244.1.0/24"}, InternalIPs: []string{"10.0.0.10", "fd00::10"}}
			addMeshPeerBPFEntries(entries, directEntries, assignmentEntries, peer, net.ParseIP("10.0.0.10"), 0, 42, ebpfpkg.TunnelProtoWireGuard)
			peer.Name = "worker/assignment/pool"
			peer.TransitPoolName = "pool"
			addMeshPeerBPFEntries(entries, directEntries, assignmentEntries, peer, net.ParseIP("10.0.0.10"), 1, 43, protocol)

			merged := takePendingBPFEntries(&wireGuardState{pendingBPFEntries: entries, pendingDirectBPFEntries: directEntries})
			if got := merged["10.244.1.0/24"].Nexthops; len(got) != 1 || got[0].PeerName != "worker" {
				t.Fatalf("local traffic lost ordinary mesh ownership: %+v", got)
			}

			if got := assignmentEntries["10.244.1.0/24"].Nexthops; len(got) != 1 || got[0].PeerName != peer.Name || got[0].Protocol != protocol || got[0].IfIndex != 43 {
				t.Fatalf("forwarded reply lost assignment downlink: %+v", got)
			}

			for _, prefix := range []string{"10.0.0.10/32", "fd00::10/128"} {
				if got := assignmentEntries[prefix].Nexthops; len(got) != 1 || got[0].PeerName != peer.Name || got[0].Protocol != protocol {
					t.Fatalf("node-IP transit return lost assignment downlink: %s: %+v", prefix, got)
				}

				if got := merged[prefix].Nexthops; len(got) != 1 || got[0].PeerName != "worker" {
					t.Fatalf("node-IP local traffic lost ordinary mesh ownership: %s: %+v", prefix, got)
				}
			}

			prefixes := assignmentReturnPrefixes(assignmentEntries, []meshPeerInfo{
				{Name: "ordinary", PodCIDRs: []string{"10.244.2.0/24"}},
				peer,
				{Name: "other/assignment/pool", TransitPoolName: "pool", PodCIDRs: []string{"fd00:1::/64"}},
				{Name: "unmanaged/assignment/pool", TransitPoolName: "pool", SkipPodCIDRRoutes: true, PodCIDRs: []string{"10.245.0.0/16"}},
			})
			if !reflect.DeepEqual(prefixes, []string{"10.0.0.10/32", "10.244.1.0/24", "10.245.0.0/16", "fd00:1::/64", "fd00::10/128"}) {
				t.Fatalf("return routes captured ordinary peers or lost an assignment: %v", prefixes)
			}

			peer.SkipPodCIDRRoutes = true
			unmanagedEntries := make(map[string]ebpfpkg.TunnelEndpoint)
			addMeshPeerBPFEntries(entries, directEntries, unmanagedEntries, peer, net.ParseIP("10.0.0.10"), 1, 43, protocol)

			if !reflect.DeepEqual(unmanagedEntries, assignmentEntries) {
				t.Fatal("external CNI ownership disabled transit return routes")
			}
		})
	}
}

func TestMeshWireGuardAcceptsNodeServiceReplies(t *testing.T) {
	t.Parallel()

	for _, transitPool := range []string{"", "pool"} {
		t.Run(transitPool, func(t *testing.T) {
			peer := meshPeerInfo{
				TransitPoolName: transitPool,
				PodCIDRs:        []string{"10.244.1.0/24", "fd00:1::/64", "invalid"},
				InternalIPs:     []string{"192.0.2.10", "2001:db8::10"},
			}

			if got := meshWireGuardAllowedIPs(peer); !reflect.DeepEqual(got, []string{
				"10.244.1.0/24", "192.0.2.10/32", "2001:db8::10/128", "fd00:1::/64",
			}) {
				t.Fatalf("pod or node service reply sources not authorized: %v", got)
			}
		})
	}
}

func TestAssignmentReturnRoutingSeparatesLocalAndForwardedTraffic(t *testing.T) {
	routes, rules := assignmentReturnPlan([]string{"10.244.1.0/24", "fd00:1::/64"}, 42, map[string]int{"wg51821": 42, "wg51820": 43, "vxlan0": 44})
	if len(routes) != 2 || len(rules) != 6 {
		t.Fatalf("incomplete dual-stack return plan: %v, %v", routes, rules)
	}

	for _, route := range routes {
		if route.Table == 0 || route.Nexthops[0].LinkIndex != 42 {
			t.Fatalf("assignment reply did not target dedicated interface: %+v", route)
		}
	}

	for _, rule := range rules {
		if rule.IifName == "" || rule.IifName == "lo" || rule.IifName == "cni0" || rule.Table != routes[0].Table {
			t.Fatalf("local pod/host traffic could bypass ordinary mesh: %+v", rule)
		}
	}

	originalList, originalAdd, originalDel := assignmentRuleList, assignmentRuleAdd, assignmentRuleDel

	t.Cleanup(func() {
		assignmentRuleList, assignmentRuleAdd, assignmentRuleDel = originalList, originalAdd, originalDel
	})

	var installed []netlink.Rule

	assignmentRuleList = func(family int) ([]netlink.Rule, error) {
		var result []netlink.Rule

		for _, rule := range installed {
			if rule.Family == family {
				result = append(result, rule)
			}
		}

		return result, nil
	}
	assignmentRuleAdd = func(rule *netlink.Rule) error { installed = append(installed, *rule); return nil }
	assignmentRuleDel = func(rule *netlink.Rule) error {
		for i, existing := range installed {
			if reflect.DeepEqual(existing, *rule) {
				installed = append(installed[:i], installed[i+1:]...)
				break
			}
		}

		return nil
	}

	if err := reconcileAssignmentReturnRules(rules); err != nil {
		t.Fatal(err)
	}

	if err := reconcileAssignmentReturnRules(rules); err != nil || len(installed) != len(rules) {
		t.Fatalf("assignment rules did not converge: %v, %v", err, installed)
	}

	if err := reconcileAssignmentReturnRules(nil); err != nil || len(installed) != 0 {
		t.Fatalf("assignment withdrawal retained return rules: %v, %v", err, installed)
	}
}

func TestAssignmentReturnRuleFailures(t *testing.T) {
	for _, operation := range []string{"list", "add", "delete"} {
		t.Run(operation, func(t *testing.T) {
			originalList, originalAdd, originalDel := assignmentRuleList, assignmentRuleAdd, assignmentRuleDel

			t.Cleanup(func() {
				assignmentRuleList, assignmentRuleAdd, assignmentRuleDel = originalList, originalAdd, originalDel
			})

			_, rules := assignmentReturnPlan([]string{"10.244.1.0/24"}, 42, map[string]int{"vxlan0": 43})
			installed := rules
			failure := errors.New("netlink failed")
			assignmentRuleList = func(family int) ([]netlink.Rule, error) {
				if operation == "list" {
					return nil, failure
				}

				if operation == "delete" && family == netlink.FAMILY_V4 {
					return installed, nil
				}

				return nil, nil
			}
			assignmentRuleAdd = func(_ *netlink.Rule) error { return failure }
			assignmentRuleDel = func(_ *netlink.Rule) error { return failure }

			if operation == "delete" {
				rules = nil
			}

			if err := reconcileAssignmentReturnRules(rules); !errors.Is(err, failure) {
				t.Fatalf("%s failure was hidden: %v", operation, err)
			}
		})
	}
}

func TestAssignmentReturnKernelRouteOwnership(t *testing.T) {
	originalList, originalDel := assignmentRouteList, assignmentRouteDel

	t.Cleanup(func() { assignmentRouteList, assignmentRouteDel = originalList, originalDel })

	_, wanted, _ := net.ParseCIDR("10.244.1.0/24")
	_, withdrawn, _ := net.ParseCIDR("10.244.2.0/24")
	_, withdrawnV6, _ := net.ParseCIDR("fd44:2::/64")
	owned := netlink.Route{Table: assignmentReturnTableBase + 1, LinkIndex: 42, Protocol: unix.RTPROT_STATIC, Dst: withdrawn}
	retained, foreignDevice, foreignTable, foreignProtocol, defaultRoute, ipv6 := owned, owned, owned, owned, owned, owned
	retained.Dst = wanted
	foreignDevice.LinkIndex = 43
	foreignTable.Table++
	foreignProtocol.Protocol = unix.RTPROT_BOOT
	defaultRoute.Dst = nil
	ipv6.Dst = withdrawnV6

	assignmentRouteList = func(family int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
		if filter.Table != owned.Table || filter.LinkIndex != 42 || filter.Protocol != unix.RTPROT_STATIC ||
			mask != netlink.RT_FILTER_TABLE|netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL {
			t.Fatalf("assignment scan was not scoped to its table, protocol, and device: %+v, %d", filter, mask)
		}

		if family == netlink.FAMILY_V6 {
			return []netlink.Route{ipv6}, nil
		}

		return []netlink.Route{retained, owned, foreignDevice, foreignTable, foreignProtocol, defaultRoute}, nil
	}

	var deleted []netlink.Route

	assignmentRouteDel = func(route *netlink.Route) error { deleted = append(deleted, *route); return nil }

	if err := reconcileAssignmentReturnKernelRoutes([]string{wanted.String()}, 42); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(deleted, []netlink.Route{owned, defaultRoute, ipv6}) {
		t.Fatalf("stale routes survived or unrelated kernel routes were removed: %+v", deleted)
	}

	failure := errors.New("netlink failed")
	assignmentRouteList = func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return nil, failure }

	deleted = nil
	if err := reconcileAssignmentReturnKernelRoutes(nil, 42); !errors.Is(err, failure) || len(deleted) != 0 {
		t.Fatalf("listing failure caused downstream deletion: %v, %+v", err, deleted)
	}

	assignmentRouteList = func(int, *netlink.Route, uint64) ([]netlink.Route, error) { return []netlink.Route{owned}, nil }
	assignmentRouteDel = func(*netlink.Route) error { return failure }

	if err := reconcileAssignmentReturnKernelRoutes(nil, 42); !errors.Is(err, failure) {
		t.Fatalf("route deletion failure was hidden: %v", err)
	}
}
