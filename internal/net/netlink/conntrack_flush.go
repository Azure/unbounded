// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"fmt"
	"net"
	"syscall"

	"github.com/vishvananda/netlink"
)

// FlushStaleTransitUDPConntrack scans the conntrack table and removes UDP
// entries that are pure L2 transit and have not been NAT-translated.
// It returns the number of entries removed.
//
// Specifically, an entry is removed when all of the following hold:
//   - protocol is UDP
//   - the original and reply tuples are exact mirrors (no SNAT/DNAT applied)
//   - neither the original source nor the original destination is one of
//     the supplied local IPs
//
// This addresses a class of bugs caused by Linux bridges with
// bridge-nf-call-iptables=1: when a downstream host's L2 frames transit a
// bridge but are not routed by the bridge node, conntrack still tracks the
// flow as UNREPLIED with no NAT applied. If the downstream host's default
// route later changes to point at the bridge node, the same flow now SHOULD
// be SNAT'd by an existing MASQUERADE rule, but conntrack reuses the stale
// (un-NAT'd) entry and skips MASQUERADE -- packets leave with the wrong
// source address and the upstream peer never replies.
//
// Periodically calling this function clears such stale entries so the next
// packet for that flow takes a fresh path through the netfilter hooks.
// UDP-only is intentional: TCP flows reach ESTABLISHED via SYN/SYN-ACK and
// would be safe to leave; the staleness mostly affects connectionless UDP
// (WireGuard handshake, GENEVE keepalive, etc).
func FlushStaleTransitUDPConntrack(localIPs []net.IP) (uint, error) {
	filter := newStaleTransitUDPFilter(localIPs)

	// IPv4 only for now: conntrack is per-family in netlink, and the bridge
	// transit issue is overwhelmingly an IPv4 NAT problem.
	n, err := netlink.ConntrackDeleteFilter(netlink.ConntrackTable, syscall.AF_INET, filter)
	if err != nil {
		return 0, fmt.Errorf("conntrack delete: %w", err)
	}

	return n, nil
}

// staleTransitUDPFilter implements netlink.CustomConntrackFilter and matches
// stale L2-transit UDP entries (see FlushStaleTransitUDPConntrack).
type staleTransitUDPFilter struct {
	localIPs map[string]struct{}
}

func newStaleTransitUDPFilter(localIPs []net.IP) *staleTransitUDPFilter {
	set := make(map[string]struct{}, len(localIPs))
	for _, ip := range localIPs {
		if ip == nil {
			continue
		}

		set[ip.String()] = struct{}{}
	}

	return &staleTransitUDPFilter{localIPs: set}
}

// MatchConntrackFlow returns true when the flow matches our predicate and
// should be removed.
func (f *staleTransitUDPFilter) MatchConntrackFlow(flow *netlink.ConntrackFlow) bool {
	if flow == nil {
		return false
	}

	if flow.Forward.Protocol != syscall.IPPROTO_UDP {
		return false
	}

	// If SNAT was applied, Reverse.DstIP would be the post-NAT source IP
	// and would not equal Forward.SrcIP. Same logic in reverse for DNAT.
	if !flow.Forward.SrcIP.Equal(flow.Reverse.DstIP) {
		return false
	}

	if !flow.Forward.DstIP.Equal(flow.Reverse.SrcIP) {
		return false
	}

	if flow.Forward.SrcPort != flow.Reverse.DstPort {
		return false
	}

	if flow.Forward.DstPort != flow.Reverse.SrcPort {
		return false
	}

	// Skip entries originating from or destined to a local IP. Those are
	// legitimate flows where the node itself is the source or sink, and
	// should not be touched.
	if _, ok := f.localIPs[flow.Forward.SrcIP.String()]; ok {
		return false
	}

	if _, ok := f.localIPs[flow.Forward.DstIP.String()]; ok {
		return false
	}

	return true
}

// LocalIPv4Addresses returns the IPv4 addresses currently configured on
// non-loopback, non-link-local interfaces. It is a thin wrapper around
// netlink.AddrList intended for use with FlushStaleTransitUDPConntrack.
func LocalIPv4Addresses() ([]net.IP, error) {
	addrs, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list addresses: %w", err)
	}

	out := make([]net.IP, 0, len(addrs))

	for _, a := range addrs {
		if a.IP == nil {
			continue
		}

		if a.IP.IsLoopback() || a.IP.IsLinkLocalUnicast() {
			continue
		}

		out = append(out, a.IP)
	}

	return out, nil
}
