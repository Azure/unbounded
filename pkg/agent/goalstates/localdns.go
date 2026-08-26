// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/template"

	utilnet "k8s.io/apimachinery/pkg/util/net"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const (
	CoreDNSVersion        = "1.12.3"
	LocalDNSInterfaceName = "localdns"

	// The next three have no Go reader, and are kept anyway. Their values are
	// a contract with code outside this module: the Corefile and upstreams
	// paths are hardcoded in
	// pkg/agent/phases/rootfs/assets/localdns-supervisor.sh, and the nftables
	// rule comment is asserted by hack/agent/e2e-kind/e2e.py. "No caller" here
	// means "no Go caller", which is not the same thing.
	LocalDNSCorefilePath  = "/etc/unbounded/localdns/Corefile"
	LocalDNSUpstreamsPath = "/etc/unbounded/localdns/node-upstreams"
	LocalDNSRuleComment   = "unbounded-localdns: skip conntrack"

	LocalDNSServiceUnit       = "localdns.service"
	LocalDNSSliceUnit         = "localdns.slice"
	LocalDNSReadinessPort     = 8181
	LocalDNSMetricsPort       = 9253
	LocalDNSNFTTable          = "unbounded_localdns"
	LocalDNSNetworkUnit       = "unbounded-localdns-network.service"
	LocalDNSSupervisorPath    = "/usr/local/libexec/unbounded-localdns-supervisor"
	LocalDNSCoreDNSBinaryPath = "/usr/local/bin/coredns"
)

const (
	hostResolvConfPath            = "/etc/resolv.conf"
	systemdResolvedResolvConfPath = "/run/systemd/resolve/resolv.conf"
)

type localDNSResolverDeps struct {
	readFile        func(string) ([]byte, error)
	resolvedDomains func() (string, error)
}

type localDNSMetricsDeps struct {
	interfaceAddrs     func() ([]net.Addr, error)
	lookupIP           func(string) ([]net.IP, error)
	resolveBindAddress func(net.IP) (net.IP, error)
}

func defaultLocalDNSResolverDeps() localDNSResolverDeps {
	return localDNSResolverDeps{
		readFile: os.ReadFile,
		resolvedDomains: func() (string, error) {
			output, err := exec.Command("resolvectl", "domain").CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("query systemd-resolved routing domains: %w: %s", err, strings.TrimSpace(string(output)))
			}

			return string(output), nil
		},
	}
}

func defaultLocalDNSMetricsDeps() localDNSMetricsDeps {
	return localDNSMetricsDeps{
		interfaceAddrs:     net.InterfaceAddrs,
		lookupIP:           net.LookupIP,
		resolveBindAddress: utilnet.ResolveBindAddress,
	}
}

var baselineLocalDNSPlugins = []string{
	"bind", "cache", "errors", "forward", "loop", "prometheus", "ready", "whoami",
}

//go:embed assets/default-localdns.Corefile.tmpl
var defaultLocalDNSCorefileTemplate string

// LocalDNS is the fully resolved machine-local DNS goal state.
type LocalDNS struct {
	Enabled                bool
	CoreDNSVersion         string
	NodeListenerIP         netip.Addr
	ClusterListenerIP      netip.Addr
	NodeUpstreamIPs        []netip.Addr
	ClusterDNSServiceIP    netip.Addr
	MetricsAddress         string
	CPULimitInMilliCores   int
	MemoryLimitInMB        int
	RequiredPlugins        []string
	Corefile               []byte
	OriginalHostResolvConf []byte
}

// LocalDNSCorefileTemplateData contains validated runtime values available to Corefile templates.
type LocalDNSCorefileTemplateData struct {
	NodeListenerIP        string
	ClusterListenerIP     string
	NodeUpstreamIPs       []string
	NodeUpstreamIPsJoined string
	ClusterDNSServiceIP   string
	MetricsAddress        string
}

type resolvedLocalDNSConfig struct {
	coreDNSVersion   string
	nodeListener     netip.Addr
	clusterListener  netip.Addr
	clusterDNS       netip.Addr
	metricsAddress   string
	cpuLimit         int
	memoryLimit      int
	requiredPlugins  []string
	corefileTemplate string
}

func resolveLocalDNSConfig(cfg *config.AgentConfig, downloads *DownloadOverrides) (resolvedLocalDNSConfig, error) {
	if err := cfg.Validate(); err != nil {
		return resolvedLocalDNSConfig{}, err
	}

	resolved := resolvedLocalDNSConfig{
		coreDNSVersion:   CoreDNSVersion,
		nodeListener:     netip.MustParseAddr(valueOrDefault(cfg.LocalDNS.NodeListenerIP, config.DefaultLocalDNSNodeListenerIP)),
		clusterListener:  netip.MustParseAddr(valueOrDefault(cfg.LocalDNS.ClusterListenerIP, config.DefaultLocalDNSClusterListenerIP)),
		clusterDNS:       netip.MustParseAddr(strings.TrimSpace(cfg.Cluster.ClusterDNS)),
		cpuLimit:         config.DefaultLocalDNSCPUMilliCores,
		memoryLimit:      config.DefaultLocalDNSMemoryLimitMB,
		corefileTemplate: cfg.LocalDNS.CorefileTemplate,
	}

	var err error

	resolved.metricsAddress, err = localDNSMetricsAddress(
		cfg.LocalDNS.MetricsAddress,
		cfg.Kubelet.NodeIP,
		cfg.NodeName,
		defaultLocalDNSMetricsDeps(),
	)
	if err != nil {
		return resolvedLocalDNSConfig{}, err
	}

	if cfg.LocalDNS.CPULimitInMilliCores != nil {
		resolved.cpuLimit = *cfg.LocalDNS.CPULimitInMilliCores
	}

	if cfg.LocalDNS.MemoryLimitInMB != nil {
		resolved.memoryLimit = *cfg.LocalDNS.MemoryLimitInMB
	}

	if downloads != nil && downloads.CoreDNS != nil && downloads.CoreDNS.Version != "" {
		resolved.coreDNSVersion = strings.TrimPrefix(downloads.CoreDNS.Version, "v")
	}

	plugins := append([]string(nil), baselineLocalDNSPlugins...)
	plugins = append(plugins, cfg.LocalDNS.RequiredPlugins...)
	resolved.requiredPlugins = normalizePluginNames(plugins)

	if resolved.corefileTemplate == "" {
		resolved.corefileTemplate = defaultLocalDNSCorefileTemplate
	}

	return resolved, nil
}

func resolveLocalDNS(cfg *config.AgentConfig, downloads *DownloadOverrides) (LocalDNS, error) {
	if cfg.LocalDNS == nil || !cfg.LocalDNS.Enabled {
		return LocalDNS{}, nil
	}

	resolved, err := resolveLocalDNSConfig(cfg, downloads)
	if err != nil {
		return LocalDNS{}, err
	}

	resolvConf, upstreams, err := discoverLocalDNSUpstreams(defaultLocalDNSResolverDeps(), resolved.nodeListener, resolved.clusterListener)
	if err != nil {
		return LocalDNS{}, err
	}

	upstreamStrings := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		upstreamStrings = append(upstreamStrings, upstream.String())
	}

	corefile, err := renderLocalDNSCorefile(resolved.corefileTemplate, LocalDNSCorefileTemplateData{
		NodeListenerIP:        resolved.nodeListener.String(),
		ClusterListenerIP:     resolved.clusterListener.String(),
		NodeUpstreamIPs:       upstreamStrings,
		NodeUpstreamIPsJoined: strings.Join(upstreamStrings, " "),
		ClusterDNSServiceIP:   resolved.clusterDNS.String(),
		MetricsAddress:        resolved.metricsAddress,
	})
	if err != nil {
		return LocalDNS{}, err
	}

	return LocalDNS{
		Enabled:                true,
		CoreDNSVersion:         resolved.coreDNSVersion,
		NodeListenerIP:         resolved.nodeListener,
		ClusterListenerIP:      resolved.clusterListener,
		NodeUpstreamIPs:        upstreams,
		ClusterDNSServiceIP:    resolved.clusterDNS,
		MetricsAddress:         resolved.metricsAddress,
		CPULimitInMilliCores:   resolved.cpuLimit,
		MemoryLimitInMB:        resolved.memoryLimit,
		RequiredPlugins:        resolved.requiredPlugins,
		Corefile:               corefile,
		OriginalHostResolvConf: resolvConf,
	}, nil
}

func discoverLocalDNSUpstreams(deps localDNSResolverDeps, listeners ...netip.Addr) ([]byte, []netip.Addr, error) {
	resolvConf, err := deps.readFile(hostResolvConfPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read host resolver configuration: %w", err)
	}

	nameservers := localDNSNameservers(resolvConf)
	if !hasLoopbackAddress(nameservers) {
		upstreams, err := parseLocalDNSUpstreams(resolvConf, listeners...)

		return resolvConf, upstreams, err
	}

	if !isSystemdResolvedStub(nameservers) {
		return nil, nil, fmt.Errorf("host resolver uses an unsupported local caching stub; LocalDNS supports direct nameservers or the systemd-resolved stub")
	}

	domains, err := deps.resolvedDomains()
	if err != nil {
		return nil, nil, err
	}

	if hasSystemdResolvedSplitDNS(domains) {
		return nil, nil, fmt.Errorf("host systemd-resolved configuration uses unsupported split-DNS routing domains")
	}

	upstreamConf, err := deps.readFile(systemdResolvedResolvConfPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read systemd-resolved upstream configuration: %w", err)
	}

	upstreams, err := parseLocalDNSUpstreams(upstreamConf, listeners...)
	if err != nil {
		return nil, nil, fmt.Errorf("systemd-resolved upstream configuration: %w", err)
	}

	return resolvConf, upstreams, nil
}

func localDNSNameservers(resolvConf []byte) []netip.Addr {
	var addresses []netip.Addr

	for _, line := range strings.Split(string(resolvConf), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}

		addr, err := netip.ParseAddr(fields[1])
		if err == nil && addr.Is4() && !addr.IsUnspecified() && !addr.IsMulticast() {
			addresses = append(addresses, addr)
		}
	}

	return addresses
}

func hasLoopbackAddress(addresses []netip.Addr) bool {
	for _, address := range addresses {
		if address.IsLoopback() {
			return true
		}
	}

	return false
}

func isSystemdResolvedStub(addresses []netip.Addr) bool {
	if len(addresses) == 0 {
		return false
	}

	for _, address := range addresses {
		if address.String() != "127.0.0.53" && address.String() != "127.0.0.54" {
			return false
		}
	}

	return true
}

func hasSystemdResolvedSplitDNS(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		_, values, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		for _, domain := range strings.Fields(values) {
			if domain != "~." {
				return true
			}
		}
	}

	return false
}

func parseLocalDNSUpstreams(resolvConf []byte, listeners ...netip.Addr) ([]netip.Addr, error) {
	listenerSet := make(map[netip.Addr]struct{}, len(listeners))
	for _, listener := range listeners {
		listenerSet[listener] = struct{}{}
	}

	set := map[netip.Addr]struct{}{}

	for _, addr := range localDNSNameservers(resolvConf) {
		if addr.IsLoopback() {
			continue
		}

		if _, loop := listenerSet[addr]; loop {
			continue
		}

		set[addr] = struct{}{}
	}

	if len(set) == 0 {
		return nil, fmt.Errorf("host resolver contains no usable direct IPv4 nameserver")
	}

	upstreams := make([]netip.Addr, 0, len(set))
	for addr := range set {
		upstreams = append(upstreams, addr)
	}

	sort.Slice(upstreams, func(i, j int) bool { return upstreams[i].Less(upstreams[j]) })

	return upstreams, nil
}

// localDNSMetricsAddress follows kubelet's non-cloud node address selection:
// explicit node IP, IP-valued node name, host-local node-name DNS result, then
// ResolveBindAddress using the host's default route. LocalDNS selects IPv4 only.
func localDNSMetricsAddress(configured, nodeIPs, nodeName string, deps localDNSMetricsDeps) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured), nil
	}

	if strings.TrimSpace(nodeIPs) != "" {
		for _, candidate := range strings.Split(nodeIPs, ",") {
			ip := net.ParseIP(strings.TrimSpace(candidate))
			if ip == nil || ip.To4() == nil {
				continue
			}

			if err := validateLocalDNSHostIP(ip, deps.interfaceAddrs); err != nil {
				return "", fmt.Errorf("resolve LocalDNS metrics address from Kubelet.NodeIP: %w", err)
			}

			return localDNSMetricsEndpoint(ip), nil
		}

		return "", fmt.Errorf("resolve LocalDNS metrics address: Kubelet.NodeIP contains no IPv4 address")
	}

	nodeName = strings.TrimSpace(nodeName)
	if nodeNameIP := net.ParseIP(nodeName); nodeNameIP != nil {
		if nodeNameIP.To4() == nil {
			return "", fmt.Errorf("resolve LocalDNS metrics address: node name IP %s is not IPv4", nodeNameIP)
		}

		if err := validateLocalDNSHostIP(nodeNameIP, deps.interfaceAddrs); err != nil {
			return "", fmt.Errorf("resolve LocalDNS metrics address from node name: %w", err)
		}

		return localDNSMetricsEndpoint(nodeNameIP), nil
	}

	if nodeName != "" {
		addresses, lookupErr := deps.lookupIP(nodeName)
		if lookupErr == nil {
			for _, address := range addresses {
				if address.To4() == nil || validateLocalDNSHostIP(address, deps.interfaceAddrs) != nil {
					continue
				}

				return localDNSMetricsEndpoint(address), nil
			}
		}
	}

	hostIP, err := deps.resolveBindAddress(nil)
	if err != nil {
		return "", fmt.Errorf("resolve LocalDNS metrics address from host default route: %w", err)
	}

	if hostIP == nil || hostIP.To4() == nil {
		return "", fmt.Errorf("resolve LocalDNS metrics address: default host address %v is not IPv4", hostIP)
	}

	return localDNSMetricsEndpoint(hostIP), nil
}

func localDNSMetricsEndpoint(ip net.IP) string {
	return net.JoinHostPort(ip.String(), fmt.Sprint(LocalDNSMetricsPort))
}

func validateLocalDNSHostIP(ip net.IP, interfaceAddrs func() ([]net.Addr, error)) error {
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("IP must be IPv4")
	}

	if ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return fmt.Errorf("IP %s is not a usable host address", ip)
	}

	addresses, err := interfaceAddrs()
	if err != nil {
		return fmt.Errorf("list host interface addresses: %w", err)
	}

	for _, address := range addresses {
		var candidate net.IP

		switch value := address.(type) {
		case *net.IPNet:
			candidate = value.IP
		case *net.IPAddr:
			candidate = value.IP
		}

		if candidate != nil && candidate.Equal(ip) {
			return nil
		}
	}

	return fmt.Errorf("IP %s is not assigned to a host interface", ip)
}

func renderLocalDNSCorefile(source string, data LocalDNSCorefileTemplateData) ([]byte, error) {
	tmpl, err := template.New("localdns-corefile").Option("missingkey=error").Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse LocalDNS Corefile template: %w", err)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, fmt.Errorf("render LocalDNS Corefile: %w", err)
	}

	if out.Len() == 0 {
		return nil, fmt.Errorf("rendered LocalDNS Corefile is empty")
	}

	if out.Len() > 512*1024 {
		return nil, fmt.Errorf("rendered LocalDNS Corefile exceeds 512 KiB")
	}

	return out.Bytes(), nil
}

func normalizePluginNames(values []string) []string {
	set := map[string]struct{}{}

	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			set[value] = struct{}{}
		}
	}

	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}

	sort.Strings(out)

	return out
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return strings.TrimSpace(value)
}
