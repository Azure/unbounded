// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const (
	CoreDNSVersion             = "1.12.3"
	LocalDNSInterfaceName      = "localdns"
	LocalDNSCorefilePath       = "/etc/unbounded/localdns/Corefile"
	LocalDNSUpstreamsPath      = "/etc/unbounded/localdns/node-upstreams"
	LocalDNSServiceUnit        = "localdns.service"
	LocalDNSSliceUnit          = "localdns.slice"
	LocalDNSReadinessPort      = 8181
	LocalDNSMetricsPort        = 9253
	LocalDNSRuleComment        = "unbounded-localdns: skip conntrack"
	LocalDNSNFTTable           = "unbounded_localdns"
	LocalDNSNetworkBackendPath = ConfigDir + "/localdns-network-backend"
	LocalDNSNetworkUnit        = "unbounded-localdns-network.service"
	LocalDNSSupervisorPath     = "/usr/local/libexec/unbounded-localdns-supervisor"
	LocalDNSCoreDNSBinaryPath  = "/usr/local/bin/coredns"
)

const (
	hostResolvConfPath            = "/etc/resolv.conf"
	systemdResolvedResolvConfPath = "/run/systemd/resolve/resolv.conf"
)

type localDNSResolverDeps struct {
	readFile        func(string) ([]byte, error)
	resolvedDomains func() (string, error)
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

var baselineLocalDNSPlugins = []string{
	"bind", "cache", "errors", "forward", "loop", "prometheus", "ready", "whoami",
}

const defaultLocalDNSCorefileTemplate = `health-check.localdns.local:53 {
    bind {{ .NodeListenerIP }} {{ .ClusterListenerIP }}
    whoami
}

.:53 {
    errors
    bind {{ .NodeListenerIP }}
    forward . {{ .NodeUpstreamIPsJoined }} {
        force_tcp
        policy sequential
        max_concurrent 1000
    }
    ready {{ .NodeListenerIP }}:8181
    cache 30 {
        success 9984
        denial 9984
        serve_stale 3600s verify
        servfail 0
    }
    loop
    prometheus {{ .MetricsAddress }}
}

.:53 {
    errors
    bind {{ .ClusterListenerIP }}
    forward . {{ .ClusterDNSServiceIP }} {
        force_tcp
        policy sequential
        max_concurrent 1000
    }
    ready {{ .ClusterListenerIP }}:8181
    cache 30 {
        success 9984
        denial 9984
        serve_stale 3600s verify
        servfail 0
    }
    loop
}
`

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

func resolveLocalDNS(cfg *config.AgentConfig, downloads *DownloadOverrides) (LocalDNS, error) {
	if cfg.LocalDNS == nil || !cfg.LocalDNS.Enabled {
		return LocalDNS{}, nil
	}

	if err := cfg.LocalDNS.Validate(cfg.Cluster.ClusterDNS, cfg.Kubelet.NodeIP); err != nil {
		return LocalDNS{}, err
	}

	nodeListener := netip.MustParseAddr(valueOrDefault(cfg.LocalDNS.NodeListenerIP, config.DefaultLocalDNSNodeListenerIP))
	clusterListener := netip.MustParseAddr(valueOrDefault(cfg.LocalDNS.ClusterListenerIP, config.DefaultLocalDNSClusterListenerIP))
	clusterDNS := netip.MustParseAddr(strings.TrimSpace(cfg.Cluster.ClusterDNS))

	resolvConf, upstreams, err := discoverLocalDNSUpstreams(defaultLocalDNSResolverDeps(), nodeListener, clusterListener)
	if err != nil {
		return LocalDNS{}, err
	}

	metricsAddress, err := localDNSMetricsAddress(cfg.LocalDNS.MetricsAddress, cfg.Kubelet.NodeIP)
	if err != nil {
		return LocalDNS{}, err
	}

	cpuLimit := config.DefaultLocalDNSCPUMilliCores
	if cfg.LocalDNS.CPULimitInMilliCores != nil {
		cpuLimit = *cfg.LocalDNS.CPULimitInMilliCores
	}

	memoryLimit := config.DefaultLocalDNSMemoryLimitMB
	if cfg.LocalDNS.MemoryLimitInMB != nil {
		memoryLimit = *cfg.LocalDNS.MemoryLimitInMB
	}

	version := CoreDNSVersion
	if downloads != nil && downloads.CoreDNS != nil && downloads.CoreDNS.Version != "" {
		version = strings.TrimPrefix(downloads.CoreDNS.Version, "v")
	}

	plugins := append([]string(nil), baselineLocalDNSPlugins...)
	plugins = append(plugins, cfg.LocalDNS.RequiredPlugins...)
	plugins = normalizePluginNames(plugins)

	upstreamStrings := make([]string, 0, len(upstreams))
	for _, upstream := range upstreams {
		upstreamStrings = append(upstreamStrings, upstream.String())
	}

	templateSource := cfg.LocalDNS.CorefileTemplate
	if templateSource == "" {
		templateSource = defaultLocalDNSCorefileTemplate
	}

	corefile, err := renderLocalDNSCorefile(templateSource, LocalDNSCorefileTemplateData{
		NodeListenerIP:        nodeListener.String(),
		ClusterListenerIP:     clusterListener.String(),
		NodeUpstreamIPs:       upstreamStrings,
		NodeUpstreamIPsJoined: strings.Join(upstreamStrings, " "),
		ClusterDNSServiceIP:   clusterDNS.String(),
		MetricsAddress:        metricsAddress,
	})
	if err != nil {
		return LocalDNS{}, err
	}

	return LocalDNS{
		Enabled:                true,
		CoreDNSVersion:         version,
		NodeListenerIP:         nodeListener,
		ClusterListenerIP:      clusterListener,
		NodeUpstreamIPs:        upstreams,
		ClusterDNSServiceIP:    clusterDNS,
		MetricsAddress:         metricsAddress,
		CPULimitInMilliCores:   cpuLimit,
		MemoryLimitInMB:        memoryLimit,
		RequiredPlugins:        plugins,
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

func localDNSMetricsAddress(configured, nodeIPs string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured), nil
	}

	for _, candidate := range strings.Split(nodeIPs, ",") {
		addr, err := netip.ParseAddr(strings.TrimSpace(candidate))
		if err == nil && addr.Is4() && !addr.IsUnspecified() {
			return net.JoinHostPort(addr.String(), fmt.Sprint(LocalDNSMetricsPort)), nil
		}
	}

	return "", fmt.Errorf("resolve LocalDNS metrics address: no IPv4 Kubelet.NodeIP")
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
