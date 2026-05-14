// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"net"
	"testing"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
)

// TestFormatKey covers the LPM key -> CIDR string rendering. v4 entries
// are stored in IPv4-mapped IPv6 form with prefix length offset by 96;
// formatKey must invert that.
func TestFormatKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		prefix    uint32
		addr      [16]byte
		want      string
		wantFamV4 bool
	}{
		{
			name:      "v4 /24",
			prefix:    96 + 24,
			addr:      [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 100, 80, 0, 0},
			want:      "100.80.0.0/24",
			wantFamV4: true,
		},
		{
			name:      "v4 default",
			prefix:    96,
			addr:      [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0, 0, 0, 0},
			want:      "0.0.0.0/0",
			wantFamV4: true,
		},
		{
			name:   "v6 /32",
			prefix: 32,
			addr:   [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			want:   "2001:db8::/32",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := ebpfpkg.LpmKey{Prefixlen: tc.prefix, Addr: tc.addr}
			got := formatKey(key)
			if got != tc.want {
				t.Errorf("formatKey: got %q, want %q", got, tc.want)
			}

			fam := familyOfKey(key)
			if (fam == "v4") != tc.wantFamV4 {
				t.Errorf("familyOfKey: got %q (wantV4=%v)", fam, tc.wantFamV4)
			}
		})
	}
}

// TestFormatEndpoint covers the underlay address rendering, including
// the IPv4-mapped detection.
func TestFormatEndpoint(t *testing.T) {
	t.Parallel()

	v4 := [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 10, 0, 0, 1}
	if got := formatEndpoint(v4); got != "10.0.0.1" {
		t.Errorf("v4-mapped: got %q", got)
	}

	v6 := [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	if got := formatEndpoint(v6); got != "2001:db8::1" {
		t.Errorf("native v6: got %q", got)
	}
}

// TestFamilyMatches covers the -4 / -6 filter semantics.
func TestFamilyMatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		filter int
		family string
		want   bool
	}{
		{familyAll, "v4", true},
		{familyAll, "v6", true},
		{familyV4, "v4", true},
		{familyV4, "v6", false},
		{familyV6, "v4", false},
		{familyV6, "v6", true},
	}

	for _, tc := range cases {
		if got := familyMatches(tc.filter, tc.family); got != tc.want {
			t.Errorf("filter=%d family=%q: got %v, want %v", tc.filter, tc.family, got, tc.want)
		}
	}
}

// TestProtocolName covers the protocol-id -> name mapping.
func TestProtocolName(t *testing.T) {
	t.Parallel()

	cases := map[uint32]string{
		ebpfpkg.TunnelProtoGENEVE:    "GENEVE",
		ebpfpkg.TunnelProtoVXLAN:     "VXLAN",
		ebpfpkg.TunnelProtoIPIP:      "IPIP",
		ebpfpkg.TunnelProtoWireGuard: "WireGuard",
		ebpfpkg.TunnelProtoNone:      "None",
		99:                           "unknown(99)",
	}

	for proto, want := range cases {
		if got := protocolName(proto); got != want {
			t.Errorf("protocolName(%d): got %q, want %q", proto, got, want)
		}
	}
}

// TestMakeEntries verifies that the per-nexthop expansion produces one
// entry per healthy/unhealthy nexthop, with v4-mapped underlays
// formatted as dotted-quad.
func TestMakeEntries(t *testing.T) {
	t.Parallel()

	v4Mapped := func(a, b, c, d byte) [16]byte {
		return [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, a, b, c, d}
	}

	key := ebpfpkg.LpmKey{Prefixlen: 96 + 24, Addr: v4Mapped(100, 80, 0, 0)}

	val := ebpfpkg.RawTunnelEndpoint{Count: 2}
	val.Nexthops[0].RemoteEndpoint = v4Mapped(10, 0, 0, 1)
	val.Nexthops[0].Vni = 42
	val.Nexthops[0].Healthy = 1
	val.Nexthops[0].Protocol = ebpfpkg.TunnelProtoGENEVE

	val.Nexthops[1].RemoteEndpoint = v4Mapped(10, 0, 0, 2)
	val.Nexthops[1].Vni = 0
	val.Nexthops[1].Healthy = 0
	val.Nexthops[1].Protocol = ebpfpkg.TunnelProtoWireGuard

	entries := makeEntries(key, val, familyAll)
	if len(entries) != 2 {
		t.Fatalf("entries: got %d, want 2", len(entries))
	}

	if entries[0].CIDR != "100.80.0.0/24" {
		t.Errorf("entries[0].CIDR: %q", entries[0].CIDR)
	}

	if entries[0].Remote != "10.0.0.1" {
		t.Errorf("entries[0].Remote: %q", entries[0].Remote)
	}

	if !entries[0].Healthy {
		t.Errorf("entries[0] should be healthy")
	}

	if entries[1].Healthy {
		t.Errorf("entries[1] should be unhealthy")
	}

	if entries[0].Family != "v4" {
		t.Errorf("entries[0].Family: %q", entries[0].Family)
	}

	// Family filter -- v6Only should hide v4 entries entirely.
	filtered := makeEntries(key, val, familyV6)
	if len(filtered) != 0 {
		t.Errorf("v6Only filter: got %d entries, want 0", len(filtered))
	}
}

// Avoid unused-import linter complaints when the package compiles without
// using net here (defensive in case of refactors).
var _ = net.IPv4
