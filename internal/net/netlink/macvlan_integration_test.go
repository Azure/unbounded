// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"net"
	"os"
	"os/exec"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Run only in a disposable network namespace, never the host namespace.
func TestPrivateMACVLANIntegration(t *testing.T) {
	if os.Getenv("UNBOUNDED_TEST_NETNS") != "1" {
		t.Skip("requires a disposable network namespace and CAP_NET_ADMIN")
	}

	backend := NewLinkManager("unb-test1")
	if err := backend.EnsureDummyInterface(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := backend.DeleteLink(); err != nil {
			t.Error(err)
		}
	})

	frontend := NewLinkManager("unb-test0")
	for range 2 {
		if err := frontend.EnsurePrivateMACVLANInterface("unb-test1"); err != nil {
			t.Fatal(err)
		}
	}

	lower, err := netlink.LinkByName("unb-test1")
	if err != nil {
		t.Fatal(err)
	}

	link, err := netlink.LinkByName("unb-test0")
	if err != nil {
		t.Fatal(err)
	}

	macvlan, ok := link.(*netlink.Macvlan)
	if !ok || macvlan.Mode != netlink.MACVLAN_MODE_PRIVATE || macvlan.ParentIndex != lower.Attrs().Index {
		t.Fatalf("unexpected MACVLAN: %+v", link)
	}

	if link.Attrs().Flags&net.FlagUp == 0 || link.Attrs().RawFlags&unix.IFF_NOARP == 0 {
		t.Fatalf("frontend is not up with NOARP: %+v", link.Attrs())
	}

	for _, mtu := range []int{1400, 1500, 1280} {
		if err := backend.EnsureMTU(mtu); err != nil {
			t.Fatal(err)
		}

		if err := frontend.EnsureMTU(mtu); err != nil {
			t.Fatal(err)
		}
	}

	for _, prefix := range []string{"192.0.2.1/32", "fe80::1/64", "2001:db8::1/128"} {
		address, err := netlink.ParseAddr(prefix)
		if err != nil {
			t.Fatal(err)
		}

		if err := netlink.AddrAdd(lower, address); err != nil {
			t.Fatal(err)
		}
	}

	for range 2 {
		if err := backend.EnsureUnnumbered(); err != nil {
			t.Fatal(err)
		}
	}

	addresses, err := backend.GetAddresses()
	if err != nil || len(addresses) != 0 {
		t.Fatalf("backend addresses: %v, error: %v", addresses, err)
	}

	address, err := netlink.ParseAddr("192.0.2.1/32")
	if err != nil {
		t.Fatal(err)
	}

	if err := netlink.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}

	_, destination, err := net.ParseCIDR("198.51.100.0/24")
	if err != nil {
		t.Fatal(err)
	}

	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: destination}); err != nil {
		t.Fatal(err)
	}

	for _, device := range []netlink.Link{link, lower} {
		if err := netlink.QdiscAdd(&netlink.Clsact{QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: device.Attrs().Index, Handle: netlink.MakeHandle(0xffff, 0), Parent: netlink.HANDLE_CLSACT,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	filter := func(device netlink.Link, action netlink.TcAct) *netlink.MatchAll {
		return &netlink.MatchAll{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: device.Attrs().Index, Parent: netlink.HANDLE_MIN_EGRESS,
				Handle: 1, Priority: 1, Protocol: unix.ETH_P_ALL,
			},
			Actions: []netlink.Action{&netlink.GenericAction{ActionAttrs: netlink.ActionAttrs{Action: action}}},
		}
	}
	if err := netlink.FilterAdd(filter(link, netlink.TC_ACT_OK)); err != nil {
		t.Fatal(err)
	}

	if err := netlink.FilterAdd(filter(lower, netlink.TC_ACT_SHOT)); err != nil {
		t.Fatal(err)
	}

	packets := func(device netlink.Link) uint32 {
		t.Helper()

		filters, err := netlink.FilterList(device, netlink.HANDLE_MIN_EGRESS)
		if err != nil {
			t.Fatal(err)
		}

		if len(filters) != 1 {
			t.Fatalf("unexpected filters: %v", filters)
		}

		match, ok := filters[0].(*netlink.MatchAll)
		if !ok || len(match.Actions) != 1 || match.Actions[0].Attrs().Statistics == nil {
			t.Fatalf("missing action statistics: %+v", filters[0])
		}

		return match.Actions[0].Attrs().Statistics.Basic.Packets
	}
	ping := func() {
		t.Helper()

		output, err := exec.CommandContext(t.Context(), "ping", "-c", "1", "-W", "1", "198.51.100.1").CombinedOutput()
		if err == nil {
			t.Fatal("ping passed through the backend drop")
		}

		t.Logf("expected drop: %s", output)
	}
	ping()

	if packets(link) == 0 || packets(lower) == 0 {
		t.Fatal("frontend PASS did not reach both hooks")
	}

	before := packets(lower)

	if err := netlink.FilterDel(filter(link, netlink.TC_ACT_OK)); err != nil {
		t.Fatal(err)
	}

	if err := netlink.FilterAdd(filter(link, netlink.TC_ACT_SHOT)); err != nil {
		t.Fatal(err)
	}

	ping()

	if after := packets(lower); after != before {
		t.Fatalf("frontend DROP reached backend: packets %d -> %d", before, after)
	}

	other := NewLinkManager("unb-test2")
	if err := other.EnsureDummyInterface(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := other.DeleteLink(); err != nil {
			t.Error(err)
		}
	})

	if err := frontend.EnsurePrivateMACVLANInterface("unb-test2"); err == nil {
		t.Fatal("accepted an existing MACVLAN with the wrong parent")
	}

	if err := other.EnsurePrivateMACVLANInterface("unb-test1"); err == nil {
		t.Fatal("accepted a non-MACVLAN frontend")
	}

	if err := NewLinkManager("unb-test3").EnsurePrivateMACVLANInterface("unb-test0"); err == nil {
		t.Fatal("accepted a non-dummy backend")
	}

	if err := netlink.LinkSetMacvlanMode(link, netlink.MACVLAN_MODE_BRIDGE); err != nil {
		t.Fatal(err)
	}

	if err := frontend.EnsurePrivateMACVLANInterface("unb-test1"); err == nil {
		t.Fatal("accepted an existing MACVLAN in bridge mode")
	}

	if err := NewLinkManager("unb-missing").EnsurePrivateMACVLANInterface("missing"); err == nil {
		t.Fatal("accepted a missing backend")
	}

	if err := NewLinkManager("unb-missing").EnsureUnnumbered(); err == nil {
		t.Fatal("accepted a missing unnumbered interface")
	}
}
