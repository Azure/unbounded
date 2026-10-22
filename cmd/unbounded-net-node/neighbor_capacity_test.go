// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestMeshNeighborEndpoints(t *testing.T) {
	peers := []meshPeerInfo{
		{SiteName: "local", TunnelProtocol: "GENEVE", InternalIPs: []string{"192.0.2.1", "2001:db8::1"}},
		{SiteName: "peered", TunnelProtocol: "WireGuard", InternalIPs: []string{"192.0.2.2", "2001:db8::2"}},
		{SiteName: "unpeered", TunnelProtocol: "WireGuard", InternalIPs: []string{"192.0.2.3", "2001:db8::3"}},
		{SiteName: "local", TunnelProtocol: "None"},
	}
	gateways := []gatewayPeerInfo{
		{SiteName: "local", PoolType: "External", TunnelProtocol: "WireGuard", InternalIPs: []string{"192.0.2.4", "2001:db8::4"}},
		{SiteName: "peered", PoolType: "External", TunnelProtocol: "WireGuard", InternalIPs: []string{"192.0.2.5", "2001:db8::5"}},
		{
			SiteName: "remote", PoolType: "External", TunnelProtocol: "WireGuard",
			InternalIPs: []string{"10.0.0.6", "fd00::6"}, ExternalIPs: []string{"192.0.2.6", "2001:db8::6"},
		},
		{SiteName: "remote", PoolType: "Internal", TunnelProtocol: "WireGuard", InternalIPs: []string{"192.0.2.7", "2001:db8::7"}},
		{SiteName: "remote", PoolType: "Internal", TunnelProtocol: "GENEVE", InternalIPs: []string{"192.0.2.8", "2001:db8::8"}},
	}

	for _, family := range []string{"IPv4", "IPv6"} {
		t.Run(family, func(t *testing.T) {
			got := meshNeighborEndpoints(&config{TunnelIPFamily: family}, peers, gateways, "local",
				map[string]bool{"local": true, "peered": true}, map[string]bool{"local": true, "peered": true})

			var addresses, want []string
			for _, ip := range got {
				addresses = append(addresses, ip.String())
			}

			for _, suffix := range []int{1, 2, 4, 5, 6, 8} {
				ip := fmt.Sprintf("192.0.2.%d", suffix)
				if family == "IPv6" {
					ip = fmt.Sprintf("2001:db8::%d", suffix)
				}

				want = append(want, ip)
			}

			if !slices.Equal(addresses, want) {
				t.Fatalf("endpoints=%v, want %v", addresses, want)
			}
		})
	}
}

type fakeNeighborPeerSource struct {
	peers []wgtypes.Peer
	err   error
}

func (s fakeNeighborPeerSource) GetPeers() ([]wgtypes.Peer, error) {
	return s.peers, s.err
}

func TestLiveWireGuardNeighborEndpoints(t *testing.T) {
	meshKey, gatewayKey, staleKey := wgtypes.Key{1}, wgtypes.Key{2}, wgtypes.Key{3}
	peers := []meshPeerInfo{{WireGuardPublicKey: meshKey.String(), TunnelProtocol: "WireGuard"}}
	gateways := []gatewayPeerInfo{{WireGuardPublicKey: gatewayKey.String(), TunnelProtocol: "WireGuard"}}
	sources := map[string]neighborWireGuardPeerSource{
		"wg51820": fakeNeighborPeerSource{peers: []wgtypes.Peer{
			{PublicKey: meshKey, Endpoint: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 51820}},
			{PublicKey: staleKey, Endpoint: &net.UDPAddr{IP: net.ParseIP("192.0.2.3"), Port: 51820}},
			{PublicKey: gatewayKey},
		}},
		"wg51821": fakeNeighborPeerSource{peers: []wgtypes.Peer{
			{PublicKey: gatewayKey, Endpoint: &net.UDPAddr{IP: net.ParseIP("2001:db8::2"), Port: 51821}},
		}},
	}

	endpoints, err := liveWireGuardNeighborEndpoints(peers, gateways, sources)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, ip := range endpoints {
		got = append(got, ip.String())
	}

	slices.Sort(got)

	if !slices.Equal(got, []string{"192.0.2.1", "2001:db8::2"}) {
		t.Fatalf("live endpoints=%v", got)
	}

	for _, tc := range []struct {
		name      string
		err       error
		wantError bool
	}{
		{name: "removed interface", err: unix.ENODEV},
		{name: "not yet created interface", err: os.ErrNotExist},
		{name: "permission failure", err: os.ErrPermission, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := map[string]neighborWireGuardPeerSource{"wg51820": fakeNeighborPeerSource{err: tc.err}}

			endpoints, err := liveWireGuardNeighborEndpoints(peers, gateways, sources)
			if len(endpoints) != 0 || (err != nil) != tc.wantError {
				t.Fatalf("endpoints=%v error=%v", endpoints, err)
			}

			if tc.wantError && (!errors.Is(err, tc.err) || !strings.Contains(err.Error(), "wg51820")) {
				t.Fatalf("error lost interface or cause: %v", err)
			}

			if _, err := liveWireGuardNeighborEndpoints(nil, nil, sources); err != nil {
				t.Fatalf("no WireGuard peers should require no device reads: %v", err)
			}
		})
	}
}

func TestMeshNeighborCounts(t *testing.T) {
	direct := func(link int) netlink.Route {
		return netlink.Route{Type: unix.RTN_UNICAST, LinkIndex: link}
	}
	routed := func(link int, gateway string) netlink.Route {
		route := direct(link)
		route.Gw = net.ParseIP(gateway)

		return route
	}

	for _, tc := range []struct {
		name      string
		endpoints []string
		routes    map[string][]netlink.Route
		want      [2]int
	}{
		{name: "empty"},
		{
			name:      "dual stack directly connected peers",
			endpoints: []string{"192.0.2.1", "192.0.2.2", "2001:db8::1"},
			routes: map[string][]netlink.Route{
				"192.0.2.1": {direct(1)}, "192.0.2.2": {direct(1)}, "2001:db8::1": {direct(1)},
			},
			want: [2]int{2, 1},
		},
		{
			name:      "deduplicate advertised endpoint and mapped IPv4",
			endpoints: []string{"192.0.2.1", "::ffff:192.0.2.1", "192.0.2.1"},
			routes:    map[string][]netlink.Route{"192.0.2.1": {direct(1)}},
			want:      [2]int{1, 0},
		},
		{
			name:      "many remote peers share router",
			endpoints: []string{"198.51.100.1", "198.51.100.2", "192.0.2.254", "2001:db8::1", "2001:db8::2"},
			routes: map[string][]netlink.Route{
				"198.51.100.1": {routed(1, "192.0.2.254")}, "198.51.100.2": {routed(1, "192.0.2.254")},
				"192.0.2.254": {direct(1)},
				"2001:db8::1": {routed(1, "fe80::1")}, "2001:db8::2": {routed(1, "fe80::1")},
			},
			want: [2]int{1, 1},
		},
		{
			name:      "multipath counts unique address interface pairs",
			endpoints: []string{"192.0.2.1", "192.0.2.2"},
			routes: map[string][]netlink.Route{
				"192.0.2.1": {{Type: unix.RTN_UNICAST, MultiPath: []*netlink.NexthopInfo{
					{LinkIndex: 1, Gw: net.ParseIP("192.0.2.254")},
					{LinkIndex: 2, Gw: net.ParseIP("192.0.2.254")},
					{LinkIndex: 1, Gw: net.ParseIP("192.0.2.253"), Flags: unix.RTNH_F_DEAD},
					{LinkIndex: 2, Gw: net.ParseIP("192.0.2.253"), Flags: unix.RTNH_F_LINKDOWN},
				}}},
				"192.0.2.2": {routed(1, "192.0.2.254")},
			},
			want: [2]int{2, 0},
		},
		{
			name:      "onlink gateway flag still resolves gateway not destination",
			endpoints: []string{"198.51.100.1", "198.51.100.2"},
			routes: map[string][]netlink.Route{
				"198.51.100.1": {{Type: unix.RTN_UNICAST, LinkIndex: 1, Gw: net.ParseIP("192.0.2.254"), Flags: unix.RTNH_F_ONLINK}},
				"198.51.100.2": {routed(1, "192.0.2.254")},
			},
			want: [2]int{1, 0},
		},
		{
			name:      "no neighbors on local or noarp or managed tunnel routes",
			endpoints: []string{"127.0.0.1", "192.0.2.1", "192.0.2.2", "192.0.2.3"},
			routes: map[string][]netlink.Route{
				"127.0.0.1": {{Type: unix.RTN_LOCAL, LinkIndex: 3}},
				"192.0.2.1": {direct(3)}, "192.0.2.2": {direct(4)}, "192.0.2.3": {direct(5)},
			},
		},
		{
			name:      "cross family gateway uses gateway neighbor table",
			endpoints: []string{"192.0.2.1"},
			routes: map[string][]netlink.Route{
				"192.0.2.1": {{Type: unix.RTN_UNICAST, LinkIndex: 1, Via: &netlink.Via{AddrFamily: unix.AF_INET6, Addr: net.ParseIP("fe80::1")}}},
			},
			want: [2]int{0, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tuner := newNeighborGCTuner()
			calls := make(map[string]int)
			tuner.routeGet = func(ip net.IP) ([]netlink.Route, error) {
				calls[ip.String()]++
				if calls[ip.String()] > 1 {
					t.Fatalf("duplicate route query for %s", ip)
				}

				routes, ok := tc.routes[ip.String()]
				if !ok {
					t.Fatalf("unexpected endpoint %s", ip)
				}

				return routes, nil
			}
			tuner.linkByIndex = func(index int) (netlink.Link, error) {
				attrs := netlink.LinkAttrs{Index: index, Name: fmt.Sprintf("eth%d", index)}
				switch index {
				case 3:
					attrs.Flags = net.FlagLoopback
				case 4:
					attrs.RawFlags = unix.IFF_NOARP
				case 5:
					attrs.Name = unbounded0DeviceName
				}

				return &netlink.Dummy{LinkAttrs: attrs}, nil
			}

			var endpoints []net.IP
			for _, ip := range tc.endpoints {
				endpoints = append(endpoints, net.ParseIP(ip))
			}

			got, err := tuner.countNeighbors(&config{WireGuardInterfacePrefix: "wg"}, endpoints)
			if err != nil || got != tc.want {
				t.Fatalf("counts=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestMeshNeighborResolutionErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		endpoint  net.IP
		routes    []netlink.Route
		routeErr  error
		linkErr   error
		wantError string
	}{
		{name: "invalid endpoint", wantError: "invalid mesh endpoint"},
		{name: "route failure", endpoint: net.ParseIP("192.0.2.1"), routeErr: os.ErrPermission, wantError: "resolve neighbor route"},
		{name: "empty lookup", endpoint: net.ParseIP("192.0.2.1"), wantError: "no neighbor route"},
		{name: "missing interface", endpoint: net.ParseIP("192.0.2.1"), routes: []netlink.Route{{Type: unix.RTN_UNICAST}}, wantError: "no egress interface"},
		{
			name: "link failure", endpoint: net.ParseIP("192.0.2.1"), routes: []netlink.Route{{Type: unix.RTN_UNICAST, LinkIndex: 1}},
			linkErr: os.ErrPermission, wantError: "resolve neighbor egress interface",
		},
		{name: "unreachable network", endpoint: net.ParseIP("192.0.2.1"), routeErr: unix.ENETUNREACH},
		{name: "unreachable host", endpoint: net.ParseIP("192.0.2.1"), routeErr: unix.EHOSTUNREACH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tuner := newNeighborGCTuner()
			tuner.routeGet = func(net.IP) ([]netlink.Route, error) { return tc.routes, tc.routeErr }
			tuner.linkByIndex = func(int) (netlink.Link, error) { return nil, tc.linkErr }
			tuner.readFile = func(string) ([]byte, error) {
				t.Fatal("resolution failure or unreachable endpoint must not read sysctls")
				return nil, nil
			}
			tuner.writeFile = func(string, []byte, os.FileMode) error {
				t.Fatal("resolution failure must not write sysctls")
				return nil
			}

			err := tuner.reconcile(&config{}, []net.IP{tc.endpoint})
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}

			if tc.routeErr != nil && tc.wantError != "" && !errors.Is(err, tc.routeErr) {
				t.Fatalf("route error not preserved: %v", err)
			}
		})
	}
}

func TestAutomaticNeighborSizingLargeDualStack(t *testing.T) {
	tuner := newNeighborGCTuner()
	values := fakeNeighborSysctls(t, tuner)
	tuner.routeGet = func(net.IP) ([]netlink.Route, error) {
		return []netlink.Route{{Type: unix.RTN_UNICAST, LinkIndex: 1}}, nil
	}
	tuner.linkByIndex = func(int) (netlink.Link, error) {
		return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "eth0"}}, nil
	}

	var endpoints []net.IP
	for i := range 2000 {
		endpoints = append(endpoints, net.IPv4(10, byte(i>>8), byte(i), 1))
	}

	for i := range 1000 {
		endpoints = append(endpoints, net.ParseIP(fmt.Sprintf("2001:db8::%x", i+1)))
	}

	if err := tuner.reconcile(&config{WireGuardInterfacePrefix: "wg"}, endpoints); err != nil {
		t.Fatal(err)
	}

	for family, want := range map[string][3]int{
		"ipv4": {2200, 2400, 2600},
		"ipv6": {1100, 1200, 1300},
	} {
		for i, threshold := range want {
			path := fmt.Sprintf("/proc/sys/net/%s/neigh/default/gc_thresh%d", family, i+1)
			if values[path] != threshold {
				t.Fatalf("%s=%d, want %d", path, values[path], threshold)
			}
		}
	}
}

func TestAutomaticNeighborSizingReconcilesWithoutPeerChanges(t *testing.T) {
	cfg := &config{
		NodeName: "self", WireGuardPort: 51820, WireGuardInterfacePrefix: "wg",
		TunnelIPFamily: "IPv4", PreferredPrivateEncap: "WireGuard", MTU: 576,
	}
	sites := newInformerWithObjects(toUnstructured(t, &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "local"},
	}))

	slice := &unboundednetv1alpha1.SiteNodeSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice"}, SiteName: "local",
	}
	for i := range 3 {
		slice.Nodes = append(slice.Nodes, unboundednetv1alpha1.NodeInfo{
			Name: fmt.Sprintf("peer-%d", i), WireGuardPublicKey: fmt.Sprintf("pub-%d", i),
			InternalIPs: []string{fmt.Sprintf("192.0.2.%d", i+1)},
			PodCIDRs:    []string{fmt.Sprintf("10.244.%d.0/24", i)},
		})
	}

	slicesInformer := newInformerWithObjects(toUnstructured(t, slice))
	empty := newInformerWithObjects()
	tuner := newNeighborGCTuner()
	values := fakeNeighborSysctls(t, tuner)
	onLink := false
	tuner.routeGet = func(net.IP) ([]netlink.Route, error) {
		route := netlink.Route{Type: unix.RTN_UNICAST, LinkIndex: 1}
		if !onLink {
			route.Gw = net.ParseIP("192.0.2.254")
		}

		return []netlink.Route{route}, nil
	}
	tuner.linkByIndex = func(int) (netlink.Link, error) {
		return &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "eth0"}}, nil
	}
	state := &wireGuardState{
		nodeName: "self", neighborGCTuner: tuner,
		clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
	}
	configureCalls := 0
	original := configureWireGuardFunc
	configureWireGuardFunc = func(context.Context, *config, string, []meshPeerInfo, []gatewayPeerInfo, string, map[string]bool, map[string]bool, map[string]bool, map[string]string, map[string]string, map[string]string, map[string]string, map[string]string, map[string]int, map[string]int, map[string]int, map[string]int, map[string]int, []unboundednetnetlink.DesiredRoute, map[string]bool, *wireGuardState, map[string]healthcheck.HealthCheckSettings) error {
		configureCalls++
		return nil
	}

	t.Cleanup(func() { configureWireGuardFunc = original })

	update := func() error {
		return updateWireGuardFromSlices(t.Context(), nil, sites, slicesInformer, empty, empty, empty, empty, empty,
			cfg, "local", "private", "pub-self", true, state)
	}
	check := func(want [3]int) {
		t.Helper()

		for i := range 3 {
			path := fmt.Sprintf("/proc/sys/net/ipv4/neigh/default/gc_thresh%d", i+1)
			if values[path] != want[i] {
				t.Fatalf("%s=%d, want %d", path, values[path], want[i])
			}

			path = fmt.Sprintf("/proc/sys/net/ipv6/neigh/default/gc_thresh%d", i+1)
			if values[path] != 1 {
				t.Fatalf("IPv4 endpoints modified IPv6 threshold %s", path)
			}
		}
	}

	if err := update(); err != nil {
		t.Fatal(err)
	}

	check([3]int{2, 2, 2})

	onLink = true
	write := tuner.writeFile
	tuner.writeFile = func(string, []byte, os.FileMode) error { return os.ErrPermission }

	if err := update(); !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "reconcile mesh neighbor capacity") {
		t.Fatalf("sysctl failure not surfaced: %v", err)
	}

	check([3]int{2, 2, 2})

	tuner.writeFile = write

	if err := update(); err != nil {
		t.Fatalf("route-only change was not retried: %v", err)
	}

	check([3]int{4, 4, 4})

	if configureCalls != 1 {
		t.Fatalf("route-only updates unnecessarily reconfigured peers: calls=%d", configureCalls)
	}

	onLink = false

	if err := update(); err != nil {
		t.Fatal(err)
	}

	check([3]int{4, 4, 4})

	onLink = true

	slice.Nodes = append(slice.Nodes, unboundednetv1alpha1.NodeInfo{
		Name: "new-peer", WireGuardPublicKey: "pub-new", InternalIPs: []string{"192.0.2.4"}, PodCIDRs: []string{"10.244.4.0/24"},
	})
	if err := slicesInformer.GetStore().Update(toUnstructured(t, slice)); err != nil {
		t.Fatal(err)
	}

	tuner.writeFile = func(string, []byte, os.FileMode) error { return os.ErrPermission }

	if err := update(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("new peer sizing failure not surfaced: %v", err)
	}

	if configureCalls != 1 {
		t.Fatalf("peer configuration ran after sizing failure: calls=%d", configureCalls)
	}

	tuner.writeFile = write

	if err := update(); err != nil {
		t.Fatal(err)
	}

	check([3]int{5, 5, 6})

	slice.Nodes = slice.Nodes[:1]
	if err := slicesInformer.GetStore().Update(toUnstructured(t, slice)); err != nil {
		t.Fatal(err)
	}

	if err := update(); err != nil {
		t.Fatal(err)
	}

	check([3]int{5, 5, 6})
}

func fakeNeighborSysctls(t *testing.T, tuner *neighborGCTuner) map[string]int {
	t.Helper()

	values := make(map[string]int)

	for _, family := range []string{"ipv4", "ipv6"} {
		for i := range 3 {
			values[fmt.Sprintf("/proc/sys/net/%s/neigh/default/gc_thresh%d", family, i+1)] = 1
		}
	}

	tuner.readFile = func(path string) ([]byte, error) {
		value, ok := values[path]
		if !ok {
			t.Fatalf("unexpected sysctl read %s", path)
		}

		return []byte(strconv.Itoa(value)), nil
	}
	tuner.writeFile = func(path string, data []byte, _ os.FileMode) error {
		old, ok := values[path]
		if !ok {
			t.Fatalf("unexpected sysctl write %s", path)
		}

		value, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || value <= old {
			t.Fatalf("invalid increase: %s = %q (was %d), err=%v", path, data, old, err)
		}

		values[path] = value

		return nil
	}

	return values
}
