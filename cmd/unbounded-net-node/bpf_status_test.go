// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"testing"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
)

func TestBpfInterfaceResolverSnapshot(t *testing.T) {
	calls := make(map[int]int)
	current := net.Interface{Name: "wg51820", MTU: 1400}
	lookup := func(index int) (*net.Interface, error) {
		calls[index]++
		if index == 9 && calls[index] == 1 {
			return nil, errors.New("interface disappeared")
		}

		return &current, nil
	}

	resolve := newBpfInterfaceResolver(lookup)
	for range 2000 {
		name, mtu := resolve(7)
		if name != "wg51820" || mtu != 1400 {
			t.Fatalf("unexpected interface: %s, %d", name, mtu)
		}
	}

	if calls[7] != 1 {
		t.Fatalf("repeated interface lookup: %d calls", calls[7])
	}

	if name, mtu := resolve(9); name != "if9" || mtu != 0 {
		t.Fatalf("missing-interface fallback changed: %s, %d", name, mtu)
	}

	if name, mtu := resolve(9); name != "wg51820" || mtu != 1400 || calls[9] != 2 {
		t.Fatalf("failed lookup was not retried: %s, %d, %v", name, mtu, calls)
	}

	current.Name, current.MTU = "wg51821", 1500

	if name, mtu := resolve(7); name != "wg51820" || mtu != 1400 {
		t.Fatal("cached interface metadata changed within the snapshot")
	}

	next := newBpfInterfaceResolver(lookup)
	if name, mtu := next(7); name != "wg51821" || mtu != 1500 || calls[7] != 2 {
		t.Fatalf("next snapshot retained stale metadata: %s, %d, %v", name, mtu, calls)
	}
}

func TestBpfAppendEntries(t *testing.T) {
	for _, tc := range []struct {
		name, address, remote, cidr string
		prefix                      uint32
	}{
		{"v4", "10.1.2.0", "192.0.2.1", "10.1.2.0/24", 120},
		{"v6", "fd00:1::", "2001:db8::1", "fd00:1::/64", 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := ebpfpkg.LpmKey{Addr: netip.MustParseAddr(tc.address).As16(), Prefixlen: tc.prefix}

			var value ebpfpkg.RawTunnelEndpoint

			value.Count = 2
			value.Nexthops[0].Ifindex = 7
			value.Nexthops[0].RemoteEndpoint = netip.MustParseAddr(tc.remote).As16()
			value.Nexthops[0].Protocol = ebpfpkg.TunnelProtoGENEVE
			value.Nexthops[0].Healthy = 1
			value.Nexthops[0].Vni = 42
			value.Nexthops[1] = value.Nexthops[0]
			value.Nexthops[1].Healthy = 0
			value.Nexthops[1].Protocol = 123

			resolver := newBpfInterfaceResolver(func(int) (*net.Interface, error) {
				return &net.Interface{Name: "ugn0", MTU: 1400}, nil
			})
			prefix := BpfEntry{CIDR: "existing"}
			got := bpfAppendEntries([]BpfEntry{prefix}, key, value, resolver)

			want := []BpfEntry{
				prefix,
				{CIDR: tc.cidr, Remote: tc.remote, Interface: "ugn0", MTU: 1400, IfIndex: 7, VNI: 42, Protocol: "GENEVE", Healthy: true},
				{CIDR: tc.cidr, Remote: tc.remote, Interface: "ugn0", MTU: 1400, IfIndex: 7, VNI: 42, Protocol: "unknown(123)", Healthy: false},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("entry fields or append order changed: got %+v, want %+v", got, want)
			}

			value.Count = 0
			if got := bpfAppendEntries(nil, key, value, resolver); len(got) != 0 {
				t.Fatalf("zero nexthops produced entries: %+v", got)
			}

			value.Count = ^uint32(0)
			if got := bpfAppendEntries(nil, key, value, resolver); len(got) != ebpfpkg.MaxNexthops {
				t.Fatalf("nexthop bound changed: %d", len(got))
			}
		})
	}
}

func BenchmarkBpfEntryCollection(b *testing.B) {
	for _, count := range []int{10, 2000} {
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("entries-%d/cached-%t", count, cached), func(b *testing.B) {
				key := ebpfpkg.LpmKey{Addr: netip.MustParseAddr("10.1.0.0").As16(), Prefixlen: 120}

				var value ebpfpkg.RawTunnelEndpoint

				value.Count = 1
				value.Nexthops[0].Ifindex = 7
				lookups := 0
				lookup := func(int) (*net.Interface, error) {
					lookups++
					return &net.Interface{Name: "wg51820", MTU: 1400}, nil
				}

				b.ReportAllocs()

				for b.Loop() {
					var entries []BpfEntry

					resolver := func(index uint32) (string, int) {
						iface, err := lookup(int(index))
						if err != nil {
							b.Fatal(err)
						}

						return iface.Name, iface.MTU
					}
					if cached {
						resolver = newBpfInterfaceResolver(lookup)
					}

					for range count {
						if cached {
							entries = bpfAppendEntries(entries, key, value, resolver)
						} else {
							entries = append(entries, bpfAppendEntries(make([]BpfEntry, 0, value.Count), key, value, resolver)...)
						}
					}

					if len(entries) != count {
						b.Fatal("incorrect entry count")
					}
				}

				b.ReportMetric(float64(lookups)/float64(b.N), "lookups/op")
			})
		}
	}
}
