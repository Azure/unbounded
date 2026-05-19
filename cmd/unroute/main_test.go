// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
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

// TestGroupByCIDR verifies that per-nexthop entries are collapsed under
// their shared CIDR into a single object with an endpoints array.
func TestGroupByCIDR(t *testing.T) {
	t.Parallel()

	in := []entry{
		{CIDR: "10.0.0.0/24", Family: "v4", Remote: "192.168.1.1", IfIndex: 7, Healthy: true},
		{CIDR: "10.0.0.0/24", Family: "v4", Remote: "192.168.1.2", IfIndex: 8, Healthy: false},
		{CIDR: "2001:db8::/32", Family: "v6", Remote: "fe80::1", IfIndex: 9, Healthy: true},
	}

	groups := groupByCIDR(in)
	if len(groups) != 2 {
		t.Fatalf("groups: got %d, want 2", len(groups))
	}

	if groups[0].CIDR != "10.0.0.0/24" || groups[0].Family != "v4" {
		t.Errorf("groups[0]: %+v", groups[0])
	}

	if len(groups[0].Endpoints) != 2 {
		t.Fatalf("groups[0].Endpoints: got %d, want 2", len(groups[0].Endpoints))
	}

	if groups[0].Endpoints[0].Remote != "192.168.1.1" || groups[0].Endpoints[0].IfIndex != 7 {
		t.Errorf("groups[0].Endpoints[0]: %+v", groups[0].Endpoints[0])
	}

	if groups[0].Endpoints[1].Healthy {
		t.Errorf("groups[0].Endpoints[1] should be unhealthy")
	}

	if groups[1].CIDR != "2001:db8::/32" || groups[1].Family != "v6" || len(groups[1].Endpoints) != 1 {
		t.Errorf("groups[1]: %+v", groups[1])
	}
}

// TestResolveColorMode covers the --color value parser.
func TestResolveColorMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"auto", false},
		{"always", false},
		{"never", false},
		{"", false}, // empty == auto
		{"yes", true},
		{"NO", true},
	}

	for _, tc := range cases {
		_, err := resolveColorMode(tc.mode, nil)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: gotErr=%v wantErr=%v", tc.mode, err, tc.wantErr)
		}
	}

	// always = on regardless of writer
	if on, _ := resolveColorMode("always", nil); !on {
		t.Errorf("always should yield on=true")
	}

	// never = off regardless of writer
	if on, _ := resolveColorMode("never", nil); on {
		t.Errorf("never should yield on=false")
	}
}

// TestRenderEndpoint covers the ip-route-style line builder. Only the
// trailing healthy/unhealthy tag should ever be colored.
func TestRenderEndpoint(t *testing.T) {
	t.Parallel()

	ep := endpointJSON{
		Remote:    "10.0.0.5",
		Node:      "gw1",
		Interface: "geneve0",
		Protocol:  "GENEVE",
		Healthy:   true,
		VNI:       42,
		MTU:       1500,
	}

	got := renderEndpoint(ep, false, textOptions{})
	for _, want := range []string{"via 10.0.0.5", "dev geneve0", "proto GENEVE", "node gw1", "vni 42", "mtu 1500"} {
		if !strings.Contains(got, want) {
			t.Errorf("single-nexthop render missing %q: %q", want, got)
		}
	}

	if strings.Contains(got, "nexthop ") || strings.Contains(got, "weight ") {
		t.Errorf("single-nexthop render should not have nexthop/weight: %q", got)
	}

	if !strings.HasSuffix(got, " healthy") {
		t.Errorf("single-nexthop render should end with ' healthy': %q", got)
	}

	got = renderEndpoint(ep, true, textOptions{})
	if !strings.HasPrefix(got, "nexthop ") || !strings.Contains(got, "weight 1") || !strings.HasSuffix(got, " healthy") {
		t.Errorf("multi-nexthop render shape wrong: %q", got)
	}

	ep.Healthy = false

	got = renderEndpoint(ep, false, textOptions{})
	if !strings.HasSuffix(got, " unhealthy") {
		t.Errorf("unhealthy render should end with ' unhealthy': %q", got)
	}

	ep.Healthy = true

	got = renderEndpoint(ep, false, textOptions{useColor: true})
	if !strings.HasSuffix(got, "\x1b[32mhealthy\x1b[0m") {
		t.Errorf("colored healthy tag wrong: %q", got)
	}

	if strings.Contains(got, "\x1b[3"+"2m10.0.0.5") || strings.Contains(got, "\x1b[3"+"1m10.0.0.5") {
		t.Errorf("remote IP should NOT be colored: %q", got)
	}

	ep.Healthy = false

	got = renderEndpoint(ep, true, textOptions{useColor: true})
	if !strings.HasSuffix(got, "\x1b[31munhealthy\x1b[0m") {
		t.Errorf("colored unhealthy tag wrong: %q", got)
	}
}
