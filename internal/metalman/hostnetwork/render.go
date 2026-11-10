// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

// File is a rendered configuration file, named relative to its target
// directory.
type File struct {
	Name string
	Data []byte
}

// Media archive paths, relative to the initramfs root.
const (
	MediaRuntimePath  = "unbounded/media/network"
	MediaNetplanPath  = "unbounded/media/netplan.yaml"
	MediaNetworkdPath = "unbounded/media/networkd"
)

// quote renders s as a YAML double-quoted scalar. Validated values are plain
// ASCII, for which Go and YAML double-quoted escaping agree.
func quote(s string) string {
	return strconv.Quote(s)
}

type yamlWriter struct {
	b      bytes.Buffer
	indent int
}

func (w *yamlWriter) line(depth int, format string, args ...any) {
	w.b.WriteString(strings.Repeat("  ", w.indent+depth))
	fmt.Fprintf(&w.b, format, args...)
	w.b.WriteByte('\n')
}

// unaddressed reports whether an interface carries no IPv4 configuration;
// bond members are always unaddressed.
func unaddressed(i *iface) bool {
	return len(i.addr.Addresses) == 0
}

func (w *yamlWriter) common(t *topology, i *iface) {
	// netplan defaults dhcp4/dhcp6 to false; they are explicit so a reader
	// does not need to know that.
	w.line(2, "dhcp4: false")
	w.line(2, "dhcp6: false")

	if unaddressed(i) {
		w.line(2, "link-local: []")
	}

	if i.mtu != nil {
		w.line(2, "mtu: %d", *i.mtu)
	}

	if len(i.addr.Addresses) > 0 {
		w.line(2, "addresses:")

		for _, a := range i.addr.Addresses {
			w.line(3, "- %s", quote(a))
		}
	}

	if i.addr.Gateway != "" || len(i.addr.Routes) > 0 {
		w.line(2, "routes:")

		if i.addr.Gateway != "" {
			w.line(3, "- to: %s", quote("default"))
			w.line(4, "via: %s", quote(i.addr.Gateway))
		}

		for _, r := range i.addr.Routes {
			w.line(3, "- to: %s", quote(r.To))
			w.line(4, "via: %s", quote(r.Via))
		}
	}

	if len(t.spec.DNS) > 0 && t.dnsIface() == i {
		w.line(2, "nameservers:")
		w.line(3, "addresses:")

		for _, d := range t.spec.DNS {
			w.line(4, "- %s", quote(d))
		}
	}
}

func renderNetplan(n *v1alpha3.HostNetworkSpec, wrap bool) ([]byte, error) {
	t, err := buildTopology(n)
	if err != nil {
		return nil, err
	}

	w := &yamlWriter{}
	if wrap {
		w.line(0, "network:")
		w.indent = 1
	}

	w.line(0, "version: 2")

	for _, kind := range []struct {
		k   ifaceKind
		key string
	}{{kindEthernet, "ethernets"}, {kindBond, "bonds"}, {kindVLAN, "vlans"}} {
		header := false

		for _, i := range t.ifaces {
			if i.kind != kind.k {
				continue
			}

			if !header {
				w.line(0, "%s:", kind.key)

				header = true
			}

			w.line(1, "%s:", quote(i.name))

			switch i.kind {
			case kindEthernet:
				// netplan 0.106 and later (Ubuntu 22.04 updates, 24.04 ships
				// 1.0) match macaddress against the permanent MAC
				// (PermanentMACAddress= in networkd), so bond members whose
				// current MAC was rewritten by the bond still match. See
				// https://discourse.ubuntu.com/t/netplan-0-106-call-for-testing/33932
				// and https://netplan.readthedocs.io/en/stable/netplan-yaml/
				// ("6-byte permanent MAC address of the device").
				w.line(2, "match:")
				w.line(3, "macaddress: %s", quote(i.mac))
				w.line(2, "set-name: %s", quote(i.name))
			case kindBond:
				b := i.bond

				w.line(2, "interfaces:")

				for _, m := range b.Interfaces {
					w.line(3, "- %s", quote(m))
				}

				w.line(2, "parameters:")
				w.line(3, "mode: %s", quote(bondMode(b)))
				// An unsuffixed integer is milliseconds in netplan, and the
				// NetworkManager backend only accepts integers.
				w.line(3, "mii-monitor-interval: %d", bondMIIMonitor(b))

				if b.TransmitHashPolicy != "" {
					w.line(3, "transmit-hash-policy: %s", quote(b.TransmitHashPolicy))
				}

				if b.LACPRate != "" {
					w.line(3, "lacp-rate: %s", quote(b.LACPRate))
				}
			case kindVLAN:
				w.line(2, "id: %d", i.vlan.ID)
				w.line(2, "link: %s", quote(i.vlan.Link))
			}

			w.common(t, i)
		}
	}

	return w.b.Bytes(), nil
}

// RenderNetplan renders a netplan v2 document with a top-level "network:"
// key, for /etc/netplan/99-unbounded-metal.yaml. The renderer key is omitted
// so the image default (networkd) applies.
func RenderNetplan(n *v1alpha3.HostNetworkSpec) ([]byte, error) {
	return renderNetplan(n, true)
}

// RenderNetplanConfig renders the same document without the "network:"
// wrapper, matching the shape of the NoCloud network-config template
// (images/netboot/assets/network-config.tmpl), which starts at "version: 2".
func RenderNetplanConfig(n *v1alpha3.HostNetworkSpec) ([]byte, error) {
	return renderNetplan(n, false)
}

type unit struct {
	b bytes.Buffer
}

func (u *unit) section(name string) {
	if u.b.Len() > 0 {
		u.b.WriteByte('\n')
	}

	fmt.Fprintf(&u.b, "[%s]\n", name)
}

func (u *unit) kv(k, v string) {
	fmt.Fprintf(&u.b, "%s=%s\n", k, v)
}

// RenderNetworkd renders systemd-networkd configuration for the topology:
// 10-unbounded-<name>.link for ethernets, 20-unbounded-<name>.netdev for
// bonds and VLANs, and 30-unbounded-<name>.network for every interface. The
// result is sorted by name.
//
// Bonds get no MACAddress=: systemd-networkd then assigns the bond a stable
// MAC derived from the machine ID and interface name (systemd.netdev(5),
// MACAddress=), so the bond MAC does not depend on member enslavement order.
// Static addressing makes the bond MAC irrelevant to the lease, which keys
// on a member's permanent MAC.
func RenderNetworkd(n *v1alpha3.HostNetworkSpec) ([]File, error) {
	t, err := buildTopology(n)
	if err != nil {
		return nil, err
	}

	var files []File

	for _, i := range t.ifaces {
		switch i.kind {
		case kindEthernet:
			var l unit
			l.section("Match")
			l.kv("PermanentMACAddress", i.mac)
			l.section("Link")
			l.kv("Name", i.name)
			files = append(files, File{Name: "10-unbounded-" + i.name + ".link", Data: l.b.Bytes()})
		case kindBond:
			b := i.bond

			var d unit
			d.section("NetDev")
			d.kv("Name", i.name)
			d.kv("Kind", "bond")

			if i.mtu != nil {
				d.kv("MTUBytes", optInt(i.mtu))
			}

			d.section("Bond")
			d.kv("Mode", bondMode(b))
			d.kv("MIIMonitorSec", fmt.Sprintf("%dms", bondMIIMonitor(b)))

			if b.TransmitHashPolicy != "" {
				d.kv("TransmitHashPolicy", b.TransmitHashPolicy)
			}

			if b.LACPRate != "" {
				d.kv("LACPTransmitRate", b.LACPRate)
			}

			files = append(files, File{Name: "20-unbounded-" + i.name + ".netdev", Data: d.b.Bytes()})
		case kindVLAN:
			var d unit
			d.section("NetDev")
			d.kv("Name", i.name)
			d.kv("Kind", "vlan")

			if i.mtu != nil {
				d.kv("MTUBytes", optInt(i.mtu))
			}

			d.section("VLAN")
			d.kv("Id", strconv.Itoa(int(i.vlan.ID)))
			files = append(files, File{Name: "20-unbounded-" + i.name + ".netdev", Data: d.b.Bytes()})
		}

		files = append(files, File{Name: "30-unbounded-" + i.name + ".network", Data: networkFile(t, i)})
	}

	sort.Slice(files, func(a, b int) bool { return files[a].Name < files[b].Name })

	return files, nil
}

func networkFile(t *topology, i *iface) []byte {
	var u unit

	u.section("Match")

	if i.kind == kindEthernet {
		// Bond members share the bond's current MAC, so only the permanent
		// MAC identifies an ethernet unambiguously.
		u.kv("PermanentMACAddress", i.mac)
	} else {
		u.kv("Name", i.name)
	}

	if i.kind == kindEthernet && i.mtu != nil {
		u.section("Link")
		u.kv("MTUBytes", optInt(i.mtu))
	}

	u.section("Network")

	if i.master != "" {
		u.kv("Bond", i.master)
	}

	if unaddressed(i) {
		u.kv("LinkLocalAddressing", "no")
	}

	for _, v := range i.vlans {
		u.kv("VLAN", v)
	}

	for _, a := range i.addr.Addresses {
		u.kv("Address", a)
	}

	if i.addr.Gateway != "" {
		u.kv("Gateway", i.addr.Gateway)
	}

	if t.dnsIface() == i {
		for _, d := range t.spec.DNS {
			u.kv("DNS", d)
		}
	}

	for _, r := range i.addr.Routes {
		u.section("Route")
		u.kv("Destination", r.To)
		u.kv("Gateway", r.Via)
	}

	return u.b.Bytes()
}

// MediaCPIO assembles the per-operation media.cpio archive: the runtime
// description, the netplan document, and the networkd files under
// unbounded/media/. It validates n against lease first.
func MediaCPIO(n *v1alpha3.HostNetworkSpec, lease v1alpha3.DHCPLease) ([]byte, error) {
	runtime, err := RenderRuntime(n, lease)
	if err != nil {
		return nil, err
	}

	netplan, err := RenderNetplan(n)
	if err != nil {
		return nil, err
	}

	networkd, err := RenderNetworkd(n)
	if err != nil {
		return nil, err
	}

	files := []CPIOFile{
		{Path: MediaRuntimePath, Mode: 0o644, Data: runtime},
		{Path: MediaNetplanPath, Mode: 0o644, Data: netplan},
		{Path: MediaNetworkdPath, Mode: fs.ModeDir | 0o755},
	}
	for _, f := range networkd {
		files = append(files, CPIOFile{Path: MediaNetworkdPath + "/" + f.Name, Mode: 0o644, Data: f.Data})
	}

	var b bytes.Buffer
	if err := WriteCPIO(&b, files); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}
