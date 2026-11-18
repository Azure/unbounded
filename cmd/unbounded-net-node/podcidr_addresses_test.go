// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
)

type recordingAddressManager struct {
	addresses []string
	removeAll bool
	calls     int
	err       error
}

func (m *recordingAddressManager) SyncAddresses(addresses []string, removeAll bool) (int, int, error) {
	m.addresses = slices.Clone(addresses)
	m.removeAll = removeAll
	m.calls++

	return 0, 0, m.err
}

func TestPodCIDRAddressOwnership(t *testing.T) {
	manager := &recordingAddressManager{}

	cidrs := []string{"10.244.1.0/24", "fd00:1::/64", "invalid"}
	for _, managed := range []bool{true, false} {
		if err := syncPodCIDRAddresses(manager, cidrs, managed); err != nil {
			t.Fatal(err)
		}

		var want []string
		if managed {
			want = []string{"10.244.1.1/32", "fd00:1::1/128"}
		}

		if !slices.Equal(manager.addresses, want) || manager.removeAll {
			t.Fatalf("managed=%t: addresses=%v removeAll=%t", managed, manager.addresses, manager.removeAll)
		}
	}

	if manager.calls != 2 {
		t.Fatal("delegating CNI ownership skipped stale address cleanup")
	}

	for _, cidrs := range [][]string{
		{"10.232.16.6/31", "10.232.16.8/29", "10.232.16.20/32", "fd00::7/128"},
		nil,
	} {
		if err := syncPodCIDRAddresses(manager, cidrs, false); err != nil || len(manager.addresses) != 0 {
			t.Fatalf("external CNI ranges claimed addresses: %v, %v", manager.addresses, err)
		}
	}

	injected := errors.New("address sync failed")

	manager.err = injected
	if err := syncPodCIDRAddresses(manager, nil, false); !errors.Is(err, injected) {
		t.Fatalf("cleanup error was lost: %v", err)
	}
}

type recordingSourceManager struct {
	ipv4, ipv6 net.IP
}

func (m *recordingSourceManager) SetPreferredSourceIPs(ipv4, ipv6 net.IP) {
	m.ipv4, m.ipv6 = ipv4, ipv6
}

func TestExternalCNIClearsPodCIDRPreferredSources(t *testing.T) {
	manager := &recordingSourceManager{}
	cidrs := []string{"10.244.1.0/24", "fd00:1::/64"}
	setPodCIDRPreferredSources(manager, cidrs, true)

	if !manager.ipv4.Equal(net.ParseIP("10.244.1.1")) || !manager.ipv6.Equal(net.ParseIP("fd00:1::1")) {
		t.Fatalf("managed sources changed: %v, %v", manager.ipv4, manager.ipv6)
	}

	setPodCIDRPreferredSources(manager, cidrs, false)

	if manager.ipv4 != nil || manager.ipv6 != nil {
		t.Fatalf("external CNI retained pod-derived sources: %v, %v", manager.ipv4, manager.ipv6)
	}
}

func TestExternalGatewayWireGuardAcceptsNodeHealthIPs(t *testing.T) {
	peer := gatewayPeerInfo{
		PodCIDRs: []string{"10.232.16.6/31"}, RoutedCidrs: []string{"10.232.16.0/20"},
		InternalIPs: []string{"10.232.0.4", "fd00::4"}, UnmanagedCNISite: true,
	}
	if got := gatewayWireGuardAllowedIPs(peer, nil); !slices.Equal(got, []string{"10.232.0.4/32", "10.232.16.0/20", "fd00::4/128"}) {
		t.Fatalf("node health IPs missing from WireGuard AllowedIPs: %v", got)
	}

	if got := gatewayWireGuardAllowedIPs(peer, []netip.Prefix{netip.MustParsePrefix("10.232.0.0/24"), netip.MustParsePrefix("fd00::/64")}); !slices.Equal(got, []string{"10.232.16.0/20"}) {
		t.Fatalf("node health IPs bypassed explicit local CIDR exclusions: %v", got)
	}
}

func TestPeeredMeshWireGuardAcceptsExternalCNINodeSources(t *testing.T) {
	for _, unmanaged := range []bool{false, true} {
		peer := meshPeerInfo{
			PodCIDRs:    []string{"10.232.16.6/31", "fd00:1::/64", "invalid"},
			InternalIPs: []string{"10.232.0.4", "fd00::4"}, UnmanagedCNISite: unmanaged,
		}

		want := []string{"10.232.16.6/31", "fd00:1::/64"}
		if unmanaged {
			want = append(want, "10.232.0.4/32", "fd00::4/128")
		}

		slices.Sort(want)

		if got := meshWireGuardAllowedIPs(peer, true); !slices.Equal(got, want) {
			t.Fatalf("unmanaged=%t: peered mesh authorization=%v want=%v", unmanaged, got, want)
		}

		if got := meshWireGuardAllowedIPs(peer, false); !slices.Contains(got, "10.232.0.4/32") || !slices.Contains(got, "fd00::4/128") {
			t.Fatalf("non-peered node source authorization regressed: %v", got)
		}
	}
}
