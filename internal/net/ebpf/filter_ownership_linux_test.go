// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"errors"
	"runtime"
	"testing"

	ciliumebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestTunnelFilterOwnership(t *testing.T) {
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.Close() }()

	isolated, err := netns.New()
	if errors.Is(err, unix.EPERM) {
		t.Skip("requires network namespace privileges")
	}

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = isolated.Close() }()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatal(err)
		}
	}()

	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "unbounded0"}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}

	if err := ensureClsactQdisc(link.Index, link.Name); err != nil {
		t.Fatal(err)
	}

	provider, err := ciliumebpf.NewProgram(&ciliumebpf.ProgramSpec{
		Type: ciliumebpf.SchedCLS, License: "MIT",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, 0), asm.Return()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = provider.Close() }()

	providerFilter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Index, Parent: netlink.HANDLE_MIN_EGRESS,
			Priority: 1, Handle: 1, Protocol: unix.ETH_P_ALL,
		},
		Fd: provider.FD(), Name: "foreign_cni", DirectAction: true,
	}
	if err := netlink.FilterAdd(providerFilter); err != nil {
		t.Fatal(err)
	}

	tunnelMap, err := NewTunnelMap(TunnelMapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tunnelMap.Close() }()

	for attempt := range 2 {
		if err := tunnelMap.AttachToInterface(link.Name); err != nil {
			t.Fatal(err)
		}

		filters, err := netlink.FilterList(link, netlink.HANDLE_MIN_EGRESS)
		if err != nil || len(filters) != 2 {
			t.Fatalf("provider and Unbounded filters must coexist: %v, %v", filters, err)
		}

		for _, filter := range filters {
			bpf, ok := filter.(*netlink.BpfFilter)
			if !ok {
				t.Fatalf("unexpected filter: %v", filter)
			}

			if bpf.Name == ProgramName {
				if err := netlink.FilterDel(filter); err != nil {
					t.Fatal(err)
				}
			} else if bpf.Name != "foreign_cni" {
				t.Fatalf("provider filter was overwritten on attempt %d: %v", attempt, bpf)
			}
		}
	}

	if err := tunnelMap.AttachToInterface(link.Name); err != nil {
		t.Fatal(err)
	}

	if err := tunnelMap.Close(); err != nil {
		t.Fatal(err)
	}

	filters, err := netlink.FilterList(link, netlink.HANDLE_MIN_EGRESS)
	if err != nil || len(filters) != 1 || filters[0].(*netlink.BpfFilter).Name != "foreign_cni" {
		t.Fatalf("cleanup removed provider filter: %v, %v", filters, err)
	}

	providerFilter.Priority, providerFilter.Handle = filterPriority, filterHandle
	if err := netlink.FilterAdd(providerFilter); err != nil {
		t.Fatal(err)
	}

	replacement, err := NewTunnelMap(TunnelMapOptions{})
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = replacement.Close() }()

	if err := replacement.AttachToInterface(link.Name); err == nil {
		t.Fatal("foreign program at our requested slot was overwritten")
	}

	filters, err = netlink.FilterList(link, netlink.HANDLE_MIN_EGRESS)
	if err != nil || len(filters) != 2 {
		t.Fatalf("rejected attachment changed provider filters: %v, %v", filters, err)
	}

	info, err := provider.Info()
	if err != nil {
		t.Fatal(err)
	}

	providerID, _ := info.ID()

	for _, filter := range filters {
		bpf, ok := filter.(*netlink.BpfFilter)
		if !ok || bpf.Name != "foreign_cni" || bpf.Id != int(providerID) {
			t.Fatalf("rejected attachment replaced a provider program: %v", filter)
		}
	}

	legacyLink := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "unb-legacy"}}
	if err := netlink.LinkAdd(legacyLink); err != nil {
		t.Fatal(err)
	}

	if err := ensureClsactQdisc(legacyLink.Index, legacyLink.Name); err != nil {
		t.Fatal(err)
	}

	legacy := *providerFilter
	legacy.LinkIndex, legacy.Priority, legacy.Handle = legacyLink.Index, 1, 1

	legacy.Fd, legacy.Name = replacement.prog.FD(), ProgramName
	if err := netlink.FilterAdd(&legacy); err != nil {
		t.Fatal(err)
	}

	if err := replacement.AttachToInterface(legacyLink.Name); err != nil {
		t.Fatal(err)
	}

	filters, err = netlink.FilterList(legacyLink, netlink.HANDLE_MIN_EGRESS)
	if err != nil || len(filters) != 1 || filters[0].Attrs().Priority != filterPriority {
		t.Fatalf("owned legacy filter was not migrated: %v, %v", filters, err)
	}

	successor, err := NewTunnelMap(TunnelMapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = successor.Close() }()

	if err := successor.AttachToInterface(legacyLink.Name); err != nil {
		t.Fatal(err)
	}

	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}

	filters, err = netlink.FilterList(legacyLink, netlink.HANDLE_MIN_EGRESS)
	if err != nil || len(filters) != 1 {
		t.Fatalf("old map cleanup removed its successor: %v, %v", filters, err)
	}

	info, err = successor.prog.Info()
	if err != nil {
		t.Fatal(err)
	}

	successorID, _ := info.ID()
	if filter, ok := filters[0].(*netlink.BpfFilter); !ok || filter.Id != int(successorID) {
		t.Fatalf("successor program was changed by cleanup: %v", filters[0])
	}
}
