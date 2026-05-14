// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// unroute dumps the eBPF tunnel-endpoint LPM trie (unb_endpts) in
// human-readable, JSON, or raw-hex form. It can also perform a
// longest-prefix-match lookup for a specific IP address.
//
// Both IPv4 and IPv6 destinations share a single map. IPv4 entries are
// stored in IPv4-mapped IPv6 form (::ffff:<v4>); the --raw, --json, and
// human-readable dumps unmap them on display.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/cilium/ebpf"
	flag "github.com/spf13/pflag"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
	"github.com/Azure/unbounded/internal/version"
)

// entry is the human-readable representation of one nexthop in one trie
// entry. We emit one entry per nexthop so multi-nexthop CIDRs produce
// multiple rows.
type entry struct {
	CIDR      string `json:"cidr"`
	Remote    string `json:"remote"`
	Node      string `json:"node,omitempty"`
	Interface string `json:"interface"`
	Protocol  string `json:"protocol"`
	Healthy   bool   `json:"healthy"`
	VNI       uint32 `json:"vni"`
	MTU       int    `json:"mtu"`
	IfIndex   uint32 `json:"ifindex"`
	Family    string `json:"family"` // "v4" or "v6", for client filtering
}

// rawEntry is the structured representation of one trie entry in raw-hex
// dump mode (no annotation, no interface resolution).
type rawEntry struct {
	KeyHex   string `json:"key"`
	ValueHex string `json:"value"`
	Family   string `json:"family"`
}

func main() {
	var (
		showVersion bool
		jsonOutput  bool
		rawOutput   bool
		statusPort  int
		v4Only      bool
		v6Only      bool
	)

	flag.BoolVarP(&jsonOutput, "json", "j", false, "Output entries as JSON")
	flag.BoolVarP(&rawOutput, "raw", "r", false, "Dump map entries as raw key/value hex")
	flag.BoolVarP(&v4Only, "v4-only", "4", false, "Show only IPv4 entries")
	flag.BoolVarP(&v6Only, "v6-only", "6", false, "Show only IPv6 entries")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.IntVar(&statusPort, "status-port", 9998, "Port of the local node status endpoint")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: unroute [options] [IP_ADDRESS]\n\n")
		fmt.Fprintf(os.Stderr, "Dump the eBPF tunnel-endpoint LPM trie (unb_endpts).\n")
		fmt.Fprintf(os.Stderr, "If an IP address is given, perform a longest-prefix-match lookup.\n\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if showVersion {
		fmt.Printf("unroute %s (commit %s, built %s)\n", version.Version, version.GitCommit, version.BuildTime)
		os.Exit(0)
	}

	if v4Only && v6Only {
		fmt.Fprintln(os.Stderr, "unroute: -4 and -6 are mutually exclusive")
		os.Exit(2)
	}

	familyFilter := familyAll
	switch {
	case v4Only:
		familyFilter = familyV4
	case v6Only:
		familyFilter = familyV6
	}

	args := flag.Args()
	if len(args) > 0 {
		if err := lookupEntry(args[0], jsonOutput, statusPort, familyFilter); err != nil {
			fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
			os.Exit(1)
		}

		return
	}

	if rawOutput {
		if err := dumpRaw(jsonOutput, familyFilter); err != nil {
			fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
			os.Exit(1)
		}

		return
	}

	if err := dumpTunnelEndpoints(jsonOutput, statusPort, familyFilter); err != nil {
		fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
		os.Exit(1)
	}
}

const (
	familyAll = iota
	familyV4
	familyV6
)

// protocolName returns the tunnel protocol name for the given protocol constant.
func protocolName(proto uint32) string {
	switch proto {
	case ebpfpkg.TunnelProtoGENEVE:
		return "GENEVE"
	case ebpfpkg.TunnelProtoVXLAN:
		return "VXLAN"
	case ebpfpkg.TunnelProtoIPIP:
		return "IPIP"
	case ebpfpkg.TunnelProtoWireGuard:
		return "WireGuard"
	case ebpfpkg.TunnelProtoNone:
		return "None"
	default:
		return fmt.Sprintf("unknown(%d)", proto)
	}
}

// resolveInterface returns the interface name and MTU for the given ifindex.
// If the interface cannot be resolved, it returns a placeholder name and zero MTU.
func resolveInterface(ifindex uint32) (string, int) {
	iface, err := net.InterfaceByIndex(int(ifindex))
	if err != nil {
		return fmt.Sprintf("if%d", ifindex), 0
	}

	return iface.Name, iface.MTU
}

// findMap returns the single unb_endpts map. Returns an explanatory error
// if the map is not present (i.e. the BPF program is not loaded).
func findMap() (*ebpf.Map, error) {
	id := ebpf.MapID(0)

	for {
		var err error

		id, err = ebpf.MapGetNextID(id)
		if err != nil {
			break
		}

		m, err := ebpf.NewMapFromID(id)
		if err != nil {
			continue
		}

		info, err := m.Info()
		if err != nil {
			_ = m.Close() //nolint:errcheck
			continue
		}

		if info.Name == ebpfpkg.MapName {
			return m, nil
		}

		_ = m.Close() //nolint:errcheck
	}

	return nil, fmt.Errorf("BPF map %q not found (is the unbounded_encap program loaded?)", ebpfpkg.MapName)
}

// dumpTunnelEndpoints walks the trie and prints one row per nexthop,
// classifying each entry by whether the key is IPv4-mapped or native v6.
func dumpTunnelEndpoints(jsonOutput bool, statusPort, familyFilter int) error {
	m, err := findMap()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }() //nolint:errcheck

	entries, err := collectEntries(m, familyFilter)
	if err != nil {
		return err
	}

	annotateEntries(entries, statusPort)
	sort.Slice(entries, func(i, j int) bool { return entries[i].CIDR < entries[j].CIDR })

	return printEntries(entries, jsonOutput)
}

// dumpRaw prints each map entry as <key hex> -> <value hex>, with no
// annotation, no interface resolution, and no status-server roundtrip. The
// caller can use this for byte-exact comparisons against what the agent
// believes it pushed.
func dumpRaw(jsonOutput bool, familyFilter int) error {
	m, err := findMap()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }() //nolint:errcheck

	var (
		key  ebpfpkg.LpmKey
		val  ebpfpkg.RawTunnelEndpoint
		rows []rawEntry
	)

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		family := familyOfKey(key)
		if !familyMatches(familyFilter, family) {
			continue
		}

		keyBytes := encodeKey(key)
		valBytes := encodeValue(val)
		rows = append(rows, rawEntry{
			KeyHex:   hex.EncodeToString(keyBytes),
			ValueHex: hex.EncodeToString(valBytes),
			Family:   family,
		})
	}

	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate %s: %w", ebpfpkg.MapName, err)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].KeyHex < rows[j].KeyHex })

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Println("(no entries)")
		return nil
	}

	for _, r := range rows {
		fmt.Printf("%s %s -> %s\n", r.Family, r.KeyHex, r.ValueHex)
	}

	fmt.Printf("\n%d entries\n", len(rows))

	return nil
}

// lookupEntry performs an LPM lookup for the supplied IP address against
// the unified trie. The IP is mapped to v6 form before lookup; the family
// filter, if set, must match the IP's natural family.
func lookupEntry(ipStr string, jsonOutput bool, statusPort, familyFilter int) error {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("invalid IP address: %s", ipStr)
	}

	ipFamily := familyV6
	if ip.To4() != nil {
		ipFamily = familyV4
	}

	if !familyMatches(familyFilter, familyName(ipFamily)) {
		return fmt.Errorf("address %s is %s but only %s entries requested", ipStr, familyName(ipFamily), familyFilterName(familyFilter))
	}

	m, err := findMap()
	if err != nil {
		return err
	}

	defer func() { _ = m.Close() }() //nolint:errcheck

	var key ebpfpkg.LpmKey

	key.Prefixlen = 128
	copy(key.Addr[:], ip.To16())

	var val ebpfpkg.RawTunnelEndpoint
	if err := m.Lookup(&key, &val); err != nil {
		return fmt.Errorf("lookup %s: %w", ipStr, err)
	}

	// We can't recover the prefix length the trie matched against; fall
	// back to the lookup key. For display, take the prefix from the
	// matched entry by iterating once more and finding the longest match.
	// In practice users care about the value here, not the prefix.
	entries := makeEntries(key, val, familyFilter)
	annotateEntries(entries, statusPort)

	return printEntries(entries, jsonOutput)
}

// collectEntries iterates the map and builds display entries, filtering
// by family.
func collectEntries(m *ebpf.Map, familyFilter int) ([]entry, error) {
	var (
		key ebpfpkg.LpmKey
		val ebpfpkg.RawTunnelEndpoint
	)

	var entries []entry

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		entries = append(entries, makeEntries(key, val, familyFilter)...)
	}

	if err := iter.Err(); err != nil {
		return entries, fmt.Errorf("iterate %s: %w", ebpfpkg.MapName, err)
	}

	return entries, nil
}

// makeEntries builds entries from a single trie key/value pair, one per
// nexthop, optionally filtered by family.
func makeEntries(key ebpfpkg.LpmKey, val ebpfpkg.RawTunnelEndpoint, familyFilter int) []entry {
	family := familyOfKey(key)
	if !familyMatches(familyFilter, family) {
		return nil
	}

	cidrStr := formatKey(key)

	entries := make([]entry, 0, val.Count)
	for i := uint32(0); i < val.Count && i < uint32(ebpfpkg.MaxNexthops); i++ {
		nh := val.Nexthops[i]
		ifName, mtu := resolveInterface(nh.Ifindex)

		entries = append(entries, entry{
			CIDR:      cidrStr,
			Remote:    formatEndpoint(nh.RemoteEndpoint),
			Interface: ifName,
			Protocol:  protocolName(nh.Protocol),
			Healthy:   nh.Healthy != 0,
			VNI:       nh.Vni,
			MTU:       mtu,
			IfIndex:   nh.Ifindex,
			Family:    family,
		})
	}

	return entries
}

// printEntries renders a slice of entries in text or JSON format.
func printEntries(entries []entry, jsonOutput bool) error {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(entries)
	}

	if len(entries) == 0 {
		fmt.Println("(no entries)")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "CIDR\tREMOTE\tNODE\tIFACE\tPROTO\tHEALTHY\tVNI\tMTU\n") //nolint:errcheck

	for _, e := range entries {
		healthStr := "yes"
		if !e.Healthy {
			healthStr = "NO"
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", //nolint:errcheck
			e.CIDR, e.Remote, e.Node, e.Interface, e.Protocol, healthStr, e.VNI, e.MTU)
	}

	_ = w.Flush() //nolint:errcheck

	fmt.Printf("\n%d entries\n", len(entries))

	return nil
}

// formatKey renders an LPM key as a CIDR string. v4-mapped entries are
// unmapped to dotted-quad with the prefix length offset removed.
func formatKey(key ebpfpkg.LpmKey) string {
	if ebpfpkg.IsV4Mapped(key.Addr) {
		ip := net.IPv4(key.Addr[12], key.Addr[13], key.Addr[14], key.Addr[15])

		prefix := int(key.Prefixlen) - 96
		if prefix < 0 {
			prefix = 0
		}

		return fmt.Sprintf("%s/%d", ip.String(), prefix)
	}

	ip := net.IP(append([]byte(nil), key.Addr[:]...))

	return fmt.Sprintf("%s/%d", ip.String(), key.Prefixlen)
}

// formatEndpoint renders a 16-byte underlay endpoint as either a v4
// dotted-quad (if it's IPv4-mapped) or the canonical v6 form.
func formatEndpoint(addr [16]byte) string {
	if ebpfpkg.IsV4Mapped(addr) {
		return net.IPv4(addr[12], addr[13], addr[14], addr[15]).String()
	}

	return net.IP(append([]byte(nil), addr[:]...)).String()
}

// familyOfKey returns "v4" or "v6" depending on whether the key is in
// the IPv4-mapped IPv6 prefix.
func familyOfKey(key ebpfpkg.LpmKey) string {
	if ebpfpkg.IsV4Mapped(key.Addr) {
		return "v4"
	}

	return "v6"
}

func familyName(f int) string {
	switch f {
	case familyV4:
		return "v4"
	case familyV6:
		return "v6"
	}

	return ""
}

func familyFilterName(f int) string {
	switch f {
	case familyV4:
		return "v4"
	case familyV6:
		return "v6"
	}

	return "v4 or v6"
}

// familyMatches reports whether the given family ("v4" or "v6") satisfies
// the requested filter.
func familyMatches(filter int, family string) bool {
	switch filter {
	case familyV4:
		return family == "v4"
	case familyV6:
		return family == "v6"
	}

	return true
}

// encodeKey serializes an LpmKey to its on-the-wire byte form (the same
// bytes the kernel stores).
func encodeKey(key ebpfpkg.LpmKey) []byte {
	out := make([]byte, 4+16)
	binary.NativeEndian.PutUint32(out[0:4], key.Prefixlen)
	copy(out[4:], key.Addr[:])

	return out
}

// encodeValue serializes a TunnelEndpointC to its on-the-wire byte form.
func encodeValue(val ebpfpkg.RawTunnelEndpoint) []byte {
	const nhSize = 16 + 4 + 4 + 4 + 4 // RemoteEndpoint + 4*uint32

	out := make([]byte, nhSize*ebpfpkg.MaxNexthops+4)

	for i := 0; i < ebpfpkg.MaxNexthops; i++ {
		base := i * nhSize
		copy(out[base:base+16], val.Nexthops[i].RemoteEndpoint[:])
		binary.NativeEndian.PutUint32(out[base+16:base+20], val.Nexthops[i].Vni)
		binary.NativeEndian.PutUint32(out[base+20:base+24], val.Nexthops[i].Ifindex)
		binary.NativeEndian.PutUint32(out[base+24:base+28], val.Nexthops[i].Healthy)
		binary.NativeEndian.PutUint32(out[base+28:base+32], val.Nexthops[i].Protocol)
	}

	binary.NativeEndian.PutUint32(out[nhSize*ebpfpkg.MaxNexthops:], val.Count)

	return out
}

// peerInfo holds the node name for a CIDR or endpoint.
type peerInfo struct {
	Name string
}

// statusPeer is a subset of the status JSON peer structure.
type statusPeer struct {
	Name            string   `json:"name"`
	InternalIPs     []string `json:"internalIPs"`
	PodCidrGateways []string `json:"podCidrGateways"`
	Tunnel          struct {
		Endpoint   string   `json:"endpoint"`
		AllowedIPs []string `json:"allowedIPs"`
	} `json:"tunnel"`
}

// statusJSON is a subset of the status JSON structure.
type statusJSON struct {
	Peers []statusPeer `json:"peers"`
}

// peerMaps holds CIDR-keyed and endpoint-keyed peer info maps.
type peerMaps struct {
	byCIDR     map[string]peerInfo
	byEndpoint map[string]peerInfo
}

// fetchPeerMaps queries the local status endpoint and builds lookup maps.
func fetchPeerMaps(statusPort int) peerMaps {
	result := peerMaps{
		byCIDR:     make(map[string]peerInfo),
		byEndpoint: make(map[string]peerInfo),
	}
	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/status/json", statusPort))
	if err != nil {
		return result
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result
	}

	var status statusJSON
	if err := json.Unmarshal(body, &status); err != nil {
		return result
	}

	for _, p := range status.Peers {
		info := peerInfo{Name: p.Name}
		for _, cidr := range p.Tunnel.AllowedIPs {
			result.byCIDR[cidr] = info
		}

		if p.Tunnel.Endpoint != "" {
			result.byEndpoint[p.Tunnel.Endpoint] = info
			if host, _, err := net.SplitHostPort(p.Tunnel.Endpoint); err == nil {
				result.byEndpoint[host] = info
			}
		}

		for _, ip := range p.InternalIPs {
			result.byEndpoint[ip] = info
		}

		for _, gw := range p.PodCidrGateways {
			result.byEndpoint[gw] = info
		}
	}

	return result
}

// annotateEntries enriches entries with the destination node name from the
// local status endpoint.
func annotateEntries(entries []entry, statusPort int) {
	pm := fetchPeerMaps(statusPort)
	if len(pm.byCIDR) == 0 && len(pm.byEndpoint) == 0 {
		return
	}

	for i := range entries {
		if info, ok := pm.byEndpoint[entries[i].Remote]; ok {
			entries[i].Node = info.Name
			continue
		}

		if info, ok := pm.byCIDR[entries[i].CIDR]; ok {
			entries[i].Node = info.Name
			continue
		}

		if _, cidr, err := net.ParseCIDR(entries[i].CIDR); err == nil {
			gw := make(net.IP, len(cidr.IP))
			copy(gw, cidr.IP)

			if ip4 := gw.To4(); ip4 != nil {
				ip4[3]++
				if info, ok := pm.byEndpoint[ip4.String()]; ok {
					entries[i].Node = info.Name
				}
			} else if len(gw) == 16 {
				gw[15]++
				if info, ok := pm.byEndpoint[gw.String()]; ok {
					entries[i].Node = info.Name
				}
			}
		}
	}
}
