// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ebpf provides eBPF-based tunnel dataplane management.
//
// TunnelMap wraps a compiled eBPF TC classifier program and its LPM trie map.
// The TC filter is attached to the egress of the default route interface
// (underlay). It intercepts packets destined to overlay CIDRs, sets the
// tunnel key via bpf_skb_set_tunnel_key, and redirects them to a flow-based
// tunnel interface (geneve0 or vxlan0) via bpf_redirect.
package ebpf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"

	_ "embed"
)

//go:embed unbounded_encap_bpfel.o
var tunnelEncapProgram []byte

// TunnelNexthop describes a single nexthop within a tunnel endpoint.
type TunnelNexthop struct {
	RemoteIP net.IP // the peer's underlay IP (4 or 16 bytes)
	VNI      uint32
	IfIndex  uint32 // tunnel interface index to redirect to
	Flags    uint32 // TunnelFlag* constants
	Protocol uint32 // TunnelProto* constant
	PeerName string // peer hostname for healthcheck correlation (not stored in BPF)
}

// TunnelEndpoint holds all nexthops for a CIDR prefix.
type TunnelEndpoint struct {
	Nexthops []TunnelNexthop
}

// MaxNexthops is the maximum number of nexthops per tunnel endpoint,
// matching MAX_NEXTHOPS in the BPF program.
const MaxNexthops = 4

// Tunnel endpoint flags matching BPF TUNNEL_F_* constants.
const (
	TunnelFlagSetKey       uint32 = 0x01 // call bpf_skb_set_tunnel_key (GENEVE, VXLAN, IPIP)
	TunnelFlagHealthy      uint32 = 0x02 // peer is healthy; BPF skips if not set
	TunnelFlagIPv6Underlay uint32 = 0x04 // use IPv6 underlay (remote_ipv6 + BPF_F_TUNINFO_IPV6)
)

// Tunnel protocol constants matching BPF PROTO_* constants.
const (
	TunnelProtoGENEVE    uint32 = 1
	TunnelProtoVXLAN     uint32 = 2
	TunnelProtoIPIP      uint32 = 3
	TunnelProtoWireGuard uint32 = 4
	TunnelProtoNone      uint32 = 5
)

// tunnelNexthopC matches struct tunnel_nexthop in the BPF program.
type tunnelNexthopC struct {
	RemoteIPv4 uint32
	VNI        uint32
	IfIndex    uint32
	Flags      uint32
	Protocol   uint32
}

// tunnelEndpointV4C matches struct tunnel_endpoint_v4 in the BPF program.
type tunnelEndpointV4C struct {
	Nexthops [MaxNexthops]tunnelNexthopC
	Count    uint32
}

// tunnelNexthopV6C matches struct tunnel_nexthop_v6 in the BPF program.
type tunnelNexthopV6C struct {
	RemoteIPv6 [4]uint32 // union with remote_ipv4 at [0]
	VNI        uint32
	IfIndex    uint32
	Flags      uint32
	Protocol   uint32
}

// tunnelEndpointV6C matches struct tunnel_endpoint_v6 in the BPF program.
type tunnelEndpointV6C struct {
	Nexthops [MaxNexthops]tunnelNexthopV6C
	Count    uint32
}

// lpmKeyV4 matches struct lpm_key_v4 in the BPF program.
type lpmKeyV4 struct {
	Prefixlen uint32
	Addr      uint32
}

// lpmKeyV6 matches struct lpm_key_v6 in the BPF program.
type lpmKeyV6 struct {
	Prefixlen uint32
	Addr      [4]uint32
}

// TunnelMap manages the eBPF tunnel encapsulation program and its LPM tries.
type TunnelMap struct {
	mu          sync.Mutex
	coll        *ebpf.Collection
	lpmV4Map    *ebpf.Map
	lpmV6Map    *ebpf.Map
	encapProg   *ebpf.Program
	maxEntries  uint32
	attachedIfs map[int]string // ifindex -> name for TC egress filters

	// lastReconciled is a snapshot of the last Reconcile() desired state,
	// used by SetPeerHealth to find and update entries for a given peer.
	lastReconciled map[string]TunnelEndpoint
}

// TunnelMapOptions configures TunnelMap creation.
type TunnelMapOptions struct {
	// MaxEntries is the capacity of the LPM trie map. Default: 16384.
	MaxEntries uint32
}

// NewTunnelMap loads the unbounded_encap eBPF program and creates the LPM tries.
func NewTunnelMap(opts TunnelMapOptions) (*TunnelMap, error) {
	if opts.MaxEntries == 0 {
		opts.MaxEntries = 16384
	}

	spec, err := ebpf.LoadCollectionSpecFromReader(
		bytes.NewReader(tunnelEncapProgram),
	)
	if err != nil {
		return nil, fmt.Errorf("load unbounded_encap spec: %w", err)
	}

	for _, m := range spec.Maps {
		if m.Type == ebpf.LPMTrie {
			m.MaxEntries = opts.MaxEntries
		}
	}

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, fmt.Errorf("create unbounded_encap collection: %w", err)
	}

	lpmV4 := coll.Maps["unbounded_endpoints_v4"]
	if lpmV4 == nil {
		coll.Close()
		return nil, errors.New("unbounded_endpoints_v4 map not found")
	}

	lpmV6 := coll.Maps["unbounded_endpoints_v6"]
	if lpmV6 == nil {
		coll.Close()
		return nil, errors.New("unbounded_endpoints_v6 map not found")
	}

	encapProg := coll.Programs["unbounded_encap"]
	if encapProg == nil {
		coll.Close()
		return nil, errors.New("unbounded_encap program not found")
	}

	klog.Infof("eBPF unbounded_encap program loaded (v4+v6, map max_entries %d)", opts.MaxEntries)

	return &TunnelMap{
		coll:       coll,
		lpmV4Map:   lpmV4,
		lpmV6Map:   lpmV6,
		encapProg:  encapProg,
		maxEntries: opts.MaxEntries,
	}, nil
}

// AttachToInterface loads the unbounded_encap TC egress BPF program onto the
// named interface (unbounded0). With NOARP on the dummy interface, no ARP
// responder is needed.
func (tm *TunnelMap) AttachToInterface(ifName string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	iface, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("find interface %q: %w", ifName, err)
	}

	if tm.attachedIfs == nil {
		tm.attachedIfs = make(map[int]string)
	}

	ifIdx := iface.Attrs().Index
	if _, already := tm.attachedIfs[ifIdx]; already {
		return nil
	}

	// Ensure clsact qdisc exists
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifIdx,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(qdisc); err != nil {
		if !errors.Is(err, os.ErrExist) && !isErrExist(err) {
			return fmt.Errorf("add clsact qdisc to %s: %w", ifName, err)
		}
	}

	// TC egress: unbounded_encap
	egressFilter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: ifIdx,
			Handle:    1,
			Parent:    netlink.HANDLE_MIN_EGRESS,
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Fd:           tm.encapProg.FD(),
		Name:         "unbounded_encap",
		DirectAction: true,
	}

	_ = netlink.FilterDel(egressFilter) //nolint:errcheck
	if err := netlink.FilterAdd(egressFilter); err != nil {
		return fmt.Errorf("add unbounded_encap to %s egress: %w", ifName, err)
	}

	tm.attachedIfs[ifIdx] = ifName
	klog.Infof("eBPF TC on %s: unbounded_encap (egress), ifindex %d", ifName, ifIdx)

	return nil
}

// Attached returns whether the TC filter has been successfully attached to
// at least one interface.
func (tm *TunnelMap) Attached() bool {
	return len(tm.attachedIfs) > 0
}

// UpdateEndpoint adds or updates an LPM trie entry mapping a destination
// CIDR to a tunnel endpoint. Automatically selects the v4 or v6 map.
func (tm *TunnelMap) UpdateEndpoint(cidr *net.IPNet, ep TunnelEndpoint) error {
	if cidr.IP.To4() != nil {
		key, err := cidrToKeyV4(cidr)
		if err != nil {
			return err
		}

		return tm.lpmV4Map.Update(key, epToV4C(ep), ebpf.UpdateAny)
	}

	key, err := cidrToKeyV6(cidr)
	if err != nil {
		return err
	}

	return tm.lpmV6Map.Update(key, epToV6C(ep), ebpf.UpdateAny)
}

// DeleteEndpoint removes an LPM trie entry for a destination CIDR.
func (tm *TunnelMap) DeleteEndpoint(cidr *net.IPNet) error {
	var err error

	if cidr.IP.To4() != nil {
		key, kerr := cidrToKeyV4(cidr)
		if kerr != nil {
			return kerr
		}

		err = tm.lpmV4Map.Delete(key)
	} else {
		key, kerr := cidrToKeyV6(cidr)
		if kerr != nil {
			return kerr
		}

		err = tm.lpmV6Map.Delete(key)
	}

	if errors.Is(err, ebpf.ErrKeyNotExist) {
		return nil
	}

	return err
}

// Reconcile sets both LPM tries to exactly match the desired state.
func (tm *TunnelMap) Reconcile(desired map[string]TunnelEndpoint) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Split desired into v4 and v6.
	v4Desired := make(map[lpmKeyV4]TunnelEndpoint)
	v6Desired := make(map[lpmKeyV6]TunnelEndpoint)

	for cidrStr, ep := range desired {
		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			continue
		}

		if cidr.IP.To4() != nil {
			key, err := cidrToKeyV4(cidr)
			if err != nil {
				continue
			}

			v4Desired[key] = ep
		} else {
			key, err := cidrToKeyV6(cidr)
			if err != nil {
				continue
			}

			v6Desired[key] = ep
		}
	}

	// Reconcile IPv4 map.
	v4Stale, v4Added := reconcileMap(tm.lpmV4Map, v4Desired,
		func(ep TunnelEndpoint) interface{} { return epToV4C(ep) })

	// Reconcile IPv6 map.
	v6Stale, v6Added := reconcileMapV6(tm.lpmV6Map, v6Desired)

	klog.V(2).Infof("eBPF tunnel map reconciled: %d v4 + %d v6 entries (%d+%d stale removed)",
		v4Added, v6Added, v4Stale, v6Stale)

	// Save snapshot for SetPeerHealth.
	tm.lastReconciled = desired

	return nil
}

// reconcileMap reconciles the IPv4 LPM trie.
func reconcileMap(m *ebpf.Map, desired map[lpmKeyV4]TunnelEndpoint,
	toC func(TunnelEndpoint) interface{},
) (staleCount, addedCount int) {
	var (
		cursor lpmKeyV4
		val    tunnelEndpointV4C
	)

	iter := m.Iterate()

	var stale []lpmKeyV4

	for iter.Next(&cursor, &val) {
		if _, ok := desired[cursor]; !ok {
			stale = append(stale, cursor)
		}
	}

	for _, key := range stale {
		_ = m.Delete(key) //nolint:errcheck
	}

	for key, ep := range desired {
		if err := m.Update(key, toC(ep), ebpf.UpdateAny); err != nil {
			klog.Warningf("eBPF reconcile v4: update failed: %v", err)
		}
	}

	return len(stale), len(desired)
}

// reconcileMapV6 reconciles the IPv6 LPM trie.
func reconcileMapV6(m *ebpf.Map, desired map[lpmKeyV6]TunnelEndpoint) (staleCount, addedCount int) {
	var (
		cursor lpmKeyV6
		val    tunnelEndpointV6C
	)

	iter := m.Iterate()

	var stale []lpmKeyV6

	for iter.Next(&cursor, &val) {
		if _, ok := desired[cursor]; !ok {
			stale = append(stale, cursor)
		}
	}

	for _, key := range stale {
		_ = m.Delete(key) //nolint:errcheck
	}

	for key, ep := range desired {
		if err := m.Update(key, epToV6C(ep), ebpf.UpdateAny); err != nil {
			klog.Warningf("eBPF reconcile v6: update failed: %v", err)
		}
	}

	return len(stale), len(desired)
}

// SetPeerHealth toggles TUNNEL_F_HEALTHY on all BPF map nexthops belonging to
// the named peer. When healthy is false, the BPF program skips the nexthop and
// falls through to kernel routing (effectively withdrawing the peer). Returns
// the number of map entries updated.
func (tm *TunnelMap) SetPeerHealth(peerName string, healthy bool) int {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	updated := 0

	for cidrStr, ep := range tm.lastReconciled {
		modified := false

		for i := range ep.Nexthops {
			if ep.Nexthops[i].PeerName != peerName {
				continue
			}

			if healthy {
				ep.Nexthops[i].Flags |= TunnelFlagHealthy
			} else {
				ep.Nexthops[i].Flags &^= TunnelFlagHealthy
			}

			modified = true
		}

		if !modified {
			continue
		}

		tm.lastReconciled[cidrStr] = ep

		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			continue
		}

		if cidr.IP.To4() != nil {
			key, err := cidrToKeyV4(cidr)
			if err != nil {
				continue
			}

			if err := tm.lpmV4Map.Update(key, epToV4C(ep), ebpf.UpdateAny); err != nil {
				klog.Warningf("eBPF SetPeerHealth v4 %s: %v", cidrStr, err)
				continue
			}
		} else {
			key, err := cidrToKeyV6(cidr)
			if err != nil {
				continue
			}

			if err := tm.lpmV6Map.Update(key, epToV6C(ep), ebpf.UpdateAny); err != nil {
				klog.Warningf("eBPF SetPeerHealth v6 %s: %v", cidrStr, err)
				continue
			}
		}

		updated++
	}

	return updated
}

// Close detaches TC filters and releases eBPF resources.
func (tm *TunnelMap) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	for ifIdx := range tm.attachedIfs {
		egressFilter := &netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: ifIdx,
				Handle:    1,
				Parent:    netlink.HANDLE_MIN_EGRESS,
				Priority:  1,
				Protocol:  unix.ETH_P_ALL,
			},
		}
		_ = netlink.FilterDel(egressFilter) //nolint:errcheck
	}

	if tm.coll != nil {
		tm.coll.Close()
	}

	return nil
}

// cidrToKeyV4 converts an IPv4 CIDR to an LPM trie key.
// Uses LittleEndian so raw memory layout preserves network byte order.
func cidrToKeyV4(cidr *net.IPNet) (lpmKeyV4, error) {
	ip4 := cidr.IP.To4()
	if ip4 == nil {
		return lpmKeyV4{}, fmt.Errorf("not an IPv4 CIDR: %s", cidr)
	}

	ones, _ := cidr.Mask.Size()

	return lpmKeyV4{
		Prefixlen: uint32(ones),
		Addr:      binary.LittleEndian.Uint32(ip4),
	}, nil
}

// cidrToKeyV6 converts an IPv6 CIDR to an LPM trie key.
func cidrToKeyV6(cidr *net.IPNet) (lpmKeyV6, error) {
	ip16 := cidr.IP.To16()
	if ip16 == nil {
		return lpmKeyV6{}, fmt.Errorf("not an IPv6 CIDR: %s", cidr)
	}

	ones, _ := cidr.Mask.Size()

	var key lpmKeyV6

	key.Prefixlen = uint32(ones)
	// Store each 4-byte segment preserving network byte order in memory.
	key.Addr[0] = binary.LittleEndian.Uint32(ip16[0:4])
	key.Addr[1] = binary.LittleEndian.Uint32(ip16[4:8])
	key.Addr[2] = binary.LittleEndian.Uint32(ip16[8:12])
	key.Addr[3] = binary.LittleEndian.Uint32(ip16[12:16])

	return key, nil
}

// epToV4C converts a TunnelEndpoint to the IPv4 BPF struct.
func epToV4C(ep TunnelEndpoint) tunnelEndpointV4C {
	var c tunnelEndpointV4C

	for i, nh := range ep.Nexthops {
		if i >= MaxNexthops {
			break
		}

		ip4 := nh.RemoteIP.To4()

		var remote uint32
		if ip4 != nil {
			remote = binary.BigEndian.Uint32(ip4)
		}

		c.Nexthops[i] = tunnelNexthopC{
			RemoteIPv4: remote,
			VNI:        nh.VNI,
			IfIndex:    nh.IfIndex,
			Flags:      nh.Flags,
			Protocol:   nh.Protocol,
		}
		c.Count++
	}

	return c
}

// epToV6C converts a TunnelEndpoint to the IPv6 BPF struct.
// For each nexthop, if the remote IP is IPv4, stores it in RemoteIPv6[0]
// (union with remote_ipv4). If the remote IP is IPv6, stores all 16 bytes
// and sets TUNNEL_F_IPV6_UNDERLAY.
func epToV6C(ep TunnelEndpoint) tunnelEndpointV6C {
	var c tunnelEndpointV6C

	for i, nh := range ep.Nexthops {
		if i >= MaxNexthops {
			break
		}

		var nhC tunnelNexthopV6C

		nhC.VNI = nh.VNI
		nhC.IfIndex = nh.IfIndex
		nhC.Flags = nh.Flags
		nhC.Protocol = nh.Protocol

		if ip4 := nh.RemoteIP.To4(); ip4 != nil {
			// IPv4 underlay: store in first element of union (remote_ipv4 position).
			nhC.RemoteIPv6[0] = binary.BigEndian.Uint32(ip4)
		} else if ip16 := nh.RemoteIP.To16(); ip16 != nil {
			// IPv6 underlay: store all 16 bytes and set the IPv6 flag.
			nhC.RemoteIPv6[0] = binary.LittleEndian.Uint32(ip16[0:4])
			nhC.RemoteIPv6[1] = binary.LittleEndian.Uint32(ip16[4:8])
			nhC.RemoteIPv6[2] = binary.LittleEndian.Uint32(ip16[8:12])
			nhC.RemoteIPv6[3] = binary.LittleEndian.Uint32(ip16[12:16])
			nhC.Flags |= TunnelFlagIPv6Underlay
		}

		c.Nexthops[i] = nhC
		c.Count++
	}

	return c
}

// TunnelMACFromIP derives a locally-administered MAC address from an IP.
// IPv4: 02:<ip[0]>:<ip[1]>:<ip[2]>:<ip[3]>:FF.
// IPv6: 02:<ip[12]>:<ip[13]>:<ip[14]>:<ip[15]>:FF (last 4 bytes).
func TunnelMACFromIP(ip net.IP) net.HardwareAddr {
	if ip4 := ip.To4(); ip4 != nil {
		return net.HardwareAddr{0x02, ip4[0], ip4[1], ip4[2], ip4[3], 0xFF}
	}

	ip16 := ip.To16()
	if ip16 != nil {
		return net.HardwareAddr{0x02, ip16[12], ip16[13], ip16[14], ip16[15], 0xFF}
	}

	return net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0xFF}
}

// isErrExist checks for EEXIST from netlink.
func isErrExist(err error) bool {
	var errno unix.Errno
	return errors.As(err, &errno) && errno == unix.EEXIST
}
