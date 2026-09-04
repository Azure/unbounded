// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net"
	"sort"

	"github.com/cilium/ebpf"
	"k8s.io/klog/v2"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// collectBpfEntries reads the unified eBPF LPM trie (unb_endpts) and returns
// BPF entries annotated with node names from the peer list. Returns an empty
// slice if the map is not present (e.g. running without root, or the
// unbounded_encap program is not loaded).
func (s *nodeStatusServer) collectBpfEntries() []statusv1alpha1.BpfEntry {
	m, err := bpfFindMap(ebpfpkg.MapName)
	if err != nil {
		klog.V(4).Infof("BPF map %q not available: %v", ebpfpkg.MapName, err)
		return nil
	}

	defer func() { _ = m.Close() }() //nolint:errcheck

	entries, err := bpfCollectEntries(m)
	if err != nil {
		klog.Warningf("Error iterating BPF map %q: %v", ebpfpkg.MapName, err)
	}

	s.annotateBpfEntries(entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].CIDR < entries[j].CIDR })

	return entries
}

// annotateBpfEntries enriches BPF entries with the destination node name
// using the peer list from the current status. Matches by CIDR (pod CIDRs /
// routed CIDRs) or by remote IP (internal / external IPs).
func (s *nodeStatusServer) annotateBpfEntries(entries []statusv1alpha1.BpfEntry) {
	if len(entries) == 0 {
		return
	}

	s.state.mu.Lock()
	byCIDR := make(map[string]string)
	byEndpoint := make(map[string]string)

	for _, p := range s.state.peers {
		for _, cidr := range p.PodCIDRs {
			byCIDR[cidr] = p.Name
		}

		for _, ip := range p.InternalIPs {
			byEndpoint[ip] = p.Name
		}
	}

	for _, gp := range s.state.gatewayPeers {
		for _, cidr := range gp.RoutedCidrs {
			byCIDR[cidr] = gp.Name
		}

		for _, cidr := range gp.PodCIDRs {
			byCIDR[cidr] = gp.Name
		}

		for _, ip := range gp.InternalIPs {
			byEndpoint[ip] = gp.Name
		}

		for _, ip := range gp.ExternalIPs {
			byEndpoint[ip] = gp.Name
		}
	}
	s.state.mu.Unlock()

	if len(byCIDR) == 0 && len(byEndpoint) == 0 {
		return
	}

	for i := range entries {
		if name, ok := byCIDR[entries[i].CIDR]; ok {
			entries[i].Node = name
			continue
		}

		if name, ok := byEndpoint[entries[i].Remote]; ok {
			entries[i].Node = name
		}
	}
}

// bpfFindMap scans loaded BPF maps for one matching the given name.
func bpfFindMap(name string) (*ebpf.Map, error) {
	id := ebpf.MapID(0)

	for {
		var err error

		id, err = ebpf.MapGetNextID(id)
		if err != nil {
			break
		}

		m, err := ebpf.NewMapFromID(id)
		if err != nil {
			continue
		}

		info, err := m.Info()
		if err != nil {
			_ = m.Close() //nolint:errcheck
			continue
		}

		if info.Name == name {
			return m, nil
		}

		_ = m.Close() //nolint:errcheck
	}

	return nil, fmt.Errorf("BPF map %q not found", name)
}

// bpfCollectEntries iterates the unified LPM trie and produces one BpfEntry
// per nexthop.
func bpfCollectEntries(m *ebpf.Map) ([]statusv1alpha1.BpfEntry, error) {
	var (
		key ebpfpkg.LpmKey
		val ebpfpkg.RawTunnelEndpoint
	)

	var entries []statusv1alpha1.BpfEntry

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		entries = append(entries, bpfMakeEntries(key, val)...)
	}

	if err := iter.Err(); err != nil {
		return entries, fmt.Errorf("iterate %s: %w", ebpfpkg.MapName, err)
	}

	return entries, nil
}

// bpfMakeEntries expands a single LPM trie entry into one BpfEntry per
// nexthop. v4 entries (those whose key is IPv4-mapped) are rendered with
// dotted-quad CIDR notation; v6 entries use canonical v6 form. Underlay
// addresses follow the same rule.
func bpfMakeEntries(key ebpfpkg.LpmKey, val ebpfpkg.RawTunnelEndpoint) []statusv1alpha1.BpfEntry {
	cidr := bpfFormatKey(key)

	entries := make([]statusv1alpha1.BpfEntry, 0, val.Count)
	for i := uint32(0); i < val.Count && i < uint32(ebpfpkg.MaxNexthops); i++ {
		nh := val.Nexthops[i]
		ifName, mtu := bpfResolveInterface(nh.Ifindex)

		entries = append(entries, statusv1alpha1.BpfEntry{
			CIDR:      cidr,
			Remote:    bpfFormatEndpoint(nh.RemoteEndpoint),
			Interface: ifName,
			Protocol:  bpfProtocolName(nh.Protocol),
			Healthy:   nh.Healthy != 0,
			VNI:       nh.Vni,
			MTU:       mtu,
			IfIndex:   nh.Ifindex,
		})
	}

	return entries
}

// bpfFormatKey renders an LPM key as a CIDR string. v4-mapped entries are
// unmapped to dotted-quad with the +96 prefix offset removed.
func bpfFormatKey(key ebpfpkg.LpmKey) string {
	if ebpfpkg.IsV4Mapped(key.Addr) {
		ip := net.IPv4(key.Addr[12], key.Addr[13], key.Addr[14], key.Addr[15])

		prefix := int(key.Prefixlen) - 96
		if prefix < 0 {
			prefix = 0
		}

		return fmt.Sprintf("%s/%d", ip.String(), prefix)
	}

	ip := net.IP(append([]byte(nil), key.Addr[:]...))

	return fmt.Sprintf("%s/%d", ip.String(), key.Prefixlen)
}

// bpfFormatEndpoint renders a 16-byte underlay endpoint as either a v4
// dotted-quad (if IPv4-mapped) or canonical v6.
func bpfFormatEndpoint(addr [16]byte) string {
	if ebpfpkg.IsV4Mapped(addr) {
		return net.IPv4(addr[12], addr[13], addr[14], addr[15]).String()
	}

	return net.IP(append([]byte(nil), addr[:]...)).String()
}

// bpfProtocolName returns the tunnel protocol name for the given constant.
func bpfProtocolName(proto uint32) string {
	switch proto {
	case ebpfpkg.TunnelProtoGENEVE:
		return "GENEVE"
	case ebpfpkg.TunnelProtoVXLAN:
		return "VXLAN"
	case ebpfpkg.TunnelProtoIPIP:
		return "IPIP"
	case ebpfpkg.TunnelProtoWireGuard:
		return "WireGuard"
	case ebpfpkg.TunnelProtoNone:
		return "None"
	default:
		return fmt.Sprintf("unknown(%d)", proto)
	}
}

// bpfResolveInterface returns the interface name and MTU for the given
// ifindex; returns a placeholder if the interface no longer exists.
func bpfResolveInterface(ifindex uint32) (string, int) {
	iface, err := net.InterfaceByIndex(int(ifindex))
	if err != nil {
		return fmt.Sprintf("if%d", ifindex), 0
	}

	return iface.Name, iface.MTU
}
