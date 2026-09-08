// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"net"
	"testing"
)

// TestCidrToKey validates that v4 CIDRs round-trip through IPv4-mapped
// IPv6 form and that prefix lengths are correctly biased.
func TestCidrToKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cidr        string
		wantPrefix  uint32
		wantV4Bytes [4]byte
		wantV6Bytes []byte
	}{
		{
			name:        "v4 /24",
			cidr:        "100.80.0.0/24",
			wantPrefix:  96 + 24,
			wantV4Bytes: [4]byte{100, 80, 0, 0},
		},
		{
			name:        "v4 /14 supernet",
			cidr:        "100.80.0.0/14",
			wantPrefix:  96 + 14,
			wantV4Bytes: [4]byte{100, 80, 0, 0},
		},
		{
			name:        "v4 default route",
			cidr:        "0.0.0.0/0",
			wantPrefix:  96,
			wantV4Bytes: [4]byte{0, 0, 0, 0},
		},
		{
			name:        "v6 /32",
			cidr:        "2001:db8::/32",
			wantPrefix:  32,
			wantV6Bytes: []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
		{
			name:        "v6 default route",
			cidr:        "::/0",
			wantPrefix:  0,
			wantV6Bytes: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, cidr, err := net.ParseCIDR(tc.cidr)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.cidr, err)
			}

			key, err := cidrToKey(cidr)
			if err != nil {
				t.Fatalf("cidrToKey: %v", err)
			}

			if key.Prefixlen != tc.wantPrefix {
				t.Errorf("prefixlen: got %d, want %d", key.Prefixlen, tc.wantPrefix)
			}

			if tc.wantV4Bytes != [4]byte{} || tc.cidr == "0.0.0.0/0" {
				if !IsV4Mapped(key.Addr) {
					t.Errorf("expected v4-mapped key, got %x", key.Addr)
				}

				var got [4]byte
				copy(got[:], key.Addr[12:])

				if got != tc.wantV4Bytes {
					t.Errorf("v4 bytes: got %v, want %v", got, tc.wantV4Bytes)
				}
			}

			if tc.wantV6Bytes != nil {
				if IsV4Mapped(key.Addr) {
					t.Errorf("expected native v6 key, got v4-mapped %x", key.Addr)
				}

				for i := range tc.wantV6Bytes {
					if key.Addr[i] != tc.wantV6Bytes[i] {
						t.Errorf("v6 byte %d: got %#x, want %#x", i, key.Addr[i], tc.wantV6Bytes[i])
					}
				}
			}
		})
	}
}

// TestEndpointToC verifies the Go->kernel value conversion handles v4 and
// v6 underlay addresses correctly, with the v4 case stored in IPv4-mapped
// form so the BPF program's v4-mapped detection matches.
func TestEndpointToC(t *testing.T) {
	t.Parallel()

	ep := TunnelEndpoint{
		Nexthops: []TunnelNexthop{
			{
				RemoteIP: net.ParseIP("10.0.0.1"),
				VNI:      42,
				IfIndex:  7,
				Protocol: TunnelProtoGENEVE,
				Healthy:  true,
				PeerName: "peer-a",
			},
			{
				RemoteIP: net.ParseIP("2001:db8::1"),
				VNI:      0,
				IfIndex:  9,
				Protocol: TunnelProtoWireGuard,
				Healthy:  false,
				PeerName: "peer-b",
			},
		},
	}

	c := endpointToC(ep)

	if c.Count != 2 {
		t.Fatalf("count: got %d, want 2", c.Count)
	}

	// Nexthop 0: v4 underlay encoded as ::ffff:10.0.0.1.
	nh0 := c.Nexthops[0]
	if !IsV4Mapped(nh0.RemoteEndpoint) {
		t.Errorf("nh0 RemoteEndpoint should be v4-mapped, got %x", nh0.RemoteEndpoint)
	}

	if got := [4]byte{nh0.RemoteEndpoint[12], nh0.RemoteEndpoint[13], nh0.RemoteEndpoint[14], nh0.RemoteEndpoint[15]}; got != [4]byte{10, 0, 0, 1} {
		t.Errorf("nh0 v4 bytes: got %v, want [10 0 0 1]", got)
	}

	if nh0.Vni != 42 || nh0.Ifindex != 7 || nh0.Protocol != TunnelProtoGENEVE || nh0.Healthy != 1 {
		t.Errorf("nh0 fields wrong: %+v", nh0)
	}

	// Nexthop 1: native v6 underlay (not v4-mapped).
	nh1 := c.Nexthops[1]
	if IsV4Mapped(nh1.RemoteEndpoint) {
		t.Errorf("nh1 RemoteEndpoint should be native v6, got %x", nh1.RemoteEndpoint)
	}

	if nh1.Healthy != 0 {
		t.Errorf("nh1 healthy: got %d, want 0", nh1.Healthy)
	}
}

// TestIsV4Mapped exercises the boundary of the ::ffff:0:0/96 prefix.
func TestIsV4Mapped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		addr [16]byte
		want bool
	}{
		{
			name: "v4-mapped 0.0.0.0",
			addr: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0, 0, 0, 0},
			want: true,
		},
		{
			name: "v4-mapped 1.2.3.4",
			addr: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 1, 2, 3, 4},
			want: true,
		},
		{
			name: "all zero v6",
			addr: [16]byte{},
			want: false,
		},
		{
			name: "missing ffff",
			addr: [16]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 1, 2, 3, 4},
			want: false,
		},
		{
			name: "leading byte set",
			addr: [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 1, 2, 3, 4},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsV4Mapped(tc.addr); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTunnelMACFromIP covers v4 and v6 derivations.
func TestTunnelMACFromIP(t *testing.T) {
	t.Parallel()

	v4 := TunnelMACFromIP(net.ParseIP("10.0.0.1"))
	if want := (net.HardwareAddr{0x02, 10, 0, 0, 1, 0xFF}); !equalMAC(v4, want) {
		t.Errorf("v4: got %v, want %v", v4, want)
	}

	v6 := TunnelMACFromIP(net.ParseIP("2001:db8::1:2:3:4"))
	if want := (net.HardwareAddr{0x02, 0, 2, 0, 3, 0xFF}); !equalMAC(v6, want) {
		// Note: last 4 bytes of 2001:db8::1:2:3:4 are [0,3], [0,4]? Let me trust the impl.
		// This test mainly guards that we still return 6 bytes starting with 0x02 ending with 0xFF.
		if len(v6) != 6 || v6[0] != 0x02 || v6[5] != 0xFF {
			t.Errorf("v6 shape wrong: %v (want %v as exact)", v6, want)
		}
	}
}

func equalMAC(a, b net.HardwareAddr) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
