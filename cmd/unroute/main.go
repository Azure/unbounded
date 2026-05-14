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
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	flag "github.com/spf13/pflag"

	ebpfpkg "github.com/Azure/unbounded/internal/net/ebpf"
	"github.com/Azure/unbounded/internal/version"
)

// entry is the per-nexthop row used for text output. We emit one row per
// nexthop so the tabular format stays scannable.
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

// cidrGroup is the per-CIDR JSON shape used by `unroute -j`: one object
// per LPM trie entry with all nexthops collapsed under an `endpoints`
// array. Each endpoint mirrors a single nexthop slot.
type cidrGroup struct {
	CIDR      string         `json:"cidr"`
	Family    string         `json:"family"`
	Endpoints []endpointJSON `json:"endpoints"`
}

type endpointJSON struct {
	Remote    string `json:"remote"`
	Node      string `json:"node,omitempty"`
	Interface string `json:"interface"`
	Protocol  string `json:"protocol"`
	Healthy   bool   `json:"healthy"`
	VNI       uint32 `json:"vni"`
	MTU       int    `json:"mtu"`
	IfIndex   uint32 `json:"ifindex"`
}

// rawEntry mirrors `bpftool map dump -j`: each entry is a structured
// key/value object with all bytes exposed as decimal arrays so the
// agent's pushed state can be compared byte-for-byte against what the
// kernel holds.
type rawEntry struct {
	Key   rawKey   `json:"key"`
	Value rawValue `json:"value"`
}

type rawKey struct {
	Prefixlen uint32   `json:"prefixlen"`
	Addr      byteList `json:"addr"`
}

type rawValue struct {
	Nexthops []rawNexthop `json:"nexthops"`
	Count    uint32       `json:"count"`
}

type rawNexthop struct {
	RemoteEndpoint byteList `json:"remote_endpoint"`
	VNI            uint32   `json:"vni"`
	IfIndex        uint32   `json:"ifindex"`
	Healthy        uint32   `json:"healthy"`
	Protocol       uint32   `json:"protocol"`
}

// byteList is a slice of bytes that marshals to JSON as a decimal array
// (e.g. [0, 0, 255, 255]) rather than encoding/json's default base64
// representation. This matches `bpftool map dump -j` output.
type byteList []uint8

// MarshalJSON implements json.Marshaler for byteList.
func (b byteList) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte("[]"), nil
	}

	out := make([]byte, 0, 4*len(b)+2)
	out = append(out, '[')

	for i, v := range b {
		if i > 0 {
			out = append(out, ',')
		}

		out = strconv.AppendUint(out, uint64(v), 10)
	}

	out = append(out, ']')

	return out, nil
}

func main() {
	var (
		showVersion bool
		jsonOutput  bool
		rawOutput   bool
		traceMode   bool
		statusPort  int
		v4Only      bool
		v6Only      bool
		colorMode   string
	)

	flag.BoolVarP(&jsonOutput, "json", "j", false, "Output entries as JSON")
	flag.BoolVarP(&rawOutput, "raw", "r", false, "Dump map entries as raw key/value hex")
	flag.BoolVarP(&traceMode, "trace", "t", false, "Stream per-packet trace events from the eBPF program (Ctrl-C to stop)")
	flag.BoolVarP(&v4Only, "v4-only", "4", false, "Show only IPv4 entries")
	flag.BoolVarP(&v6Only, "v6-only", "6", false, "Show only IPv6 entries")
	flag.StringVar(&colorMode, "color", "auto", "Color output: auto, always, never")
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

	useColor, colorErr := resolveColorMode(colorMode, os.Stdout)
	if colorErr != nil {
		fmt.Fprintf(os.Stderr, "unroute: %v\n", colorErr)
		os.Exit(2)
	}

	familyFilter := familyAll
	switch {
	case v4Only:
		familyFilter = familyV4
	case v6Only:
		familyFilter = familyV6
	}

	textOpts := textOptions{useColor: useColor}

	args := flag.Args()
	if len(args) > 0 {
		if err := lookupEntry(args[0], jsonOutput, statusPort, familyFilter, textOpts); err != nil {
			fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
			os.Exit(1)
		}

		return
	}

	if traceMode {
		if err := streamTrace(textOpts); err != nil {
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

	if err := dumpTunnelEndpoints(jsonOutput, statusPort, familyFilter, textOpts); err != nil {
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
func dumpTunnelEndpoints(jsonOutput bool, statusPort, familyFilter int, opts textOptions) error {
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

	return printEntries(entries, jsonOutput, opts)
}

// dumpRaw prints the LPM trie in a structured form matching
// `bpftool map dump -j`: each entry is a JSON object whose key and value
// expose every byte. No annotation, no interface resolution, no status
// server roundtrip. Useful for byte-exact comparison against what the
// agent believes it pushed into the kernel.
//
// Without --json the output is the same JSON structure pretty-printed
// (this matches user expectations from bpftool, which also emits its
// "raw" form as JSON-ish text).
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
		if !familyMatches(familyFilter, familyOfKey(key)) {
			continue
		}

		rows = append(rows, makeRawEntry(key, val))
	}

	if err := iter.Err(); err != nil {
		return fmt.Errorf("iterate %s: %w", ebpfpkg.MapName, err)
	}

	sort.Slice(rows, func(i, j int) bool {
		// Sort by raw addr bytes for stable output.
		for k := range rows[i].Key.Addr {
			if rows[i].Key.Addr[k] != rows[j].Key.Addr[k] {
				return rows[i].Key.Addr[k] < rows[j].Key.Addr[k]
			}
		}

		return rows[i].Key.Prefixlen < rows[j].Key.Prefixlen
	})

	_ = jsonOutput // raw output is always JSON

	// Emit a JSON array with one compact entry per line so byte arrays
	// stay inline like `bpftool map dump -j` and the output remains
	// jq-friendly.
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush() //nolint:errcheck

	if _, err := out.WriteString("[\n"); err != nil {
		return err
	}

	for i, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("marshal entry %d: %w", i, err)
		}

		sep := ","
		if i == len(rows)-1 {
			sep = ""
		}

		if _, err := fmt.Fprintf(out, "  %s%s\n", b, sep); err != nil {
			return err
		}
	}

	_, err = out.WriteString("]\n")

	return err
}

// makeRawEntry decodes a single LPM trie key/value pair into the
// bpftool-style structured representation.
func makeRawEntry(key ebpfpkg.LpmKey, val ebpfpkg.RawTunnelEndpoint) rawEntry {
	out := rawEntry{
		Key: rawKey{
			Prefixlen: key.Prefixlen,
			Addr:      append(byteList(nil), key.Addr[:]...),
		},
		Value: rawValue{
			Count:    val.Count,
			Nexthops: make([]rawNexthop, 0, val.Count),
		},
	}

	for i := uint32(0); i < val.Count && i < uint32(ebpfpkg.MaxNexthops); i++ {
		nh := val.Nexthops[i]
		out.Value.Nexthops = append(out.Value.Nexthops, rawNexthop{
			RemoteEndpoint: append(byteList(nil), nh.RemoteEndpoint[:]...),
			VNI:            nh.Vni,
			IfIndex:        nh.Ifindex,
			Healthy:        nh.Healthy,
			Protocol:       nh.Protocol,
		})
	}

	return out
}

// lookupEntry performs an LPM lookup for the supplied IP address against
// the unified trie. The IP is mapped to v6 form before lookup; the family
// filter, if set, must match the IP's natural family.
func lookupEntry(ipStr string, jsonOutput bool, statusPort, familyFilter int, opts textOptions) error {
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

	return printEntries(entries, jsonOutput, opts)
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

// textOptions controls human-readable output rendering.
type textOptions struct {
	useColor bool
}

// resolveColorMode parses --color=auto|always|never and decides whether
// to emit ANSI color escapes. auto = enabled if the writer is a TTY.
func resolveColorMode(mode string, w *os.File) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto", "":
		if w == nil {
			return false, nil
		}

		fi, err := w.Stat()
		if err != nil {
			return false, nil
		}

		return (fi.Mode() & os.ModeCharDevice) != 0, nil
	default:
		return false, fmt.Errorf("invalid --color value %q (want auto|always|never)", mode)
	}
}

// ANSI color helpers.
const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
)

func colorize(s, code string, on bool) string {
	if !on || code == "" {
		return s
	}

	return code + s + ansiReset
}

// printEntries renders a slice of entries. Text output mimics `ip route`:
// one summary line per CIDR with the nexthop appended when there is only
// one, or a CIDR header followed by indented "nexthop ..." lines for ECMP.
// JSON output groups nexthops under their CIDR into a single object per
// LPM trie entry with an `endpoints` array.
func printEntries(entries []entry, jsonOutput bool, opts textOptions) error {
	if jsonOutput {
		groups := groupByCIDR(entries)

		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(groups)
	}

	if len(entries) == 0 {
		fmt.Println("(no entries)")
		return nil
	}

	groups := groupByCIDR(entries)

	w := bufio.NewWriter(os.Stdout)
	defer w.Flush() //nolint:errcheck

	for _, g := range groups {
		switch len(g.Endpoints) {
		case 0:
			// Should not happen (an LPM entry without nexthops is
			// filtered out upstream), but be defensive.
			fmt.Fprintf(w, "%s (no endpoints)\n", g.CIDR)
		case 1:
			ep := g.Endpoints[0]
			fmt.Fprintf(w, "%s %s\n", g.CIDR, renderEndpoint(ep, false, opts))
		default:
			fmt.Fprintf(w, "%s\n", g.CIDR)
			for _, ep := range g.Endpoints {
				fmt.Fprintf(w, "    %s\n", renderEndpoint(ep, true, opts))
			}
		}
	}

	return nil
}

// renderEndpoint formats a single endpoint as an ip-route-style line
// fragment. If asNexthop is true, the line is prefixed with "nexthop "
// and includes a default weight; otherwise the fields are emitted
// inline for the single-nexthop summary line. The healthy/unhealthy
// tag is always present and is the only field that receives color.
func renderEndpoint(ep endpointJSON, asNexthop bool, opts textOptions) string {
	var sb strings.Builder

	if asNexthop {
		sb.WriteString("nexthop ")
	}

	if ep.Remote != "" {
		sb.WriteString("via ")
		sb.WriteString(ep.Remote)
		sb.WriteByte(' ')
	}

	if ep.Interface != "" {
		sb.WriteString("dev ")
		sb.WriteString(ep.Interface)
		sb.WriteByte(' ')
	}

	if ep.Protocol != "" {
		sb.WriteString("proto ")
		sb.WriteString(ep.Protocol)
		sb.WriteByte(' ')
	}

	if ep.Node != "" {
		sb.WriteString("node ")
		sb.WriteString(ep.Node)
		sb.WriteByte(' ')
	}

	if ep.VNI != 0 {
		fmt.Fprintf(&sb, "vni %d ", ep.VNI)
	}

	if ep.MTU != 0 {
		fmt.Fprintf(&sb, "mtu %d ", ep.MTU)
	}

	if asNexthop {
		sb.WriteString("weight 1 ")
	}

	tag := "healthy"
	color := ansiGreen
	if !ep.Healthy {
		tag = "unhealthy"
		color = ansiRed
	}

	sb.WriteString(colorize(tag, color, opts.useColor))

	return sb.String()
}

// groupByCIDR collapses a per-nexthop entry slice into a per-CIDR slice
// with nexthops under each CIDR's `endpoints` array. Preserves the input
// order of CIDRs (caller is expected to have sorted them already).
func groupByCIDR(entries []entry) []cidrGroup {
	groups := make([]cidrGroup, 0)
	idx := make(map[string]int, len(entries))

	for _, e := range entries {
		i, ok := idx[e.CIDR]
		if !ok {
			i = len(groups)
			idx[e.CIDR] = i
			groups = append(groups, cidrGroup{
				CIDR:   e.CIDR,
				Family: e.Family,
			})
		}

		groups[i].Endpoints = append(groups[i].Endpoints, endpointJSON{
			Remote:    e.Remote,
			Node:      e.Node,
			Interface: e.Interface,
			Protocol:  e.Protocol,
			Healthy:   e.Healthy,
			VNI:       e.VNI,
			MTU:       e.MTU,
			IfIndex:   e.IfIndex,
		})
	}

	return groups
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
