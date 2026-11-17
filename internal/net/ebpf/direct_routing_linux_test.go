//go:build linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ebpf

import (
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestNoneProtocolNativeNeighborResolution(t *testing.T) {
	runtime.LockOSThread()

	defer runtime.UnlockOSThread()

	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.Close() }()

	isolated, err := netns.New()
	if errors.Is(err, unix.EPERM) {
		t.Skip("requires network namespace privileges")
	}

	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = isolated.Close() }()
	defer func() {
		if err := netns.Set(original); err != nil {
			t.Fatal(err)
		}
	}()

	fabric := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "fabric0"}}
	overlay := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "unbounded0"}}

	for _, link := range []netlink.Link{fabric, overlay} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}

		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
	}

	for _, address := range []struct {
		link netlink.Link
		cidr string
	}{
		{fabric, "192.0.2.1/24"},
		{fabric, "2001:db8::1/64"},
		{overlay, "10.246.0.1/32"},
		{overlay, "fd46::1/128"},
	} {
		addr, err := netlink.ParseAddr(address.cidr)
		if err != nil {
			t.Fatal(err)
		}

		addr.Flags = unix.IFA_F_NODAD
		if err := netlink.AddrAdd(address.link, addr); err != nil {
			t.Fatal(err)
		}
	}

	neighborMAC := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}

	for _, gateway := range []string{"192.0.2.2", "2001:db8::2"} {
		ip := net.ParseIP(gateway)
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: fabric.Index, Gw: ip}); err != nil {
			t.Fatal(err)
		}

		if err := netlink.NeighSet(&netlink.Neigh{
			LinkIndex: fabric.Index, IP: ip, HardwareAddr: neighborMAC, State: netlink.NUD_PERMANENT,
		}); err != nil {
			t.Fatal(err)
		}
	}

	tunnelMap, err := NewTunnelMap(TunnelMapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tunnelMap.Close() }()

	if err := tunnelMap.AttachToInterface(overlay.Name); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, source, destination, prefix, endpoint string
		protocol                                    uint16
	}{
		{"IPv4", "10.246.0.1", "10.246.1.2", "10.246.1.0/24", "192.0.2.2", unix.ETH_P_IP},
		{"IPv6", "fd46::1", "fd46:0:0:1::2", "fd46:0:0:1::/64", "2001:db8::2", unix.ETH_P_IPV6},
		{"routed IPv4", "10.246.0.1", "10.246.2.2", "10.246.2.0/24", "198.51.100.2", unix.ETH_P_IP},
		{"routed IPv6", "fd46::1", "fd46:0:0:2::2", "fd46:0:0:2::/64", "2001:db8:1::2", unix.ETH_P_IPV6},
	} {
		t.Log(tc.name)

		func() {
			_, prefix, err := net.ParseCIDR(tc.prefix)
			if err != nil {
				t.Fatal(err)
			}

			if err := netlink.RouteAdd(&netlink.Route{
				LinkIndex: overlay.Index, Dst: prefix, Src: net.ParseIP(tc.source),
			}); err != nil {
				t.Fatal(err)
			}

			ep := TunnelEndpoint{Nexthops: []TunnelNexthop{{
				RemoteIP: net.ParseIP(tc.endpoint), IfIndex: uint32(fabric.Index), Protocol: TunnelProtoNone, Healthy: true, PeerName: tc.name,
			}}}

			var updateErr error
			if tc.name == "routed IPv4" || tc.name == "routed IPv6" {
				updateErr = tunnelMap.Reconcile(map[string]TunnelEndpoint{tc.prefix: ep})
				if updateErr == nil {
					tunnelMap.SetPeerHealth(tc.name, false)
					tunnelMap.SetPeerHealth(tc.name, true)
				}
			} else {
				updateErr = tunnelMap.UpdateEndpoint(prefix, ep)
			}

			if updateErr != nil {
				t.Fatal(updateErr)
			}

			for _, phase := range []string{"initial", "router replacement", "health flap"} {
				routed := tc.name == "routed IPv4" || tc.name == "routed IPv6"
				if phase != "initial" && !routed {
					continue
				}

				wantMAC := neighborMAC
				if phase != "initial" {
					wantMAC = net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
				}

				if phase == "router replacement" {
					router := net.ParseIP("192.0.2.3")
					if tc.protocol == unix.ETH_P_IPV6 {
						router = net.ParseIP("2001:db8::3")
					}

					if err := netlink.NeighSet(&netlink.Neigh{
						LinkIndex: fabric.Index, IP: router, HardwareAddr: wantMAC, State: netlink.NUD_PERMANENT,
					}); err != nil {
						t.Fatal(err)
					}

					if err := netlink.RouteReplace(&netlink.Route{LinkIndex: fabric.Index, Gw: router}); err != nil {
						t.Fatal(err)
					}

					if err := tunnelMap.RefreshNativeNexthops(); err != nil {
						t.Fatal(err)
					}
				}

				if phase == "health flap" {
					if tunnelMap.SetPeerHealth(tc.name, false) != 1 {
						t.Fatal("native peer health withdrawal was lost")
					}

					if err := tunnelMap.RefreshNativeNexthops(); err != nil {
						t.Fatal(err)
					}

					key, err := cidrToKey(prefix)
					if err != nil {
						t.Fatal(err)
					}

					var endpoint tunnelEndpointC
					if err := tunnelMap.endpoints.Lookup(key, &endpoint); err != nil || endpoint.Nexthops[0].Healthy != 0 {
						t.Fatalf("native refresh reset withdrawn peer health: %v, %+v", err, endpoint)
					}

					if tunnelMap.SetPeerHealth(tc.name, true) != 1 {
						t.Fatal("native peer health recovery was lost")
					}
				}

				if !tunnelMap.lastReconciled[tc.prefix].Nexthops[0].RemoteIP.Equal(net.ParseIP(tc.endpoint)) {
					t.Fatal("native resolution replaced the original peer address")
				}

				t.Log(tc.name, phase)

				fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
				if err != nil {
					t.Fatal(err)
				}

				defer func() { _ = unix.Close(fd) }()

				if err := unix.Bind(fd, &unix.SockaddrLinklayer{
					Ifindex: fabric.Index, Protocol: uint16(unix.ETH_P_ALL << 8),
				}); err != nil {
					t.Fatal(err)
				}

				if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
					t.Fatal(err)
				}

				conn, err := net.ListenPacket("udp", net.JoinHostPort(tc.source, "0"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()

				if _, err := conn.WriteTo([]byte("direct"), &net.UDPAddr{IP: net.ParseIP(tc.destination), Port: 8080}); err != nil {
					t.Fatal(err)
				}

				packet := make([]byte, 2048)

				n, _, err := unix.Recvfrom(fd, packet, 0)
				if err != nil {
					t.Fatalf("no native underlay packet: %v", err)
				}

				minimumLength := 34
				if tc.protocol == unix.ETH_P_IPV6 {
					minimumLength = 54
				}

				if n < minimumLength || binary.BigEndian.Uint16(packet[12:14]) != tc.protocol ||
					net.HardwareAddr(packet[:6]).String() != wantMAC.String() {
					t.Fatalf("incorrect native frame: %x", packet[:n])
				}

				source, destination := net.IP(packet[26:30]), net.IP(packet[30:34])
				if tc.protocol == unix.ETH_P_IPV6 {
					source, destination = net.IP(packet[22:38]), net.IP(packet[38:54])
				}

				if !source.Equal(net.ParseIP(tc.source)) || !destination.Equal(net.ParseIP(tc.destination)) {
					t.Fatalf("native routing translated %s -> %s", source, destination)
				}
			}

			if err := tunnelMap.DeleteEndpoint(prefix); err != nil {
				t.Fatal(err)
			}

			if err := tunnelMap.RefreshNativeNexthops(); err != nil {
				t.Fatal(err)
			}

			key, err := cidrToKey(prefix)
			if err != nil {
				t.Fatal(err)
			}

			var endpoint tunnelEndpointC
			if err := tunnelMap.endpoints.Lookup(key, &endpoint); !errors.Is(err, ebpf.ErrKeyNotExist) {
				t.Fatalf("native refresh resurrected a withdrawn endpoint: %v", err)
			}
		}()
	}
}
