// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net"
	"testing"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
)

// TestAddPeerBPFEntries_MultiNexthopForSameCIDR confirms that the
// ECMP-eligible programming path appends nexthops for repeated CIDRs.
func TestAddPeerBPFEntries_MultiNexthopForSameCIDR(t *testing.T) {
	entries := make(map[string]ebpfpkg.TunnelEndpoint)

	addPeerBPFEntries(entries, []string{"10.244.0.0/16"},
		net.ParseIP("10.0.0.1"), 0, 100, 0, ebpfpkg.TunnelProtoWireGuard, "peerA")
	addPeerBPFEntries(entries, []string{"10.244.0.0/16"},
		net.ParseIP("10.0.0.2"), 0, 200, 0, ebpfpkg.TunnelProtoWireGuard, "peerB")

	ep, ok := entries["10.244.0.0/16"]
	if !ok {
		t.Fatal("expected entry for 10.244.0.0/16")
	}

	if got, want := len(ep.Nexthops), 2; got != want {
		t.Fatalf("multi-nexthop path: got %d nexthops, want %d", got, want)
	}
}

// TestAddPeerBPFEntriesSingleNexthop_FirstWins confirms that supernet
// RoutedCidr programming retains exactly one nexthop, set by the first
// caller, even when subsequent peers advertise the same prefix.
func TestAddPeerBPFEntriesSingleNexthop_FirstWins(t *testing.T) {
	entries := make(map[string]ebpfpkg.TunnelEndpoint)

	addPeerBPFEntriesSingleNexthop(entries, []string{"10.244.0.0/16"},
		net.ParseIP("10.0.0.1"), 0, 100, 0, ebpfpkg.TunnelProtoWireGuard, "peerA")
	addPeerBPFEntriesSingleNexthop(entries, []string{"10.244.0.0/16"},
		net.ParseIP("10.0.0.2"), 0, 200, 0, ebpfpkg.TunnelProtoWireGuard, "peerB")

	ep, ok := entries["10.244.0.0/16"]
	if !ok {
		t.Fatal("expected entry for 10.244.0.0/16")
	}

	if got, want := len(ep.Nexthops), 1; got != want {
		t.Fatalf("single-nexthop path: got %d nexthops, want %d", got, want)
	}

	nh := ep.Nexthops[0]
	if nh.PeerName != "peerA" {
		t.Errorf("first-peer-wins: got peer %q, want %q", nh.PeerName, "peerA")
	}

	if got, want := nh.IfIndex, uint32(100); got != want {
		t.Errorf("first-peer-wins: got ifindex %d, want %d", got, want)
	}

	if !nh.RemoteIP.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("first-peer-wins: got remote %s, want 10.0.0.1", nh.RemoteIP)
	}
}

// TestAddPeerBPFEntriesSingleNexthop_DoesNotAffectExistingMultiNexthop
// confirms that calling the single-nexthop helper for a CIDR that already
// has multi-nexthop entries (e.g. from PodCIDR programming) leaves them
// untouched. This guards against accidentally collapsing peer-specific
// PodCIDR entries when a different peer's RoutedCidr happens to match.
func TestAddPeerBPFEntriesSingleNexthop_DoesNotAffectExistingMultiNexthop(t *testing.T) {
	entries := make(map[string]ebpfpkg.TunnelEndpoint)

	addPeerBPFEntries(entries, []string{"10.244.0.0/24"},
		net.ParseIP("10.0.0.1"), 0, 100, 0, ebpfpkg.TunnelProtoWireGuard, "peerA")
	addPeerBPFEntries(entries, []string{"10.244.0.0/24"},
		net.ParseIP("10.0.0.2"), 0, 200, 0, ebpfpkg.TunnelProtoWireGuard, "peerB")

	addPeerBPFEntriesSingleNexthop(entries, []string{"10.244.0.0/24"},
		net.ParseIP("10.0.0.3"), 0, 300, 0, ebpfpkg.TunnelProtoWireGuard, "peerC")

	ep := entries["10.244.0.0/24"]
	if got, want := len(ep.Nexthops), 2; got != want {
		t.Fatalf("expected existing multi-nexthop entry to be preserved (got %d, want %d)", got, want)
	}

	for _, nh := range ep.Nexthops {
		if nh.PeerName == "peerC" {
			t.Errorf("single-nexthop helper unexpectedly added a nexthop for an existing entry")
		}
	}
}
