// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

func i32(v int32) *int32 { return &v }

// reference returns the reference topology: two ethernets in an LACP bond
// carrying the provisioning address and a tagged VLAN with static routes.
func reference() (*v1alpha3.HostNetworkSpec, v1alpha3.DHCPLease) {
	n := &v1alpha3.HostNetworkSpec{
		Ethernets: []v1alpha3.EthernetSpec{
			{Name: "enp49s0f0np0", MACAddress: "02:00:00:00:00:01"},
			{Name: "enp49s0f1np1", MACAddress: "02:00:00:00:00:02"},
		},
		Bonds: []v1alpha3.BondSpec{{
			Name:               "bond0",
			Interfaces:         []string{"enp49s0f0np0", "enp49s0f1np1"},
			Mode:               "802.3ad",
			MIIMonitorInterval: i32(100),
			TransmitHashPolicy: "layer3+4",
			InterfaceAddressing: v1alpha3.InterfaceAddressing{
				MTU:       i32(1500),
				Addresses: []string{"192.0.2.30/24"},
				Gateway:   "192.0.2.1",
			},
		}},
		VLANs: []v1alpha3.VLANSpec{{
			Name: "bond0.64",
			ID:   64,
			Link: "bond0",
			InterfaceAddressing: v1alpha3.InterfaceAddressing{
				Addresses: []string{"198.51.100.30/24"},
				Routes: []v1alpha3.RouteSpec{
					{To: "203.0.113.0/24", Via: "198.51.100.1"},
					{To: "100.64.0.0/16", Via: "198.51.100.1"},
				},
			},
		}},
		DNS: []string{"192.0.2.53", "192.0.2.54"},
	}
	lease := v1alpha3.DHCPLease{IPv4: "192.0.2.30", MAC: "02:00:00:00:00:01", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}

	return n, lease
}

// single returns a minimal single-NIC topology.
func single() (*v1alpha3.HostNetworkSpec, v1alpha3.DHCPLease) {
	n := &v1alpha3.HostNetworkSpec{
		Ethernets: []v1alpha3.EthernetSpec{{
			Name:       "eth0",
			MACAddress: "02:00:00:00:00:0a",
			InterfaceAddressing: v1alpha3.InterfaceAddressing{
				Addresses: []string{"192.0.2.10/24"},
				Gateway:   "192.0.2.1",
			},
		}},
	}

	return n, v1alpha3.DHCPLease{IPv4: "192.0.2.10", MAC: "02:00:00:00:00:0a", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}
}

func TestValidateAccepts(t *testing.T) {
	cases := map[string]func(*v1alpha3.HostNetworkSpec, *v1alpha3.DHCPLease){
		"reference": func(*v1alpha3.HostNetworkSpec, *v1alpha3.DHCPLease) {},
		"lease on second member": func(n *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			l.MAC = "02:00:00:00:00:02"
		},
		"defaults": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Mode = ""
			n.Bonds[0].MIIMonitorInterval = nil
		},
		"miimon zero": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].MIIMonitorInterval = i32(0) },
		"lacp fast":   func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].LACPRate = "fast" },
		"no dns":      func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.DNS = nil },
		"vlan mtu ok": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.VLANs[0].MTU = i32(1500) },
		"vlan mtu without link mtu": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].MTU = nil
			n.VLANs[0].MTU = i32(9000)
		},
		"balance-xor hash": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].Mode = "balance-xor" },
		"active-backup": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Mode = "active-backup"
			n.Bonds[0].TransmitHashPolicy = ""
		},
		"lease on vlan": func(n *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = ""
			n.VLANs[0].Gateway = "198.51.100.1"
			*l = v1alpha3.DHCPLease{IPv4: "198.51.100.30", MAC: "02:00:00:00:00:02", SubnetMask: "255.255.255.0", Gateway: "198.51.100.1"}
		},
		"vlan on ethernet": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets = append(n.Ethernets, v1alpha3.EthernetSpec{Name: "eth2", MACAddress: "02:00:00:00:00:03"})
			n.VLANs = append(n.VLANs, v1alpha3.VLANSpec{Name: "eth2.64", ID: 64, Link: "eth2"})
		},
		"multiple addresses": func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Addresses = append(n.Bonds[0].Addresses, "203.0.113.5/30")
		},
		"slash30": func(n *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			n.Bonds[0].Addresses = []string{"192.0.2.2/30"}
			l.IPv4, l.SubnetMask = "192.0.2.2", "255.255.255.252"
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			n, l := reference()
			mutate(n, &l)

			if err := Validate(n, l); err != nil {
				t.Fatal(err)
			}
		})
	}

	n, l := single()
	if err := Validate(n, l); err != nil {
		t.Fatalf("single: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*v1alpha3.HostNetworkSpec, *v1alpha3.DHCPLease)
		want   string
	}{
		{"no ethernets", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Ethernets = nil }, "network.ethernets"},
		{"bad ethernet name", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].Name = "0bad"
		}, "network.ethernets[0].name"},
		{"long name", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[1].Name = "abcdefghijklmnop"
		}, "network.ethernets[1].name"},
		{"shell meta in name", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[1].Name = "a;b"
		}, "network.ethernets[1].name"},
		{"dot in bond name", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Name = "bond.0"
		}, "network.bonds[0].name"},
		{"bad vlan name", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Name = "bond0 64"
		}, "network.vlans[0].name"},
		{"duplicate name across kinds", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Name = "bond0"
		}, "network.vlans[0].name"},
		{"uppercase mac", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].MACAddress = "02:00:00:00:00:0A"
		}, "network.ethernets[0].macAddress"},
		{"dash mac", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].MACAddress = "02-00-00-00-00-01"
		}, "network.ethernets[0].macAddress"},
		{"multicast mac", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].MACAddress = "03:00:00:00:00:01"
		}, "network.ethernets[0].macAddress"},
		{"zero mac", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].MACAddress = "00:00:00:00:00:00"
		}, "network.ethernets[0].macAddress"},
		{"duplicate mac", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[1].MACAddress = "02:00:00:00:00:01"
		}, "network.ethernets[1].macAddress"},
		{"mtu low", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].MTU = i32(575) }, "network.bonds[0].mtu"},
		{"mtu high", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.VLANs[0].MTU = i32(9217) }, "network.vlans[0].mtu"},
		{"bad mode", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].Mode = "balance-rr" }, "network.bonds[0].mode"},
		{"miimon negative", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].MIIMonitorInterval = i32(-1)
		}, "network.bonds[0].miiMonitorInterval"},
		{"miimon high", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].MIIMonitorInterval = i32(10001)
		}, "network.bonds[0].miiMonitorInterval"},
		{"bad hash", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].TransmitHashPolicy = "layer5"
		}, "network.bonds[0].transmitHashPolicy"},
		{"hash with active-backup", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Mode = "active-backup"
		}, "network.bonds[0].transmitHashPolicy"},
		{"bad lacp", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].LACPRate = "medium" }, "network.bonds[0].lacpRate"},
		{"lacp with balance-xor", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Mode = "balance-xor"
			n.Bonds[0].LACPRate = "fast"
		}, "network.bonds[0].lacpRate"},
		{"no members", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.Bonds[0].Interfaces = nil }, "network.bonds[0].interfaces"},
		{"too many members", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Interfaces = make([]string, 9)
		}, "network.bonds[0].interfaces"},
		{"unknown member", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Interfaces[1] = "eth9"
		}, "network.bonds[0].interfaces[1]"},
		{"member is bond", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Interfaces[1] = "bond0"
		}, "network.bonds[0].interfaces[1]"},
		{"member is vlan", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Interfaces[1] = "bond0.64"
		}, "network.bonds[0].interfaces[1]"},
		{"repeated member", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Interfaces[1] = "enp49s0f0np0"
		}, "network.bonds[0].interfaces[1]"},
		{"member in two bonds", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds = append(n.Bonds, v1alpha3.BondSpec{Name: "bond1", Interfaces: []string{"enp49s0f1np1"}})
		}, "network.bonds[1].interfaces[0]"},
		{"member with address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[1].Addresses = []string{"203.0.113.9/24"}
		}, "network.bonds[0].interfaces[1]"},
		{"member with gateway", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].Gateway = "192.0.2.1"
		}, "network.bonds[0].interfaces[0]"},
		{"member with routes", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets[0].Routes = []v1alpha3.RouteSpec{{To: "203.0.113.0/24", Via: "192.0.2.1"}}
		}, "network.bonds[0].interfaces[0]"},
		{"vlan id zero", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.VLANs[0].ID = 0 }, "network.vlans[0].id"},
		{"vlan id 4095", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.VLANs[0].ID = 4095 }, "network.vlans[0].id"},
		{"vlan unknown link", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.VLANs[0].Link = "bond9" }, "network.vlans[0].link"},
		{"vlan on vlan", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs = append(n.VLANs, v1alpha3.VLANSpec{Name: "q", ID: 5, Link: "bond0.64"})
		}, "network.vlans[1].link"},
		{"vlan on member", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Link = "enp49s0f0np0"
		}, "network.vlans[0].link"},
		{"duplicate vlan id", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs = append(n.VLANs, v1alpha3.VLANSpec{Name: "bond0.x", ID: 64, Link: "bond0"})
		}, "network.vlans[1]"},
		{"vlan mtu exceeds link", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].MTU = i32(9000)
		}, "network.vlans[0].mtu"},
		{"non-canonical cidr", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "198.51.100.030/24"
		}, "network.vlans[0].addresses[0]"},
		{"bare ip", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "198.51.100.30"
		}, "network.vlans[0].addresses[0]"},
		{"ipv6 address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "2001:db8::1/64"
		}, "network.vlans[0].addresses[0]"},
		{"prefix 31", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "198.51.100.30/31"
		}, "network.vlans[0].addresses[0]"},
		{"prefix 0", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "198.51.100.30/0"
		}, "network.vlans[0].addresses[0]"},
		{"loopback address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "127.0.0.2/8"
		}, "network.vlans[0].addresses[0]"},
		{"link-local address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "169.254.1.1/16"
		}, "network.vlans[0].addresses[0]"},
		{"network address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "198.51.100.0/24"
		}, "network.vlans[0].addresses[0]"},
		{"broadcast address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "198.51.100.255/24"
		}, "network.vlans[0].addresses[0]"},
		{"duplicate address", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Addresses[0] = "192.0.2.30/25"
		}, "network.vlans[0].addresses[0]"},
		{"two gateways", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Gateway = "198.51.100.1"
		}, "network.vlans[0].gateway"},
		{"gateway outside subnet", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = "198.51.100.1"
		}, "network.bonds[0].gateway"},
		{"gateway without addresses", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Ethernets = append(n.Ethernets, v1alpha3.EthernetSpec{Name: "eth2", MACAddress: "02:00:00:00:00:03"})
			n.Ethernets[2].Gateway = "192.0.2.1"
			n.Bonds[0].Gateway = ""
		}, "network.ethernets[2].gateway"},
		{"gateway non-canonical", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = "192.0.2.01"
		}, "network.bonds[0].gateway"},
		{"gateway is broadcast", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = "192.0.2.255"
		}, "network.bonds[0].gateway"},
		{"gateway is self", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = "192.0.2.30"
		}, "network.bonds[0]"},
		{"route default", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Routes[0].To = "0.0.0.0/0"
		}, "network.vlans[0].routes[0].to"},
		{"route host bits", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Routes[1].To = "100.64.0.1/16"
		}, "network.vlans[0].routes[1].to"},
		{"route bare ip", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Routes[1].To = "default"
		}, "network.vlans[0].routes[1].to"},
		{"route duplicate", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Routes[1].To = "203.0.113.0/24"
		}, "network.vlans[0].routes[1].to"},
		{"route via outside subnet", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Routes[0].Via = "192.0.2.1"
		}, "network.vlans[0].routes[0].via"},
		{"route via bad", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.VLANs[0].Routes[0].Via = "x"
		}, "network.vlans[0].routes[0].via"},
		{"too many dns", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) {
			n.DNS = []string{"192.0.2.53", "192.0.2.54", "192.0.2.55", "192.0.2.56"}
		}, "network.dns"},
		{"bad dns", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.DNS[1] = "dns.example" }, "network.dns[1]"},
		{"dns unspecified", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.DNS[1] = "0.0.0.0" }, "network.dns[1]"},
		{"dns duplicate", func(n *v1alpha3.HostNetworkSpec, _ *v1alpha3.DHCPLease) { n.DNS[1] = n.DNS[0] }, "network.dns[1]"},
		{"lease ip invalid", func(_ *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) { l.IPv4 = "" }, "dhcpLeases[0].ipv4"},
		{"lease ip undeclared", func(_ *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			l.IPv4 = "192.0.2.31"
		}, "dhcpLeases[0].ipv4"},
		{"lease mask invalid", func(_ *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			l.SubnetMask = "255.0.255.0"
		}, "dhcpLeases[0].subnetMask"},
		{"lease mask mismatch", func(_ *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			l.SubnetMask = "255.255.0.0"
		}, "dhcpLeases[0].subnetMask"},
		{"lease mac not underlying", func(n *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			n.Ethernets = append(n.Ethernets, v1alpha3.EthernetSpec{Name: "eth2", MACAddress: "02:00:00:00:00:03"})
			l.MAC = "02:00:00:00:00:03"
		}, "dhcpLeases[0].mac"},
		{"lease gateway mismatch", func(_ *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			l.Gateway = "192.0.2.2"
		}, "dhcpLeases[0].gateway"},
		{"lease without gateway", func(n *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = ""
			l.Gateway = ""
		}, "dhcpLeases[0].gateway"},
		{"gateway on other interface", func(n *v1alpha3.HostNetworkSpec, l *v1alpha3.DHCPLease) {
			n.Bonds[0].Gateway = ""
			n.VLANs[0].Gateway = "198.51.100.1"
			l.Gateway = "198.51.100.1"
		}, "network.vlans[0].gateway"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, l := reference()
			tc.mutate(n, &l)

			err := Validate(n, l)
			if err == nil {
				t.Fatal("expected error")
			}

			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error %q does not start with %q", err, tc.want)
			}
		})
	}
}

func TestValidateNil(t *testing.T) {
	_, l := reference()
	if err := Validate(nil, l); err == nil {
		t.Fatal("expected error")
	}
}

func TestBroadcast(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.30/24":  "192.0.2.255",
		"192.0.2.2/30":   "192.0.2.3",
		"10.1.2.3/8":     "10.255.255.255",
		"198.51.100.9/1": "255.255.255.255",
	} {
		p := netip.MustParsePrefix(in)
		if got := broadcast(p).String(); got != want {
			t.Errorf("broadcast(%s) = %s, want %s", in, got, want)
		}
	}
}
