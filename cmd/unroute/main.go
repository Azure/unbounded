// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// unroute dumps the eBPF LPM trie tunnel maps (unbounded_endpoints_v4 and
// unbounded_endpoints_v6) in human-readable or JSON format. It can also perform
// a longest-prefix-match lookup for a specific IP address or dump the
// local_cidrs map.
package main

import (
	"encoding/binary"
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

	"github.com/Azure/unbounded-kube/internal/net/buildinfo"
)

const (
	mapName   = "unbounded_endpo" // truncated from unbounded_endpoints_v{4,6}
	v4KeySize = 8                 // sizeof(lpmKeyV4)
	v6KeySize = 20                // sizeof(lpmKeyV6)
)

// lpmKeyV4 matches struct lpm_key_v4 in the BPF programs.
type lpmKeyV4 struct {
	Prefixlen uint32
	Addr      uint32
}

// lpmKeyV6 matches struct lpm_key_v6 in the BPF programs.
type lpmKeyV6 struct {
	Prefixlen uint32
	Addr      [4]uint32
}

const maxNexthops = 4

// tunnelNexthopV4 matches struct tunnel_nexthop in the BPF programs.
type tunnelNexthopV4 struct {
	RemoteIPv4 uint32
	VNI        uint32
	IfIndex    uint32
	Flags      uint32
	Protocol   uint32
}

// tunnelEndpointV4 matches struct tunnel_endpoint_v4 in the BPF programs.
type tunnelEndpointV4 struct {
	Nexthops [maxNexthops]tunnelNexthopV4
	Count    uint32
}

// tunnelNexthopV6 matches struct tunnel_nexthop_v6 in the BPF programs.
type tunnelNexthopV6 struct {
	RemoteIPv6 [4]uint32
	VNI        uint32
	IfIndex    uint32
	Flags      uint32
	Protocol   uint32
}

// tunnelEndpointV6 matches struct tunnel_endpoint_v6 in the BPF programs.
type tunnelEndpointV6 struct {
	Nexthops [maxNexthops]tunnelNexthopV6
	Count    uint32
}

// entry is the unified representation for display output.
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
}

func main() {
	var (
		showLocal   bool
		showVersion bool
		jsonOutput  bool
		statusPort  int
	)

	flag.BoolVarP(&jsonOutput, "json", "j", false, "Output entries as JSON array")
	flag.BoolVar(&showLocal, "local", false, "Dump the local_cidrs map instead of tunnel endpoints")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.IntVar(&statusPort, "status-port", 9998, "Port of the local node status endpoint")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: unroute [options] [IP_ADDRESS]\n\n")
		fmt.Fprintf(os.Stderr, "Dump the eBPF LPM trie tunnel maps in human-readable format.\n")
		fmt.Fprintf(os.Stderr, "If an IP address is given, perform a longest-prefix-match lookup.\n\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if showVersion {
		fmt.Printf("unroute %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime)
		os.Exit(0)
	}

	if showLocal {
		if err := dumpLocalCIDRs(); err != nil {
			fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
			os.Exit(1)
		}

		return
	}

	args := flag.Args()
	if len(args) > 0 {
		if err := lookupEntry(args[0], jsonOutput, statusPort); err != nil {
			fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
			os.Exit(1)
		}

		return
	}

	if err := dumpTunnelEndpoints(jsonOutput, statusPort); err != nil {
		fmt.Fprintf(os.Stderr, "unroute: %v\n", err)
		os.Exit(1)
	}
}

// protocolName returns the tunnel protocol name for the given protocol constant.
func protocolName(proto uint32) string {
	switch proto {
	case 1:
		return "GENEVE"
	case 2:
		return "VXLAN"
	case 3:
		return "IPIP"
	case 4:
		return "WireGuard"
	case 5:
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

// findMapsByName scans all loaded BPF maps and returns every map matching
// the given name.
func findMapsByName(name string) ([]*ebpf.Map, error) {
	var maps []*ebpf.Map

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

		if info.Name == name {
			maps = append(maps, m)
		} else {
			_ = m.Close() //nolint:errcheck
		}
	}

	if len(maps) == 0 {
		return nil, fmt.Errorf("BPF map %q not found (are the BPF programs loaded?)", name)
	}

	return maps, nil
}

// findMapByNameAndKeySize scans loaded BPF maps for one matching both name
// and key size.
func findMapByNameAndKeySize(name string, keySize uint32) (*ebpf.Map, error) {
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

		if info.Name == name && info.KeySize == keySize {
			return m, nil
		}

		_ = m.Close() //nolint:errcheck
	}

	return nil, fmt.Errorf("BPF map %q (key size %d) not found", name, keySize)
}

// uint32ToIPv4LE converts a uint32 stored via LittleEndian.Uint32 back to
// a dotted-decimal IPv4 string.
func uint32ToIPv4LE(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, v)

	return ip
}

// uint32ToIPv4BE converts a uint32 stored via BigEndian.Uint32 (network byte
// order) back to a dotted-decimal IPv4 string.
func uint32ToIPv4BE(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)

	return ip
}

// uint32ArrayToIPv6 converts a [4]uint32 array where each segment was stored
// via LittleEndian.Uint32 back to a 16-byte IPv6 address.
func uint32ArrayToIPv6(arr [4]uint32) net.IP {
	ip := make(net.IP, 16)
	binary.LittleEndian.PutUint32(ip[0:4], arr[0])
	binary.LittleEndian.PutUint32(ip[4:8], arr[1])
	binary.LittleEndian.PutUint32(ip[8:12], arr[2])
	binary.LittleEndian.PutUint32(ip[12:16], arr[3])

	return ip
}

// formatIP returns the string representation of an IP, using IPv4 notation
// for IPv4-mapped IPv6 addresses.
func formatIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}

	return ip.String()
}

// makeEntriesV4 builds entries from v4 key/value pairs, one per nexthop.
func makeEntriesV4(key lpmKeyV4, val tunnelEndpointV4) []entry {
	var entries []entry

	cidr := fmt.Sprintf("%s/%d", uint32ToIPv4LE(key.Addr), key.Prefixlen)

	for i := uint32(0); i < val.Count && i < maxNexthops; i++ {
		nh := val.Nexthops[i]
		ifName, mtu := resolveInterface(nh.IfIndex)
		entries = append(entries, entry{
			CIDR:      cidr,
			Remote:    uint32ToIPv4BE(nh.RemoteIPv4).String(),
			Interface: ifName,
			Protocol:  protocolName(nh.Protocol),
			Healthy:   nh.Flags&0x02 != 0,
			VNI:       nh.VNI,
			MTU:       mtu,
			IfIndex:   nh.IfIndex,
		})
	}

	return entries
}

// makeEntriesV6 builds entries from v6 key/value pairs, one per nexthop.
func makeEntriesV6(key lpmKeyV6, val tunnelEndpointV6) []entry {
	var entries []entry

	cidr := fmt.Sprintf("%s/%d", formatIP(uint32ArrayToIPv6(key.Addr)), key.Prefixlen)

	for i := uint32(0); i < val.Count && i < maxNexthops; i++ {
		nh := val.Nexthops[i]
		ifName, mtu := resolveInterface(nh.IfIndex)

		var remote string
		if nh.Flags&0x04 != 0 {
			// IPv6 underlay
			remote = formatIP(uint32ArrayToIPv6(nh.RemoteIPv6))
		} else {
			// IPv4 underlay (stored in first element of union)
			remote = uint32ToIPv4BE(nh.RemoteIPv6[0]).String()
		}

		entries = append(entries, entry{
			CIDR:      cidr,
			Remote:    remote,
			Interface: ifName,
			Protocol:  protocolName(nh.Protocol),
			Healthy:   nh.Flags&0x02 != 0,
			VNI:       nh.VNI,
			MTU:       mtu,
			IfIndex:   nh.IfIndex,
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

// collectV4Entries iterates a v4 tunnel endpoint map and appends entries.
func collectV4Entries(m *ebpf.Map, entries []entry) ([]entry, error) {
	var (
		key lpmKeyV4
		val tunnelEndpointV4
	)

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		entries = append(entries, makeEntriesV4(key, val)...)
	}

	if err := iter.Err(); err != nil {
		return entries, fmt.Errorf("iterate unbounded_endpoints_v4: %w", err)
	}

	return entries, nil
}

// collectV6Entries iterates a v6 tunnel endpoint map and appends entries.
func collectV6Entries(m *ebpf.Map, entries []entry) ([]entry, error) {
	var (
		key lpmKeyV6
		val tunnelEndpointV6
	)

	iter := m.Iterate()
	for iter.Next(&key, &val) {
		entries = append(entries, makeEntriesV6(key, val)...)
	}

	if err := iter.Err(); err != nil {
		return entries, fmt.Errorf("iterate unbounded_endpoints_v6: %w", err)
	}

	return entries, nil
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

// fetchPeerMaps queries the local status endpoint and builds lookup maps
// keyed by CIDR and by tunnel endpoint (underlay IP).
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
			// Also index by IP without port so BPF entries (which
			// store raw underlay IPs) can be matched.
			if host, _, err := net.SplitHostPort(p.Tunnel.Endpoint); err == nil {
				result.byEndpoint[host] = info
			}
		}
		// Index by internal IPs for peers without tunnel endpoints.
		for _, ip := range p.InternalIPs {
			result.byEndpoint[ip] = info
		}
		// Index pod CIDR gateways so that per-node BPF entries can
		// be resolved by their CIDR's first usable IP.
		for _, gw := range p.PodCidrGateways {
			result.byEndpoint[gw] = info
		}
	}

	return result
}

// annotateEntries enriches entries with the destination node name from the
// local status endpoint. It first tries remote (underlay) IP match, then
// falls back to CIDR match. Remote IP is checked first because multi-nexthop
// entries share the same CIDR but have different remote IPs pointing to
// different nodes.
func annotateEntries(entries []entry, statusPort int) {
	pm := fetchPeerMaps(statusPort)
	if len(pm.byCIDR) == 0 && len(pm.byEndpoint) == 0 {
		return
	}

	for i := range entries {
		// Try remote (underlay) IP first -- this correctly resolves
		// multi-nexthop entries where each nexthop targets a different node.
		if info, ok := pm.byEndpoint[entries[i].Remote]; ok {
			entries[i].Node = info.Name
			continue
		}

		if info, ok := pm.byCIDR[entries[i].CIDR]; ok {
			entries[i].Node = info.Name
			continue
		}
		// Try the CIDR's first usable IP (gateway IP) as a lookup key.
		// This resolves per-node pod CIDR entries (e.g. 100.80.0.0/24)
		// by matching against podCidrGateways (100.80.0.1).
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

// dumpTunnelEndpoints iterates all tunnel endpoint maps (v4 and v6) and
// prints their entries.
func dumpTunnelEndpoints(jsonOutput bool, statusPort int) error {
	maps, err := findMapsByName(mapName)
	if err != nil {
		return err
	}

	defer func() {
		for _, m := range maps {
			_ = m.Close() //nolint:errcheck
		}
	}()

	var entries []entry

	for _, m := range maps {
		info, err := m.Info()
		if err != nil {
			return fmt.Errorf("map info: %w", err)
		}

		switch info.KeySize {
		case v4KeySize:
			entries, err = collectV4Entries(m, entries)
		case v6KeySize:
			entries, err = collectV6Entries(m, entries)
		default:
			continue
		}

		if err != nil {
			return err
		}
	}

	annotateEntries(entries, statusPort)
	sort.Slice(entries, func(i, j int) bool { return entries[i].CIDR < entries[j].CIDR })

	return printEntries(entries, jsonOutput)
}

// dumpLocalCIDRs iterates the local_cidrs map and prints each entry.
func dumpLocalCIDRs() error {
	maps, err := findMapsByName("local_cidrs")
	if err != nil {
		return err
	}

	defer func() {
		for _, m := range maps {
			_ = m.Close() //nolint:errcheck
		}
	}()

	count := 0

	for _, m := range maps {
		var (
			key lpmKeyV4
			val uint32
		)

		iter := m.Iterate()
		for iter.Next(&key, &val) {
			cidr := fmt.Sprintf("%s/%d", uint32ToIPv4LE(key.Addr), key.Prefixlen)
			fmt.Printf("%s (value=%d)\n", cidr, val)

			count++
		}

		if err := iter.Err(); err != nil {
			return fmt.Errorf("iterate local_cidrs: %w", err)
		}
	}

	if count == 0 {
		fmt.Println("(no entries)")
	} else {
		fmt.Printf("\n%d entries\n", count)
	}

	return nil
}

// lookupEntry looks up a single IP address in the appropriate tunnel
// endpoint trie (v4 or v6) and displays the matching entry.
func lookupEntry(ipStr string, jsonOutput bool, statusPort int) error {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("invalid IP address: %s", ipStr)
	}

	var entries []entry

	if ip4 := ip.To4(); ip4 != nil {
		m, err := findMapByNameAndKeySize(mapName, v4KeySize)
		if err != nil {
			return err
		}

		defer func() { _ = m.Close() }() //nolint:errcheck

		key := lpmKeyV4{
			Prefixlen: 32,
			Addr:      binary.LittleEndian.Uint32(ip4),
		}

		var val tunnelEndpointV4
		if err := m.Lookup(&key, &val); err != nil {
			return fmt.Errorf("lookup %s: %w", ipStr, err)
		}

		entries = makeEntriesV4(key, val)
	} else {
		ip6 := ip.To16()
		if ip6 == nil {
			return fmt.Errorf("invalid IP address: %s", ipStr)
		}

		m, err := findMapByNameAndKeySize(mapName, v6KeySize)
		if err != nil {
			return err
		}

		defer func() { _ = m.Close() }() //nolint:errcheck

		key := lpmKeyV6{Prefixlen: 128}
		key.Addr[0] = binary.LittleEndian.Uint32(ip6[0:4])
		key.Addr[1] = binary.LittleEndian.Uint32(ip6[4:8])
		key.Addr[2] = binary.LittleEndian.Uint32(ip6[8:12])
		key.Addr[3] = binary.LittleEndian.Uint32(ip6[12:16])

		var val tunnelEndpointV6
		if err := m.Lookup(&key, &val); err != nil {
			return fmt.Errorf("lookup %s: %w", ipStr, err)
		}

		entries = makeEntriesV6(key, val)
	}

	annotateEntries(entries, statusPort)

	return printEntries(entries, jsonOutput)
}
