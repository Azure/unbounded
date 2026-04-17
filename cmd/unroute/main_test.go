// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestProtocolName(t *testing.T) {
	tests := []struct {
		proto uint32
		want  string
	}{
		{1, "GENEVE"},
		{2, "VXLAN"},
		{3, "IPIP"},
		{4, "WireGuard"},
		{0, "unknown(0)"},
		{99, "unknown(99)"},
	}
	for _, tt := range tests {
		got := protocolName(tt.proto)
		if got != tt.want {
			t.Errorf("protocolName(%d) = %q, want %q", tt.proto, got, tt.want)
		}
	}
}

func TestUint32ToIPv4LE(t *testing.T) {
	// 100.80.5.0 stored via LittleEndian.Uint32 = 0x00055064
	ip := net.IP{100, 80, 5, 0}
	val := binary.LittleEndian.Uint32(ip)

	got := uint32ToIPv4LE(val)
	if !got.Equal(ip) {
		t.Errorf("uint32ToIPv4LE(0x%08x) = %s, want %s", val, got, ip)
	}
}

func TestUint32ToIPv4BE(t *testing.T) {
	// 100.64.128.107 stored via BigEndian.Uint32 = 0x6440806b
	ip := net.IP{100, 64, 128, 107}
	val := binary.BigEndian.Uint32(ip)

	got := uint32ToIPv4BE(val)
	if !got.Equal(ip) {
		t.Errorf("uint32ToIPv4BE(0x%08x) = %s, want %s", val, got, ip)
	}
}

func TestUint32ArrayToIPv6(t *testing.T) {
	// fd01::5:0:0:1 = fd01:0000:0000:0005:0000:0000:0000:0001
	ip := net.ParseIP("fd01::5:0:0:1").To16()

	var arr [4]uint32

	arr[0] = binary.LittleEndian.Uint32(ip[0:4])
	arr[1] = binary.LittleEndian.Uint32(ip[4:8])
	arr[2] = binary.LittleEndian.Uint32(ip[8:12])
	arr[3] = binary.LittleEndian.Uint32(ip[12:16])

	got := uint32ArrayToIPv6(arr)
	if !got.Equal(ip) {
		t.Errorf("uint32ArrayToIPv6(%v) = %s, want %s", arr, got, ip)
	}
}

func TestMakeEntriesV4(t *testing.T) {
	key := lpmKeyV4{
		Prefixlen: 24,
		Addr:      binary.LittleEndian.Uint32(net.IP{100, 80, 5, 0}),
	}
	val := tunnelEndpointV4{
		Count: 1,
	}
	val.Nexthops[0] = tunnelNexthopV4{
		RemoteIPv4: binary.BigEndian.Uint32(net.IP{100, 64, 128, 107}),
		VNI:        1,
		IfIndex:    4,
		Flags:      3,
		Protocol:   1, // GENEVE
	}

	entries := makeEntriesV4(key, val)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}

	e := entries[0]
	if e.CIDR != "100.80.5.0/24" {
		t.Errorf("CIDR = %q, want 100.80.5.0/24", e.CIDR)
	}

	if e.Remote != "100.64.128.107" {
		t.Errorf("Remote = %q, want 100.64.128.107", e.Remote)
	}

	if e.Protocol != "GENEVE" {
		t.Errorf("Protocol = %q, want GENEVE", e.Protocol)
	}

	if e.VNI != 1 {
		t.Errorf("VNI = %d, want 1", e.VNI)
	}
}

func TestMakeEntriesV6(t *testing.T) {
	ip6 := net.ParseIP("fd01::5:0:0:0").To16()

	var addr [4]uint32

	addr[0] = binary.LittleEndian.Uint32(ip6[0:4])
	addr[1] = binary.LittleEndian.Uint32(ip6[4:8])
	addr[2] = binary.LittleEndian.Uint32(ip6[8:12])
	addr[3] = binary.LittleEndian.Uint32(ip6[12:16])

	key := lpmKeyV6{Prefixlen: 80, Addr: addr}

	t.Run("IPv4 underlay", func(t *testing.T) {
		val := tunnelEndpointV6{
			Count: 1,
		}
		val.Nexthops[0] = tunnelNexthopV6{
			RemoteIPv6: [4]uint32{binary.BigEndian.Uint32(net.IP{100, 64, 128, 107}), 0, 0, 0},
			VNI:        1,
			IfIndex:    4,
			Flags:      3, // SET_KEY | HEALTHY, no IPV6_UNDERLAY
			Protocol:   1, // GENEVE
		}

		entries := makeEntriesV6(key, val)
		if len(entries) != 1 {
			t.Fatalf("got %d entries, want 1", len(entries))
		}

		e := entries[0]
		if e.CIDR != "fd01::5:0:0:0/80" {
			t.Errorf("CIDR = %q, want fd01::5:0:0:0/80", e.CIDR)
		}

		if e.Remote != "100.64.128.107" {
			t.Errorf("Remote = %q, want 100.64.128.107", e.Remote)
		}
	})

	t.Run("IPv6 underlay", func(t *testing.T) {
		remoteIPv6 := net.ParseIP("fd00::6b").To16()

		var remote [4]uint32

		remote[0] = binary.LittleEndian.Uint32(remoteIPv6[0:4])
		remote[1] = binary.LittleEndian.Uint32(remoteIPv6[4:8])
		remote[2] = binary.LittleEndian.Uint32(remoteIPv6[8:12])
		remote[3] = binary.LittleEndian.Uint32(remoteIPv6[12:16])

		val := tunnelEndpointV6{
			Count: 1,
		}
		val.Nexthops[0] = tunnelNexthopV6{
			RemoteIPv6: remote,
			VNI:        1,
			IfIndex:    4,
			Flags:      7, // SET_KEY | HEALTHY | IPV6_UNDERLAY
			Protocol:   1, // GENEVE
		}

		entries := makeEntriesV6(key, val)
		if len(entries) != 1 {
			t.Fatalf("got %d entries, want 1", len(entries))
		}

		e := entries[0]
		if e.Remote != "fd00::6b" {
			t.Errorf("Remote = %q, want fd00::6b", e.Remote)
		}
	})
}

func TestFormatIP(t *testing.T) {
	tests := []struct {
		ip   string
		want string
	}{
		{"100.80.5.1", "100.80.5.1"},
		{"fd01::5:0:0:1", "fd01::5:0:0:1"},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)

		got := formatIP(ip)
		if got != tt.want {
			t.Errorf("formatIP(%s) = %q, want %q", tt.ip, got, tt.want)
		}
	}
}

func TestAnnotateEntries(t *testing.T) {
	// annotateEntries with unreachable status port should be a no-op
	entries := []entry{
		{CIDR: "100.80.5.0/24", Remote: "100.64.128.107"},
		{CIDR: "100.80.1.0/24", Remote: "100.64.128.100"},
	}
	// Use a port that nothing is listening on
	annotateEntries(entries, 19999)

	if entries[0].Node != "" {
		t.Errorf("expected empty node, got %q", entries[0].Node)
	}
}
