// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"sort"

	"github.com/cilium/ebpf"
	"k8s.io/klog/v2"

	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

const (
	bpfMapName     = "unbounded_endpo" // kernel truncates to 15 chars
	bpfV4KeySize   = 8                 // sizeof(lpmKeyV4)
	bpfV6KeySize   = 20                // sizeof(lpmKeyV6)
	bpfMaxNexthops = 4
)

// bpfLpmKeyV4 matches struct lpm_key_v4 in the BPF programs.
type bpfLpmKeyV4 struct {
	Prefixlen uint32
	Addr      uint32
}

// bpfLpmKeyV6 matches struct lpm_key_v6 in the BPF programs.
type bpfLpmKeyV6 struct {
	Prefixlen uint32
	Addr      [4]uint32
}

// bpfTunnelNexthopV4 matches struct tunnel_nexthop in the BPF programs.
type bpfTunnelNexthopV4 struct {
	RemoteIPv4 uint32
	VNI        uint32
	IfIndex    uint32
	Flags      uint32
	Protocol   uint32
}

// bpfTunnelEndpointV4 matches struct tunnel_endpoint_v4 in the BPF programs.
type bpfTunnelEndpointV4 struct {
	Nexthops [bpfMaxNexthops]bpfTunnelNexthopV4
	Count    uint32
}

// bpfTunnelNexthopV6 matches struct tunnel_nexthop_v6 in the BPF programs.
type bpfTunnelNexthopV6 struct {
	RemoteIPv6 [4]uint32
	VNI        uint32
	IfIndex    uint32
	Flags      uint32
	Protocol   uint32
}

// bpfTunnelEndpointV6 matches struct tunnel_endpoint_v6 in the BPF programs.
type bpfTunnelEndpointV6 struct {
	Nexthops [bpfMaxNexthops]bpfTunnelNexthopV6
	Count    uint32
}

// collectBpfEntries reads the eBPF LPM trie tunnel maps and returns BPF entries
// annotated with node names from the peer list. Returns an empty slice if the
// maps are not found (e.g. when not running as root or BPF programs not loaded).
func (s *nodeStatusServer) collectBpfEntries() []statusv1alpha1.BpfEntry {
	maps, err := bpfFindMapsByName(bpfMapName)
	if err != nil {
		klog.V(4).Infof("BPF maps not available: %v", err)
		return nil
	}

	defer func() {
		for _, m := range maps {
			_ = m.Close() //nolint:errcheck
		}
	}()

	var entries []statusv1alpha1.BpfEntry

	for _, m := range maps {
		info, err := m.Info()
		if err != nil {
			klog.V(4).Infof("BPF map info error: %v", err)
			continue
		}

		switch info.KeySize {
		case bpfV4KeySize:
			entries, err = bpfCollectV4Entries(m, entries)
		case bpfV6KeySize:
			entries, err = bpfCollectV6Entries(m, entries)
		default:
			continue
		}

		if err != nil {
			klog.Warningf("Error iterating BPF map (key_size=%d): %v", info.KeySize, err)
		}
	}

	// Annotate entries with node names from the peer list
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

// bpfFindMapsByName scans all loaded BPF maps and returns every map matching
// the given name. Returns an error if no maps are found.
func bpfFindMapsByName(name string) ([]*ebpf.Map, error) {
	var maps []*ebpf.Map

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
			maps = append(maps, m)
		} else {
			_ = m.Close() //nolint:errcheck
		}
	}

	if len(maps) == 0 {
		return nil, fmt.Errorf("BPF map %q not found", name)
	}

	return maps, nil
}

// bpfCollectV4Entries iterates a v4 tunnel endpoint map and appends entries.
func bpfCollectV4Entries(m *ebpf.Map, entries []statusv1alpha1.BpfEntry) ([]statusv1alpha1.BpfEntry, error) {
	var (
		key bpfLpmKeyV4
		val bpfTunnelEndpointV4
	)

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		entries = append(entries, bpfMakeEntriesV4(key, val)...)
	}

	if err := iter.Err(); err != nil {
		return entries, fmt.Errorf("iterate unbounded_endpoints_v4: %w", err)
	}

	return entries, nil
}

// bpfCollectV6Entries iterates a v6 tunnel endpoint map and appends entries.
func bpfCollectV6Entries(m *ebpf.Map, entries []statusv1alpha1.BpfEntry) ([]statusv1alpha1.BpfEntry, error) {
	var (
		key bpfLpmKeyV6
		val bpfTunnelEndpointV6
	)

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		entries = append(entries, bpfMakeEntriesV6(key, val)...)
	}

	if err := iter.Err(); err != nil {
		return entries, fmt.Errorf("iterate unbounded_endpoints_v6: %w", err)
	}

	return entries, nil
}

// bpfMakeEntriesV4 builds BpfEntry values from v4 key/value pairs, one per nexthop.
func bpfMakeEntriesV4(key bpfLpmKeyV4, val bpfTunnelEndpointV4) []statusv1alpha1.BpfEntry {
	var entries []statusv1alpha1.BpfEntry

	cidr := fmt.Sprintf("%s/%d", bpfUint32ToIPv4LE(key.Addr), key.Prefixlen)

	for i := uint32(0); i < val.Count && i < bpfMaxNexthops; i++ {
		nh := val.Nexthops[i]
		ifName, mtu := bpfResolveInterface(nh.IfIndex)
		entries = append(entries, statusv1alpha1.BpfEntry{
			CIDR:      cidr,
			Remote:    bpfUint32ToIPv4BE(nh.RemoteIPv4).String(),
			Interface: ifName,
			Protocol:  bpfProtocolName(nh.Protocol),
			Healthy:   nh.Flags&0x02 != 0,
			VNI:       nh.VNI,
			MTU:       mtu,
			IfIndex:   nh.IfIndex,
		})
	}

	return entries
}

// bpfMakeEntriesV6 builds BpfEntry values from v6 key/value pairs, one per nexthop.
func bpfMakeEntriesV6(key bpfLpmKeyV6, val bpfTunnelEndpointV6) []statusv1alpha1.BpfEntry {
	var entries []statusv1alpha1.BpfEntry

	cidr := fmt.Sprintf("%s/%d", bpfFormatIP(bpfUint32ArrayToIPv6(key.Addr)), key.Prefixlen)

	for i := uint32(0); i < val.Count && i < bpfMaxNexthops; i++ {
		nh := val.Nexthops[i]
		ifName, mtu := bpfResolveInterface(nh.IfIndex)

		var remote string
		if nh.Flags&0x04 != 0 {
			// IPv6 underlay
			remote = bpfFormatIP(bpfUint32ArrayToIPv6(nh.RemoteIPv6))
		} else {
			// IPv4 underlay (stored in first element of union)
			remote = bpfUint32ToIPv4BE(nh.RemoteIPv6[0]).String()
		}

		entries = append(entries, statusv1alpha1.BpfEntry{
			CIDR:      cidr,
			Remote:    remote,
			Interface: ifName,
			Protocol:  bpfProtocolName(nh.Protocol),
			Healthy:   nh.Flags&0x02 != 0,
			VNI:       nh.VNI,
			MTU:       mtu,
			IfIndex:   nh.IfIndex,
		})
	}

	return entries
}

// bpfProtocolName returns the tunnel protocol name for the given constant.
func bpfProtocolName(proto uint32) string {
	switch proto {
	case 1:
		return "GENEVE"
	case 2:
		return "VXLAN"
	case 3:
		return "IPIP"
	case 4:
		return "WireGuard"
	case 5:
		return "None"
	default:
		return fmt.Sprintf("unknown(%d)", proto)
	}
}

// bpfResolveInterface returns the interface name and MTU for the given ifindex.
func bpfResolveInterface(ifindex uint32) (string, int) {
	iface, err := net.InterfaceByIndex(int(ifindex))
	if err != nil {
		return fmt.Sprintf("if%d", ifindex), 0
	}

	return iface.Name, iface.MTU
}

// bpfUint32ToIPv4LE converts a uint32 stored via LittleEndian.Uint32 back to
// a dotted-decimal IPv4 address.
func bpfUint32ToIPv4LE(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, v)

	return ip
}

// bpfUint32ToIPv4BE converts a uint32 stored via BigEndian.Uint32 (network byte
// order) back to a dotted-decimal IPv4 address.
func bpfUint32ToIPv4BE(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)

	return ip
}

// bpfUint32ArrayToIPv6 converts a [4]uint32 array where each segment was stored
// via LittleEndian.Uint32 back to a 16-byte IPv6 address.
func bpfUint32ArrayToIPv6(arr [4]uint32) net.IP {
	ip := make(net.IP, 16)
	binary.LittleEndian.PutUint32(ip[0:4], arr[0])
	binary.LittleEndian.PutUint32(ip[4:8], arr[1])
	binary.LittleEndian.PutUint32(ip[8:12], arr[2])
	binary.LittleEndian.PutUint32(ip[12:16], arr[3])

	return ip
}

// bpfFormatIP returns the string representation of an IP, using IPv4 notation
// for IPv4-mapped IPv6 addresses.
func bpfFormatIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}

	return ip.String()
}
