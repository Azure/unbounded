// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netlink

import (
	"net"
	"syscall"
	"testing"

	vishnetlink "github.com/vishvananda/netlink"
)

func makeFlow(proto uint8, fSrc, fDst, rSrc, rDst string, fSp, fDp, rSp, rDp uint16) *vishnetlink.ConntrackFlow {
	return &vishnetlink.ConntrackFlow{
		Forward: vishnetlink.IPTuple{
			Protocol: proto,
			SrcIP:    net.ParseIP(fSrc),
			DstIP:    net.ParseIP(fDst),
			SrcPort:  fSp,
			DstPort:  fDp,
		},
		Reverse: vishnetlink.IPTuple{
			Protocol: proto,
			SrcIP:    net.ParseIP(rSrc),
			DstIP:    net.ParseIP(rDst),
			SrcPort:  rSp,
			DstPort:  rDp,
		},
	}
}

func TestStaleTransitUDPFilter_MatchesUntranslatedTransit(t *testing.T) {
	f := newStaleTransitUDPFilter([]net.IP{net.ParseIP("172.18.10.252"), net.ParseIP("10.132.164.4")})

	// spark (172.18.10.7) → AKS (52.138.2.134) un-NAT'd: tuples are mirror.
	flow := makeFlow(syscall.IPPROTO_UDP, "172.18.10.7", "52.138.2.134", "52.138.2.134", "172.18.10.7", 51821, 51820, 51820, 51821)
	if !f.MatchConntrackFlow(flow) {
		t.Fatal("expected mirror UDP transit flow to match (stale, un-NAT'd)")
	}
}

func TestStaleTransitUDPFilter_SkipsNATTranslated(t *testing.T) {
	f := newStaleTransitUDPFilter([]net.IP{net.ParseIP("10.132.164.4")})

	// spark → AKS with SNAT applied: Reverse.DstIP is the post-NAT source.
	flow := makeFlow(syscall.IPPROTO_UDP, "172.18.10.7", "52.138.2.134", "52.138.2.134", "10.132.164.4", 51821, 51820, 51820, 63470)
	if f.MatchConntrackFlow(flow) {
		t.Fatal("expected NAT-translated flow to be skipped")
	}
}

func TestStaleTransitUDPFilter_SkipsTCP(t *testing.T) {
	f := newStaleTransitUDPFilter(nil)

	flow := makeFlow(syscall.IPPROTO_TCP, "172.18.10.7", "52.138.2.134", "52.138.2.134", "172.18.10.7", 5555, 80, 80, 5555)
	if f.MatchConntrackFlow(flow) {
		t.Fatal("expected TCP flow to be skipped")
	}
}

func TestStaleTransitUDPFilter_SkipsLocalSource(t *testing.T) {
	local := net.ParseIP("172.18.10.252")
	f := newStaleTransitUDPFilter([]net.IP{local})

	flow := makeFlow(syscall.IPPROTO_UDP, "172.18.10.252", "52.138.2.134", "52.138.2.134", "172.18.10.252", 9999, 51820, 51820, 9999)
	if f.MatchConntrackFlow(flow) {
		t.Fatal("expected flow originating from local IP to be skipped")
	}
}

func TestStaleTransitUDPFilter_SkipsLocalDestination(t *testing.T) {
	local := net.ParseIP("172.18.10.252")
	f := newStaleTransitUDPFilter([]net.IP{local})

	flow := makeFlow(syscall.IPPROTO_UDP, "10.0.0.1", "172.18.10.252", "172.18.10.252", "10.0.0.1", 5555, 53, 53, 5555)
	if f.MatchConntrackFlow(flow) {
		t.Fatal("expected flow destined to local IP to be skipped")
	}
}

func TestStaleTransitUDPFilter_SkipsNilFlow(t *testing.T) {
	f := newStaleTransitUDPFilter(nil)
	if f.MatchConntrackFlow(nil) {
		t.Fatal("expected nil flow to be skipped")
	}
}

func TestStaleTransitUDPFilter_SkipsPortMismatch(t *testing.T) {
	f := newStaleTransitUDPFilter(nil)

	// Same IPs but ports don't mirror -> not a clean un-NAT'd entry.
	flow := makeFlow(syscall.IPPROTO_UDP, "172.18.10.7", "52.138.2.134", "52.138.2.134", "172.18.10.7", 51821, 51820, 51820, 9999)
	if f.MatchConntrackFlow(flow) {
		t.Fatal("expected port-mismatched flow to be skipped")
	}
}
