// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"os"
	"testing"

	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"

	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestStagedTunnelMapIntegration(t *testing.T) {
	if os.Getenv("UNBOUNDED_TEST_NETNS") != "1" {
		t.Skip("requires a disposable network namespace, CAP_NET_ADMIN, and CAP_BPF")
	}

	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}

	frontend := unboundednetnetlink.NewLinkManager(unbounded0DeviceName)
	backend := unboundednetnetlink.NewLinkManager(unbounded1DeviceName)

	if err := frontend.EnsureDummyInterface(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := backend.DeleteLink(); err != nil {
			t.Error(err)
		}
	})

	cfg := &config{TunnelDataplaneMapSize: 16}
	state := &wireGuardState{nodePodCIDRs: []string{"192.0.2.0/24", "2001:db8::/64"}, manageCniPlugin: true}

	t.Cleanup(func() {
		if state.tunnelMap != nil {
			if err := state.tunnelMap.Close(); err != nil {
				t.Error(err)
			}
		}
	})

	for range 2 {
		if _, err := ensureTunnelMap(cfg, state); err != nil {
			t.Fatal(err)
		}
	}

	link, err := netlink.LinkByName(unbounded0DeviceName)
	if err != nil {
		t.Fatal(err)
	}

	macvlan, ok := link.(*netlink.Macvlan)
	if !ok || macvlan.Mode != netlink.MACVLAN_MODE_PRIVATE {
		t.Fatalf("legacy dummy not replaced with private MACVLAN: %+v", link)
	}

	filters, err := netlink.FilterList(link, netlink.HANDLE_MIN_EGRESS)
	if err != nil || len(filters) != 0 {
		t.Fatalf("frontend must be available for the CNI, filters: %v, error: %v", filters, err)
	}

	lower, err := netlink.LinkByName(unbounded1DeviceName)
	if err != nil {
		t.Fatal(err)
	}

	filters, err = netlink.FilterList(lower, netlink.HANDLE_MIN_EGRESS)
	if err != nil || len(filters) != 1 {
		t.Fatalf("backend filter missing: %v, error: %v", filters, err)
	}

	addresses, err := backend.GetAddresses()
	if err != nil || len(addresses) != 0 {
		t.Fatalf("backend must be unnumbered: %v, error: %v", addresses, err)
	}

	addresses, err = frontend.GetAddresses()
	if err != nil || len(addresses) < 2 {
		t.Fatalf("frontend gateway addresses missing: %v, error: %v", addresses, err)
	}

	state.manageCniPlugin = false
	if _, err := ensureTunnelMap(cfg, state); err != nil {
		t.Fatal(err)
	}

	addresses, err = frontend.GetAddresses()
	if err != nil {
		t.Fatal(err)
	}

	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address)
		if err != nil || !ip.IsLinkLocalUnicast() {
			t.Fatalf("external CNI frontend acquired gateway address: %s, %v", address, err)
		}
	}

	if err := frontend.DeleteLink(); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureTunnelMap(cfg, state); err != nil {
		t.Fatalf("frontend recreation failed: %v", err)
	}
}
