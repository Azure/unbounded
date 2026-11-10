// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostnetwork validates a declared Metalman host network topology
// and renders it for the local installer (runtime description), netplan,
// and systemd-networkd. See designs/metalman-host-network.md.
package hostnetwork

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	defaultMIIMonitorInterval = 100
	minMTU, maxMTU            = 576, 9216
)

var (
	// tokenPattern is the character set of every runtime description token.
	tokenPattern    = regexp.MustCompile(`^[a-zA-Z0-9_.:/,+-]+$`)
	linkNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`)
	vlanNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,14}$`)
	macPattern      = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	bondModes       = []string{v1alpha3.BondMode8023AD, v1alpha3.BondModeActiveBackup, v1alpha3.BondModeBalanceXOR}
	hashPolicies    = []string{"layer2", "layer2+3", "layer3+4", "encap2+3", "encap3+4"}
	lacpRates       = []string{"slow", "fast"}
	hashPolicyModes = []string{v1alpha3.BondMode8023AD, v1alpha3.BondModeBalanceXOR}
	errNilNetwork   = fmt.Errorf("network: host network is not set")
)

type ifaceKind int

const (
	kindEthernet ifaceKind = iota
	kindBond
	kindVLAN
)

// iface is the validated, kind-independent view of a declared interface.
type iface struct {
	kind     ifaceKind
	name     string
	path     string
	mac      string
	mtu      *int32
	addr     *v1alpha3.InterfaceAddressing
	prefixes []netip.Prefix
	bond     *v1alpha3.BondSpec
	vlan     *v1alpha3.VLANSpec
	master   string   // bond owning an ethernet
	vlans    []string // VLANs whose link is this interface
}

// topology indexes a host network in declaration order: ethernets, bonds,
// then VLANs.
type topology struct {
	spec   *v1alpha3.HostNetworkSpec
	ifaces []*iface
	byName map[string]*iface
}

func (t *topology) underlyingMACs(i *iface) []string {
	switch i.kind {
	case kindEthernet:
		return []string{i.mac}
	case kindBond:
		var macs []string
		for _, m := range i.bond.Interfaces {
			macs = append(macs, t.byName[m].mac)
		}

		return macs
	default:
		return t.underlyingMACs(t.byName[i.vlan.Link])
	}
}

// gatewayIface returns the interface that carries the default gateway, or
// nil.
func (t *topology) gatewayIface() *iface {
	for _, i := range t.ifaces {
		if i.addr.Gateway != "" {
			return i
		}
	}

	return nil
}

// dnsIface returns the interface that carries nameservers: the gateway
// interface, else the first addressed interface.
func (t *topology) dnsIface() *iface {
	if g := t.gatewayIface(); g != nil {
		return g
	}

	for _, i := range t.ifaces {
		if len(i.addr.Addresses) > 0 {
			return i
		}
	}

	return nil
}

// sourceIface returns the interface and prefix carrying ip.
func (t *topology) sourceIface(ip netip.Addr) (*iface, netip.Prefix) {
	for _, i := range t.ifaces {
		for _, p := range i.prefixes {
			if p.Addr() == ip {
				return i, p
			}
		}
	}

	return nil, netip.Prefix{}
}

// Validate checks the topology and its consistency with the provisioning
// lease. Errors name the offending field path, for example
// "network.bonds[0].interfaces[1]".
func Validate(n *v1alpha3.HostNetworkSpec, lease v1alpha3.DHCPLease) error {
	_, err := build(n, lease)

	return err
}

func build(n *v1alpha3.HostNetworkSpec, lease v1alpha3.DHCPLease) (*topology, error) {
	t, err := buildTopology(n)
	if err != nil {
		return nil, err
	}

	if err := t.checkLease(lease); err != nil {
		return nil, err
	}

	return t, nil
}

// buildTopology runs every check that does not involve the lease.
func buildTopology(n *v1alpha3.HostNetworkSpec) (*topology, error) {
	if n == nil {
		return nil, errNilNetwork
	}

	t := &topology{spec: n, byName: map[string]*iface{}}

	if err := t.index(); err != nil {
		return nil, err
	}

	if err := t.checkBonds(); err != nil {
		return nil, err
	}

	if err := t.checkVLANs(); err != nil {
		return nil, err
	}

	if err := t.checkAddressing(); err != nil {
		return nil, err
	}

	if err := t.checkDNS(); err != nil {
		return nil, err
	}

	return t, nil
}

func (t *topology) add(i *iface, namePattern *regexp.Regexp) error {
	if !namePattern.MatchString(i.name) {
		return fmt.Errorf("%s.name: invalid interface name %q", i.path, i.name)
	}

	if prev, ok := t.byName[i.name]; ok {
		return fmt.Errorf("%s.name: interface name %q already declared at %s", i.path, i.name, prev.path)
	}

	if i.mtu != nil && (*i.mtu < minMTU || *i.mtu > maxMTU) {
		return fmt.Errorf("%s.mtu: %d is outside %d-%d", i.path, *i.mtu, minMTU, maxMTU)
	}

	t.byName[i.name] = i
	t.ifaces = append(t.ifaces, i)

	return nil
}

func (t *topology) index() error {
	n := t.spec
	if len(n.Ethernets) == 0 {
		return fmt.Errorf("network.ethernets: at least one ethernet is required")
	}

	macs := map[string]string{}

	for k := range n.Ethernets {
		e := &n.Ethernets[k]
		i := &iface{kind: kindEthernet, name: e.Name, path: fmt.Sprintf("network.ethernets[%d]", k), mac: e.MACAddress, mtu: e.MTU, addr: &e.InterfaceAddressing}

		if err := t.add(i, linkNamePattern); err != nil {
			return err
		}

		hw, err := net.ParseMAC(e.MACAddress)
		if !macPattern.MatchString(e.MACAddress) || err != nil || hw[0]&1 != 0 || slices.Equal(hw, make(net.HardwareAddr, 6)) {
			return fmt.Errorf("%s.macAddress: %q is not a lowercase colon-separated unicast MAC", i.path, e.MACAddress)
		}

		if prev, ok := macs[e.MACAddress]; ok {
			return fmt.Errorf("%s.macAddress: %s already declared at %s", i.path, e.MACAddress, prev)
		}

		macs[e.MACAddress] = i.path + ".macAddress"
	}

	for k := range n.Bonds {
		b := &n.Bonds[k]
		if err := t.add(&iface{kind: kindBond, name: b.Name, path: fmt.Sprintf("network.bonds[%d]", k), mtu: b.MTU, addr: &b.InterfaceAddressing, bond: b}, linkNamePattern); err != nil {
			return err
		}
	}

	for k := range n.VLANs {
		v := &n.VLANs[k]
		if err := t.add(&iface{kind: kindVLAN, name: v.Name, path: fmt.Sprintf("network.vlans[%d]", k), mtu: v.MTU, addr: &v.InterfaceAddressing, vlan: v}, vlanNamePattern); err != nil {
			return err
		}
	}

	return nil
}

func bondMode(b *v1alpha3.BondSpec) string {
	if b.Mode == "" {
		return v1alpha3.BondMode8023AD
	}

	return b.Mode
}

func bondMIIMonitor(b *v1alpha3.BondSpec) int32 {
	if b.MIIMonitorInterval == nil {
		return defaultMIIMonitorInterval
	}

	return *b.MIIMonitorInterval
}

func (t *topology) checkBonds() error {
	for _, i := range t.ifaces {
		if i.kind != kindBond {
			continue
		}

		b := i.bond
		mode := bondMode(b)

		if !slices.Contains(bondModes, mode) {
			return fmt.Errorf("%s.mode: unsupported bond mode %q", i.path, b.Mode)
		}

		if m := bondMIIMonitor(b); m < 0 || m > 10000 {
			return fmt.Errorf("%s.miiMonitorInterval: %d is outside 0-10000", i.path, m)
		}

		if b.TransmitHashPolicy != "" {
			if !slices.Contains(hashPolicies, b.TransmitHashPolicy) {
				return fmt.Errorf("%s.transmitHashPolicy: unsupported policy %q", i.path, b.TransmitHashPolicy)
			}

			if !slices.Contains(hashPolicyModes, mode) {
				return fmt.Errorf("%s.transmitHashPolicy: only valid for modes 802.3ad and balance-xor, not %s", i.path, mode)
			}
		}

		if b.LACPRate != "" {
			if !slices.Contains(lacpRates, b.LACPRate) {
				return fmt.Errorf("%s.lacpRate: unsupported rate %q", i.path, b.LACPRate)
			}

			if mode != v1alpha3.BondMode8023AD {
				return fmt.Errorf("%s.lacpRate: requires mode 802.3ad, not %s", i.path, mode)
			}
		}

		if len(b.Interfaces) == 0 || len(b.Interfaces) > 8 {
			return fmt.Errorf("%s.interfaces: a bond needs 1-8 members, got %d", i.path, len(b.Interfaces))
		}

		for k, name := range b.Interfaces {
			path := fmt.Sprintf("%s.interfaces[%d]", i.path, k)

			m, ok := t.byName[name]
			if !ok || m.kind != kindEthernet {
				return fmt.Errorf("%s: %q is not a declared ethernet", path, name)
			}

			if m.master != "" {
				return fmt.Errorf("%s: ethernet %q already belongs to bond %q", path, name, m.master)
			}

			m.master = b.Name

			if a := m.addr; len(a.Addresses) > 0 || a.Gateway != "" || len(a.Routes) > 0 {
				return fmt.Errorf("%s: bond member %q must not have addresses, gateway, or routes", path, name)
			}
		}
	}

	return nil
}

func (t *topology) checkVLANs() error {
	type key struct {
		link string
		id   int32
	}

	seen := map[key]string{}

	for _, i := range t.ifaces {
		if i.kind != kindVLAN {
			continue
		}

		v := i.vlan
		if v.ID < 1 || v.ID > 4094 {
			return fmt.Errorf("%s.id: %d is outside 1-4094", i.path, v.ID)
		}

		l, ok := t.byName[v.Link]
		if !ok || l.kind == kindVLAN {
			return fmt.Errorf("%s.link: %q is not a declared ethernet or bond", i.path, v.Link)
		}

		if l.master != "" {
			return fmt.Errorf("%s.link: %q is a member of bond %q", i.path, v.Link, l.master)
		}

		k := key{v.Link, v.ID}
		if prev, ok := seen[k]; ok {
			return fmt.Errorf("%s: VLAN %d on %q already declared at %s", i.path, v.ID, v.Link, prev)
		}

		seen[k] = i.path

		if i.mtu != nil && l.mtu != nil && *i.mtu > *l.mtu {
			return fmt.Errorf("%s.mtu: %d exceeds link %q MTU %d", i.path, *i.mtu, v.Link, *l.mtu)
		}

		l.vlans = append(l.vlans, v.Name)
	}

	return nil
}

func parseIPv4(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)

	return a, err == nil && a.Is4() && a.String() == s
}

func (t *topology) checkAddressing() error {
	addrs := map[netip.Addr]string{}

	var gateway string

	for _, i := range t.ifaces {
		a := i.addr

		for k, s := range a.Addresses {
			path := fmt.Sprintf("%s.addresses[%d]", i.path, k)

			p, err := netip.ParsePrefix(s)
			if err != nil || !p.Addr().Is4() || p.String() != s {
				return fmt.Errorf("%s: %q is not a canonical IPv4 CIDR", path, s)
			}

			if p.Bits() < 1 || p.Bits() > 30 {
				return fmt.Errorf("%s: prefix length %d is outside 1-30", path, p.Bits())
			}

			ip := p.Addr()
			if !ip.IsGlobalUnicast() {
				return fmt.Errorf("%s: %s is not a global unicast address", path, ip)
			}

			if ip == p.Masked().Addr() || ip == broadcast(p) {
				return fmt.Errorf("%s: %s is the network or broadcast address of %s", path, ip, p.Masked())
			}

			if prev, ok := addrs[ip]; ok {
				return fmt.Errorf("%s: %s already declared at %s", path, ip, prev)
			}

			addrs[ip] = path
			i.prefixes = append(i.prefixes, p)
		}

		if a.Gateway != "" {
			path := i.path + ".gateway"
			if gateway != "" {
				return fmt.Errorf("%s: only one interface may set a gateway, already set at %s", path, gateway)
			}

			gateway = path

			if err := t.checkNextHop(i, path, a.Gateway); err != nil {
				return err
			}
		}

		dests := map[netip.Prefix]bool{}

		for k, r := range a.Routes {
			path := fmt.Sprintf("%s.routes[%d]", i.path, k)

			p, err := netip.ParsePrefix(r.To)
			if err != nil || !p.Addr().Is4() || p.String() != r.To || p.Masked() != p {
				return fmt.Errorf("%s.to: %q is not a canonical IPv4 network prefix", path, r.To)
			}

			if p.Bits() == 0 {
				return fmt.Errorf("%s.to: use gateway for the default route", path)
			}

			if dests[p] {
				return fmt.Errorf("%s.to: duplicate route to %s", path, p)
			}

			dests[p] = true

			if err := t.checkNextHop(i, path+".via", r.Via); err != nil {
				return err
			}
		}
	}

	// A next hop must not be one of the host's own addresses.
	for _, i := range t.ifaces {
		hops := []string{i.addr.Gateway}
		for _, r := range i.addr.Routes {
			hops = append(hops, r.Via)
		}

		for _, h := range hops {
			if ip, ok := parseIPv4(h); ok {
				if owner, mine := addrs[ip]; mine {
					return fmt.Errorf("%s: next hop %s is the host address declared at %s", i.path, h, owner)
				}
			}
		}
	}

	return nil
}

func (t *topology) checkNextHop(i *iface, path, s string) error {
	ip, ok := parseIPv4(s)
	if !ok || !ip.IsGlobalUnicast() {
		return fmt.Errorf("%s: %q is not a canonical global unicast IPv4 address", path, s)
	}

	for _, p := range i.prefixes {
		if p.Contains(ip) {
			if ip == p.Masked().Addr() || ip == broadcast(p) {
				return fmt.Errorf("%s: %s is the network or broadcast address of %s", path, s, p.Masked())
			}

			return nil
		}
	}

	return fmt.Errorf("%s: %s is not inside any subnet of %s", path, s, i.name)
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1

	for k := range 4 {
		b[k] |= byte(host >> (24 - 8*k))
	}

	return netip.AddrFrom4(b)
}

func (t *topology) checkDNS() error {
	if len(t.spec.DNS) > 3 {
		return fmt.Errorf("network.dns: at most 3 nameservers, got %d", len(t.spec.DNS))
	}

	seen := map[string]bool{}

	for k, s := range t.spec.DNS {
		ip, ok := parseIPv4(s)
		if !ok || ip.IsUnspecified() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return fmt.Errorf("network.dns[%d]: %q is not a canonical unicast IPv4 address", k, s)
		}

		if seen[s] {
			return fmt.Errorf("network.dns[%d]: duplicate nameserver %s", k, s)
		}

		seen[s] = true
	}

	return nil
}

func (t *topology) checkLease(l v1alpha3.DHCPLease) error {
	ip, ok := parseIPv4(l.IPv4)
	if !ok {
		return fmt.Errorf("dhcpLeases[0].ipv4: %q is not a canonical IPv4 address", l.IPv4)
	}

	mask, ok := parseIPv4(l.SubnetMask)

	ones, bits := net.IPMask(mask.AsSlice()).Size()
	if !ok || bits != 32 {
		return fmt.Errorf("dhcpLeases[0].subnetMask: %q is not a contiguous IPv4 netmask", l.SubnetMask)
	}

	src, p := t.sourceIface(ip)
	if src == nil {
		return fmt.Errorf("dhcpLeases[0].ipv4: %s is not declared on any interface", l.IPv4)
	}

	if p.Bits() != ones {
		return fmt.Errorf("dhcpLeases[0].subnetMask: %s (/%d) does not match %s declared on %s", l.SubnetMask, ones, p, src.name)
	}

	if !slices.Contains(t.underlyingMACs(src), l.MAC) {
		return fmt.Errorf("dhcpLeases[0].mac: %q is not an ethernet underlying %s", l.MAC, src.name)
	}

	gw := t.gatewayIface()
	if gw == nil || l.Gateway != gw.addr.Gateway {
		return fmt.Errorf("dhcpLeases[0].gateway: %q does not match the declared network gateway", l.Gateway)
	}

	if gw != src {
		return fmt.Errorf("%s.gateway: the gateway must be on %s, which carries dhcpLeases[0].ipv4", gw.path, src.name)
	}

	// Defense in depth: every runtime token must be safe for the installer's
	// shell parser even though the checks above already constrain them.
	for _, line := range runtimeLines(t, ip) {
		for _, tok := range line {
			if !tokenPattern.MatchString(tok) {
				return fmt.Errorf("network: unsafe runtime token %q", tok)
			}
		}
	}

	return nil
}
