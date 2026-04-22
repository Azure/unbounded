// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	configpkg "github.com/Azure/unbounded-kube/internal/net/config"
	"github.com/Azure/unbounded-kube/internal/net/metrics"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
	"github.com/Azure/unbounded-kube/internal/version"
)

// CNIConfig represents the CNI configuration file structure
type CNIConfig struct {
	CNIVersion string       `json:"cniVersion"`
	Name       string       `json:"name"`
	Plugins    []PluginConf `json:"plugins"`
}

// PluginConf represents a CNI plugin configuration
type PluginConf struct {
	Type         string      `json:"type"`
	Bridge       string      `json:"bridge,omitempty"`
	IsGateway    bool        `json:"isGateway,omitempty"`
	IsDefaultGW  bool        `json:"isDefaultGateway,omitempty"`
	ForceAddress bool        `json:"forceAddress,omitempty"`
	IPMasq       bool        `json:"ipMasq,omitempty"`
	HairpinMode  bool        `json:"hairpinMode,omitempty"`
	MTU          int         `json:"mtu,omitempty"`
	IPAM         *IPAMConfig `json:"ipam,omitempty"`
	Capabilities *Caps       `json:"capabilities,omitempty"`
}

// IPAMConfig represents the IPAM configuration
type IPAMConfig struct {
	Type   string      `json:"type"`
	Ranges [][]IPRange `json:"ranges,omitempty"`
}

// IPRange represents an IP range for IPAM
type IPRange struct {
	Subnet string `json:"subnet"`
}

// Caps represents plugin capabilities
type Caps struct {
	PortMappings bool `json:"portMappings,omitempty"`
}

type config struct {
	ConfigFile                    string
	KubeconfigPath                string
	ApiserverURL                  string // Override Kubernetes API server URL (empty = use default)
	NodeName                      string
	CNIConfDir                    string
	CNIConfFile                   string
	BridgeName                    string
	WireGuardDir                  string
	WireGuardPort                 int
	EnablePolicyRouting           bool
	MTU                           int
	HealthPort                    int
	InformerResyncPeriod          time.Duration
	StatusPushEnabled             bool          // Whether to push status to controller
	StatusPushURL                 string        // Controller URL for status push
	StatusPushInterval            time.Duration // Interval between status pushes to controller
	StatusPushAPIServerInterval   time.Duration // Interval between status pushes via aggregated API server
	StatusPushDelta               bool          // Whether periodic HTTP pushes use deltas
	StatusWSEnabled               bool          // Whether websocket push is enabled
	StatusWSURL                   string        // Controller websocket URL for status push
	StatusWSAPIServerMode         string        // API server websocket mode: never, fallback, preferred
	StatusWSAPIServerURL          string        // API server websocket URL for status push fallback
	StatusWSAPIServerStartupDelay time.Duration // Delay before API server fallback is allowed after startup
	StatusWSKeepaliveInterval     time.Duration // Interval between websocket keepalive pings (0 disables keepalive)
	StatusWSKeepaliveFailureCount int           // Sequential websocket keepalive ping failures before reconnect
	RemoveConfigurationOnShutdown bool          // Remove all managed configuration (WireGuard, routes, masquerade, etc.) on shutdown
	RemoveWireGuardOnShutdown     bool          // Deprecated: use RemoveConfigurationOnShutdown
	CleanupNetlinkOnShutdown      bool          // Deprecated: use RemoveConfigurationOnShutdown
	RemoveMasqueradeOnShutdown    bool          // Deprecated: use RemoveConfigurationOnShutdown
	HealthCheckPort               int           // UDP port for health check probes (default 9997)
	BaseMetric                    int           // Base metric for programmed routes (default 1)
	RouteTableID                  int           // Route table ID for managed routes (default 252)
	CriticalDeltaEvery            time.Duration // Maximum critical delta publish frequency; changes are queued up to this interval for batching
	StatsDeltaEvery               time.Duration // Maximum statistics delta publish frequency; changes are queued up to this interval for batching
	FullSyncEvery                 time.Duration // Forced full status sync interval; ensures controller has complete status periodically
	GenevePort                    int           // GENEVE UDP destination port (default 6081)
	GeneveVNI                     int           // GENEVE Virtual Network Identifier (default 1)
	GeneveInterfaceName           string        // GENEVE interface name (default geneve0)
	VXLANPort                     int           // VXLAN UDP destination port (default 4789)
	VXLANSrcPortLow               int           // VXLAN UDP source port range low (default 47891)
	VXLANSrcPortHigh              int           // VXLAN UDP source port range high (default 47922)
	PreferredPrivateEncap         string        // Preferred encap for private/internal networks (GENEVE, IPIP, VXLAN, WireGuard)
	PreferredPublicEncap          string        // Preferred encap for public/external networks (WireGuard, IPIP, GENEVE, VXLAN)
	HealthFlapMaxBackoff          time.Duration // Maximum backoff duration for health check flap dampening
	KubeProxyHealthInterval       time.Duration // Interval between kube-proxy health checks (0 to disable)
	NetlinkResyncPeriod           time.Duration // Interval between full netlink cache resyncs
	TunnelDataplane               string        // Tunnel dataplane mode: "ebpf" (default) or "netlink"
	TunnelDataplaneMapSize        int           // Maximum LPM trie entries for eBPF tunnel map (default 16384)
	TunnelIPFamily                string        // Tunnel underlay IP family: "IPv4" (default) or "IPv6"
}

var siteGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sites",
}

var siteNodeSliceGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sitenodeslices",
}

var gatewayPoolGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypools",
}

var gatewayNodeGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypoolnodes",
}

var sitePeeringGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sitepeerings",
}

var siteGatewayPoolAssignmentGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sitegatewaypoolassignments",
}

var gatewayPoolPeeringGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypoolpeerings",
}

const (
	// WireGuard public key annotation on the node
	WireGuardPubKeyAnnotation = "net.unbounded-kube.io/wg-pubkey"

	// TunnelMTUAnnotation is the maximum tunnel MTU this node
	// can support, based on its default-route interface MTU minus encapsulation
	// overhead. The controller uses this to validate that the configured MTU
	// does not exceed what any node in the cluster can handle.
	TunnelMTUAnnotation = "net.unbounded-kube.io/tunnel-mtu"

	// Gateway node taint key - prevents regular workloads from running on gateway nodes
	// since they don't have regular pod CIDR routing
	GatewayNodeTaintKey = "net.unbounded-kube.io/gateway-node"

	// gatewayNodeHeartbeatInterval controls how frequently the node agent refreshes
	// GatewayNode.status.lastUpdated. Route staleness is derived from this cadence.
	gatewayNodeHeartbeatInterval = 10 * time.Second
)

func main() {
	// Initialize klog flags
	klog.InitFlags(nil)

	// Add klog flags to pflag
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	cfg := &config{
		ConfigFile:                    "/etc/unbounded-net/config.yaml",
		CNIConfDir:                    "/etc/cni/net.d",
		CNIConfFile:                   "10-unbounded.conflist",
		BridgeName:                    "cbr0",
		WireGuardDir:                  "/etc/wireguard",
		WireGuardPort:                 51820,
		EnablePolicyRouting:           false,
		MTU:                           1280, // default WireGuard MTU (IPv6 minimum)
		HealthPort:                    9998,
		InformerResyncPeriod:          600 * time.Second,
		StatusPushEnabled:             true,             // Enabled by default
		StatusPushInterval:            10 * time.Second, // Default 10s push interval
		StatusPushAPIServerInterval:   30 * time.Second,
		StatusPushDelta:               true,
		StatusWSEnabled:               true,
		StatusWSAPIServerMode:         statusWSAPIServerModeFallback,
		StatusWSAPIServerStartupDelay: 60 * time.Second,
		StatusWSKeepaliveInterval:     10 * time.Second,
		StatusWSKeepaliveFailureCount: 2,
		CriticalDeltaEvery:            1 * time.Second,
		StatsDeltaEvery:               15 * time.Second,
		FullSyncEvery:                 2 * time.Minute,
		GenevePort:                    6081,
		GeneveVNI:                     1,
		GeneveInterfaceName:           "geneve0",
		VXLANPort:                     4789,
		VXLANSrcPortLow:               47891,
		VXLANSrcPortHigh:              47922,
		PreferredPrivateEncap:         "GENEVE",
		PreferredPublicEncap:          "WireGuard",
		NetlinkResyncPeriod:           300 * time.Second,
		TunnelDataplane:               "ebpf",
		TunnelDataplaneMapSize:        16384,
		TunnelIPFamily:                "IPv4",
	}

	rootCmd := &cobra.Command{
		Use:   "unbounded-net-node",
		Short: "CNI configuration agent for unbounded-net",
		Long: `unbounded-net-node runs on each node as a DaemonSet and configures CNI
networking by writing a CNI configuration file based on the node's podCIDRs.

It watches the node object in Kubernetes and waits for podCIDRs to be assigned
by the unbounded-net-controller. Once assigned, it writes a CNI configuration
file that sets up pod networking using the bridge plugin with host-local IPAM.

It also generates WireGuard keys for the node and stores them in /etc/wireguard,
then annotates the node with the public key.`,
		Version:      version.Version + " (commit: " + version.GitCommit + ")",
		SilenceUsage: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return applyNodeRuntimeConfig(cmd, cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cfg)
		},
	}

	// Add flags
	flags := rootCmd.Flags()

	// Change version flag from -v to -V to avoid conflict with klog's -v flag
	rootCmd.Flags().BoolP("version", "V", false, "Print version information")
	rootCmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	// General flags
	flags.StringVar(&cfg.ConfigFile, "config-file", "/etc/unbounded-net/config.yaml", "Path to runtime YAML config file")
	flags.StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "Path to kubeconfig file (uses in-cluster config if not specified)")
	flags.StringVar(&cfg.ApiserverURL, "apiserver-url", "", "Override Kubernetes API server URL (empty = use default from kubeconfig or in-cluster config)")
	flags.StringVar(&cfg.NodeName, "node-name", os.Getenv("NODE_NAME"), "Name of this node (defaults to NODE_NAME env var)")
	flags.IntVar(&cfg.HealthPort, "health-port", 9998, "Port for health check HTTP server (0 to disable)")
	flags.DurationVar(&cfg.InformerResyncPeriod, "informer-resync-period", 600*time.Second, "Resync period for Kubernetes informers")

	// CNI configuration flags
	flags.StringVar(&cfg.CNIConfDir, "cni-conf-dir", "/etc/cni/net.d", "Directory to write CNI configuration")
	flags.StringVar(&cfg.CNIConfFile, "cni-conf-file", "10-unbounded.conflist", "Name of the CNI configuration file")
	flags.StringVar(&cfg.BridgeName, "bridge-name", "cbr0", "Name of the bridge interface")
	flags.IntVar(&cfg.MTU, "mtu", 1280, "MTU for WireGuard and bridge interfaces (default 1280, the IPv6 minimum)")

	// WireGuard configuration flags
	flags.StringVar(&cfg.WireGuardDir, "wireguard-dir", "/etc/wireguard", "Directory to store WireGuard keys")
	flags.IntVar(&cfg.WireGuardPort, "wireguard-port", 51820, "WireGuard listen port")
	flags.BoolVar(&cfg.EnablePolicyRouting, "enable-policy-routing", false, "Enable policy-based routing on gateway interfaces (deprecated, per-interface FORWARD rules replace PBR)")

	// GENEVE configuration flags
	flags.IntVar(&cfg.GenevePort, "geneve-port", 6081, "GENEVE UDP destination port")
	flags.IntVar(&cfg.GeneveVNI, "geneve-vni", 1, "GENEVE Virtual Network Identifier")
	flags.StringVar(&cfg.GeneveInterfaceName, "geneve-interface", "geneve0", "GENEVE interface name (empty = disabled)")
	flags.IntVar(&cfg.VXLANPort, "vxlan-port", 4789, "VXLAN UDP destination port")
	flags.IntVar(&cfg.VXLANSrcPortLow, "vxlan-src-port-low", 47891, "VXLAN UDP source port range low (narrow range reduces VM flow count in cloud platforms)")
	flags.IntVar(&cfg.VXLANSrcPortHigh, "vxlan-src-port-high", 47922, "VXLAN UDP source port range high (narrow range reduces VM flow count in cloud platforms)")
	flags.StringVar(&cfg.PreferredPrivateEncap, "preferred-private-encap", "GENEVE", "Preferred encapsulation for private networks (GENEVE, IPIP, VXLAN, WireGuard)")
	flags.StringVar(&cfg.PreferredPublicEncap, "preferred-public-encap", "WireGuard", "Preferred encapsulation for public networks (WireGuard, IPIP, GENEVE, VXLAN)")

	// Status push flags
	flags.BoolVar(&cfg.StatusPushEnabled, "status-push-enabled", true, "Enable pushing node status to controller")
	flags.StringVar(&cfg.StatusPushURL, "status-push-url", "", "Controller URL for status push (default: use UNBOUNDED_NET_CONTROLLER_SERVICE_HOST/PORT)")
	flags.DurationVar(&cfg.StatusPushInterval, "status-push-interval", 60*time.Second, "Interval between status pushes to controller")
	flags.DurationVar(&cfg.StatusPushAPIServerInterval, "status-push-apiserver-interval", 60*time.Second, "Interval between status pushes via aggregated API server")
	flags.BoolVar(&cfg.StatusPushDelta, "status-push-delta", true, "Enable delta mode for periodic HTTP status push")
	flags.BoolVar(&cfg.StatusWSEnabled, "status-ws-enabled", true, "Enable websocket status push to controller")
	flags.StringVar(&cfg.StatusWSURL, "status-ws-url", "", "Controller websocket URL for status push (default: ws://service/status/nodews)")
	flags.StringVar(&cfg.StatusWSAPIServerMode, "status-ws-apiserver-mode", statusWSAPIServerModeFallback, "API server websocket mode: never, fallback, preferred")
	flags.StringVar(&cfg.StatusWSAPIServerURL, "status-ws-apiserver-url", "", "API server websocket URL for status push fallback (default: wss://$(KUBERNETES_SERVICE_HOST)/apis/status.net.unbounded-kube.io/v1alpha1/status/nodews)")
	flags.DurationVar(&cfg.StatusWSAPIServerStartupDelay, "status-ws-apiserver-startup-delay", 60*time.Second, "Delay before API server websocket/push fallback is allowed after startup (0 to disable delay)")
	flags.DurationVar(&cfg.StatusWSKeepaliveInterval, "status-ws-keepalive-interval", 10*time.Second, "Interval between websocket keepalive pings (0 to disable)")
	flags.IntVar(&cfg.StatusWSKeepaliveFailureCount, "status-ws-keepalive-failure-count", 2, "Sequential websocket keepalive ping failures before reconnect")
	flags.BoolVar(&cfg.RemoveConfigurationOnShutdown, "remove-configuration-on-shutdown", false, "Remove all managed configuration (WireGuard, routes, masquerade, tunnel interfaces) on shutdown")
	flags.BoolVar(&cfg.RemoveWireGuardOnShutdown, "shutdown-remove-wireguard-configuration", false, "Remove WireGuard interfaces/configuration on shutdown (deprecated: use --remove-configuration-on-shutdown)")
	flags.BoolVar(&cfg.CleanupNetlinkOnShutdown, "shutdown-cleanup-netlink", false, "Remove managed netlink routes/policy rules on shutdown (deprecated: use --remove-configuration-on-shutdown)")
	flags.BoolVar(&cfg.RemoveMasqueradeOnShutdown, "shutdown-remove-masquerade-rules", false, "Remove managed masquerade rules on shutdown (deprecated: use --remove-configuration-on-shutdown)")
	flags.IntVar(&cfg.HealthCheckPort, "healthcheck-port", 9997, "UDP port for health check probes")
	flags.IntVar(&cfg.BaseMetric, "base-metric", 1, "Base metric for programmed routes")
	flags.IntVar(&cfg.RouteTableID, "route-table-id", 252, "Route table ID for managed routes (default 252, set to 254 for main table)")
	flags.DurationVar(&cfg.CriticalDeltaEvery, "status-critical-interval", 15*time.Second, "Maximum critical delta publish frequency; changed fields are queued up to this interval for batching")
	flags.DurationVar(&cfg.StatsDeltaEvery, "status-stats-interval", 60*time.Second, "Maximum statistics delta publish frequency; changed fields are queued up to this interval for batching")
	flags.DurationVar(&cfg.FullSyncEvery, "status-full-sync-interval", 2*time.Minute, "Forced full status sync interval; ensures controller has complete status periodically")
	flags.DurationVar(&cfg.HealthFlapMaxBackoff, "health-flap-max-backoff", 120*time.Second, "Maximum backoff duration for health check flap dampening")
	flags.DurationVar(&cfg.KubeProxyHealthInterval, "kube-proxy-health-interval", 30*time.Second, "Interval between kube-proxy health checks (0 to disable)")
	flags.DurationVar(&cfg.NetlinkResyncPeriod, "netlink-resync-period", 300*time.Second, "Interval between full netlink cache resyncs")
	flags.StringVar(&cfg.TunnelDataplane, "tunnel-dataplane", "ebpf", "Tunnel dataplane mode: ebpf (default) or netlink")
	flags.IntVar(&cfg.TunnelDataplaneMapSize, "tunnel-dataplane-map-size", 16384, "Maximum LPM trie entries for eBPF tunnel map")
	flags.StringVar(&cfg.TunnelIPFamily, "tunnel-ip-family", "IPv4", "Tunnel underlay IP family: IPv4 (default) or IPv6")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func applyNodeRuntimeConfig(cmd *cobra.Command, cfg *config) error {
	runtimeCfg, err := configpkg.LoadRuntimeConfig(cfg.ConfigFile)
	if err != nil {
		return err
	}

	flags := cmd.Flags()
	nodeCfg := runtimeCfg.Node

	if !flags.Changed("informer-resync-period") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.InformerResyncPeriod, "node.informerResyncPeriod"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.InformerResyncPeriod = d
		}
	}

	if !flags.Changed("node-name") && nodeCfg.NodeName != "" {
		cfg.NodeName = nodeCfg.NodeName
	}

	if !flags.Changed("cni-conf-dir") && nodeCfg.CNIConfDir != "" {
		cfg.CNIConfDir = nodeCfg.CNIConfDir
	}

	if !flags.Changed("cni-conf-file") && nodeCfg.CNIConfFile != "" {
		cfg.CNIConfFile = nodeCfg.CNIConfFile
	}

	if !flags.Changed("bridge-name") && nodeCfg.BridgeName != "" {
		cfg.BridgeName = nodeCfg.BridgeName
	}

	if !flags.Changed("wireguard-dir") && nodeCfg.WireGuardDir != "" {
		cfg.WireGuardDir = nodeCfg.WireGuardDir
	}

	if !flags.Changed("wireguard-port") && nodeCfg.WireGuardPort != nil {
		cfg.WireGuardPort = *nodeCfg.WireGuardPort
	}

	if !flags.Changed("enable-policy-routing") && nodeCfg.EnablePolicyRouting != nil { //nolint:staticcheck // intentional use of deprecated field for backward compat
		cfg.EnablePolicyRouting = *nodeCfg.EnablePolicyRouting //nolint:staticcheck // intentional use of deprecated field
	}

	if !flags.Changed("mtu") && nodeCfg.MTU != nil {
		cfg.MTU = *nodeCfg.MTU
	}

	if !flags.Changed("health-port") && nodeCfg.HealthPort != nil {
		cfg.HealthPort = *nodeCfg.HealthPort
	}

	if !flags.Changed("status-push-enabled") && nodeCfg.StatusPushEnabled != nil {
		cfg.StatusPushEnabled = *nodeCfg.StatusPushEnabled
	}

	if !flags.Changed("status-push-url") && nodeCfg.StatusPushURL != "" {
		cfg.StatusPushURL = nodeCfg.StatusPushURL
	}

	if !flags.Changed("status-push-interval") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.StatusPushInterval, "node.statusPushInterval"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.StatusPushInterval = d
		}
	}

	if !flags.Changed("status-push-apiserver-interval") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.StatusPushAPIServerInterval, "node.statusPushApiserverInterval"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.StatusPushAPIServerInterval = d
		}
	}

	if !flags.Changed("status-push-delta") && nodeCfg.StatusPushDelta != nil {
		cfg.StatusPushDelta = *nodeCfg.StatusPushDelta
	}

	if !flags.Changed("status-ws-enabled") && nodeCfg.StatusWSEnabled != nil {
		cfg.StatusWSEnabled = *nodeCfg.StatusWSEnabled
	}

	if !flags.Changed("status-ws-url") && nodeCfg.StatusWSURL != "" {
		cfg.StatusWSURL = nodeCfg.StatusWSURL
	}

	if !flags.Changed("status-ws-apiserver-mode") && nodeCfg.StatusWSAPIServerMode != "" {
		cfg.StatusWSAPIServerMode = nodeCfg.StatusWSAPIServerMode
	}

	if !flags.Changed("status-ws-apiserver-url") && nodeCfg.StatusWSAPIServerURL != "" {
		cfg.StatusWSAPIServerURL = nodeCfg.StatusWSAPIServerURL
	}

	if !flags.Changed("status-ws-apiserver-startup-delay") && nodeCfg.StatusWSAPIServerStartupDelay != "" {
		d, parseErr := configpkg.ParseDurationField(nodeCfg.StatusWSAPIServerStartupDelay, "node.statusWebsocketApiserverStartupDelay")
		if parseErr != nil {
			return parseErr
		}

		if d < 0 {
			return fmt.Errorf("node.statusWebsocketApiserverStartupDelay must be >= 0")
		}

		cfg.StatusWSAPIServerStartupDelay = d
	}

	if !flags.Changed("status-ws-keepalive-interval") && nodeCfg.StatusWSKeepaliveInterval != "" {
		d, parseErr := configpkg.ParseDurationField(nodeCfg.StatusWSKeepaliveInterval, "node.statusWebsocketKeepaliveInterval")
		if parseErr != nil {
			return parseErr
		}

		cfg.StatusWSKeepaliveInterval = d
	}

	if !flags.Changed("status-ws-keepalive-failure-count") && nodeCfg.StatusWSKeepaliveFailCount != nil {
		cfg.StatusWSKeepaliveFailureCount = *nodeCfg.StatusWSKeepaliveFailCount
	}
	// New consolidated shutdown cleanup flag.
	if !flags.Changed("remove-configuration-on-shutdown") && nodeCfg.RemoveConfigurationOnShutdown != nil {
		cfg.RemoveConfigurationOnShutdown = *nodeCfg.RemoveConfigurationOnShutdown
	}
	// Deprecated individual shutdown cleanup flags (kept for backward compatibility).
	if !flags.Changed("shutdown-remove-wireguard-configuration") && nodeCfg.ShutdownRemoveWireGuardConfiguration != nil {
		cfg.RemoveWireGuardOnShutdown = *nodeCfg.ShutdownRemoveWireGuardConfiguration
	}

	if !flags.Changed("shutdown-cleanup-netlink") && nodeCfg.ShutdownRemoveIPRoutes != nil {
		cfg.CleanupNetlinkOnShutdown = *nodeCfg.ShutdownRemoveIPRoutes
	}

	if !flags.Changed("shutdown-remove-masquerade-rules") && nodeCfg.ShutdownRemoveMasqueradeRules != nil {
		cfg.RemoveMasqueradeOnShutdown = *nodeCfg.ShutdownRemoveMasqueradeRules
	}
	// If any deprecated flag is true, activate the consolidated flag.
	if cfg.RemoveWireGuardOnShutdown || cfg.CleanupNetlinkOnShutdown || cfg.RemoveMasqueradeOnShutdown {
		cfg.RemoveConfigurationOnShutdown = true
	}

	if _, err := parseStatusWSAPIServerMode(cfg.StatusWSAPIServerMode); err != nil {
		return err
	}

	if !flags.Changed("status-critical-interval") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.CriticalDeltaEvery, "node.criticalDeltaEvery"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.CriticalDeltaEvery = d
		}
	}

	if !flags.Changed("status-stats-interval") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.StatsDeltaEvery, "node.statsDeltaEvery"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.StatsDeltaEvery = d
		}
	}

	if !flags.Changed("status-full-sync-interval") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.FullSyncEvery, "node.fullSyncEvery"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.FullSyncEvery = d
		}
	}

	if cfg.StatusWSKeepaliveFailureCount < 1 {
		return fmt.Errorf("node.statusWsKeepaliveFailureCount must be >= 1")
	}

	// Apply preferred encapsulation from config file if not set via CLI.
	if !flags.Changed("preferred-private-encap") && nodeCfg.PreferredPrivateNetworkEncapsulation != "" {
		cfg.PreferredPrivateEncap = nodeCfg.PreferredPrivateNetworkEncapsulation
	}

	if !flags.Changed("preferred-public-encap") && nodeCfg.PreferredPublicNetworkEncapsulation != "" {
		cfg.PreferredPublicEncap = nodeCfg.PreferredPublicNetworkEncapsulation
	}

	if !flags.Changed("health-flap-max-backoff") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.HealthFlapMaxBackoff, "node.healthFlapMaxBackoff"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.HealthFlapMaxBackoff = d
		}
	}

	if !flags.Changed("route-table-id") && nodeCfg.RouteTableID != nil {
		cfg.RouteTableID = *nodeCfg.RouteTableID
	}

	if !flags.Changed("kube-proxy-health-interval") && nodeCfg.KubeProxyHealthInterval != "" {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.KubeProxyHealthInterval, "node.kubeProxyHealthInterval"); parseErr != nil {
			return parseErr
		} else {
			cfg.KubeProxyHealthInterval = d
		}
	}

	if !flags.Changed("netlink-resync-period") {
		if d, parseErr := configpkg.ParseDurationField(nodeCfg.NetlinkResyncPeriod, "node.netlinkResyncPeriod"); parseErr != nil {
			return parseErr
		} else if d > 0 {
			cfg.NetlinkResyncPeriod = d
		}
	}

	if !flags.Changed("tunnel-dataplane") && nodeCfg.TunnelDataplane != "" {
		cfg.TunnelDataplane = nodeCfg.TunnelDataplane
	}

	if !flags.Changed("tunnel-dataplane-map-size") && nodeCfg.TunnelDataplaneMapSize != nil {
		cfg.TunnelDataplaneMapSize = *nodeCfg.TunnelDataplaneMapSize
	}

	if !flags.Changed("tunnel-ip-family") && nodeCfg.TunnelIPFamily != "" {
		cfg.TunnelIPFamily = nodeCfg.TunnelIPFamily
	}

	if !flags.Changed("vxlan-src-port-low") && nodeCfg.VXLANSrcPortLow != nil {
		cfg.VXLANSrcPortLow = *nodeCfg.VXLANSrcPortLow
	}

	if !flags.Changed("vxlan-src-port-high") && nodeCfg.VXLANSrcPortHigh != nil {
		cfg.VXLANSrcPortHigh = *nodeCfg.VXLANSrcPortHigh
	}

	// Validate and default tunnelDataplane
	if cfg.TunnelDataplane == "" {
		cfg.TunnelDataplane = "ebpf"
	}

	switch cfg.TunnelDataplane {
	case "ebpf", "netlink":
		// valid
	default:
		return fmt.Errorf("invalid tunnel-dataplane %q: must be 'ebpf' or 'netlink'", cfg.TunnelDataplane)
	}

	// Validate and default tunnelIPFamily
	switch cfg.TunnelIPFamily {
	case "IPv4", "IPv6":
		// valid
	case "":
		cfg.TunnelIPFamily = "IPv4"
	default:
		return fmt.Errorf("invalid tunnel-ip-family %q: must be 'IPv4' or 'IPv6'", cfg.TunnelIPFamily)
	}

	// Normalize MTU: treat 0 as 1280 (the IPv6 minimum, safe for all links).
	if cfg.MTU == 0 {
		cfg.MTU = 1280
	}

	// Apply common config.
	if !flags.Changed("apiserver-url") && runtimeCfg.Common.ApiserverURL != "" {
		cfg.ApiserverURL = runtimeCfg.Common.ApiserverURL
	}

	return nil
}

func run(cfg *config) error {
	klog.Infof("unbounded-net-node version=%s commit=%s built=%s", version.Version, version.GitCommit, version.BuildTime)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		klog.Info("Received shutdown signal")
		cancel()
	}()

	// Validate node name
	if cfg.NodeName == "" {
		klog.Fatal("Node name is required. Set NODE_NAME environment variable or use --node-name flag")
	}

	klog.Infof("Running on node: %s", cfg.NodeName)

	if cfg.EnablePolicyRouting {
		klog.Info("Policy-based routing on gateway interfaces is enabled")
	} else {
		klog.Info("Policy-based routing on gateway interfaces is disabled")
	}

	// Build Kubernetes client
	var (
		restConfig *rest.Config
		err        error
	)

	if cfg.KubeconfigPath != "" {
		restConfig, err = clientcmd.BuildConfigFromFlags(cfg.ApiserverURL, cfg.KubeconfigPath)
	} else {
		restConfig, err = rest.InClusterConfig()
		if err == nil && cfg.ApiserverURL != "" {
			restConfig.Host = cfg.ApiserverURL
		}
	}

	if err != nil {
		klog.Fatalf("Failed to build kubeconfig: %v", err)
	}

	if cfg.ApiserverURL != "" {
		klog.Infof("Using API server URL override: %s", cfg.ApiserverURL)
	}

	// Wire client-go metrics into Prometheus before creating clients.
	metrics.RegisterClientGoMetrics()

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Failed to create dynamic Kubernetes client: %v", err)
	}

	// Generate WireGuard keys and annotate node
	pubKey, err := ensureWireGuardKeys(cfg)
	if err != nil {
		klog.Fatalf("Failed to ensure WireGuard keys: %v", err)
	}

	klog.Infof("WireGuard public key: %s", pubKey)

	if err := annotateNodeWithPubKey(ctx, clientset, cfg.NodeName, pubKey); err != nil {
		klog.Fatalf("Failed to annotate node with WireGuard public key: %v", err)
	}

	klog.Info("Node annotated with WireGuard public key")

	// Detect and annotate the node's maximum tunnel MTU so the controller
	// can validate that the configured MTU is compatible across all nodes.
	if detectedMTU := unboundednetnetlink.DetectDefaultRouteMTU(); detectedMTU > 0 {
		wgMTU := detectedMTU - unboundednetnetlink.WireGuardMTUOverhead
		if err := annotateNodeWithMTU(ctx, clientset, cfg.NodeName, wgMTU); err != nil {
			klog.Warningf("Failed to annotate node with tunnel MTU: %v", err)
		} else {
			klog.Infof("Node annotated with tunnel MTU %d (detected default route MTU %d - %d overhead)", wgMTU, detectedMTU, unboundednetnetlink.WireGuardMTUOverhead)
		}
	}

	// Watch the config file for dynamic log level changes.
	go configpkg.WatchConfigLogLevel(ctx, cfg.ConfigFile)

	// Warn if public network traffic will be sent unencrypted.
	if cfg.PreferredPublicEncap != "" && cfg.PreferredPublicEncap != "WireGuard" {
		klog.Warningf("WARNING: preferredPublicNetworkEncapsulation is set to %q -- traffic over public networks will be sent UNENCRYPTED", cfg.PreferredPublicEncap)
	}

	// Create informers early - before any CRD-based lookups
	// This allows us to use the informer cache for all CRD operations
	informerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, cfg.InformerResyncPeriod)
	sliceInformer := informerFactory.ForResource(siteNodeSliceGVR).Informer()
	siteInformer := informerFactory.ForResource(siteGVR).Informer()
	gatewayPoolInformer := informerFactory.ForResource(gatewayPoolGVR).Informer()
	gatewayNodeInformer := informerFactory.ForResource(gatewayNodeGVR).Informer()
	sitePeeringInformer := informerFactory.ForResource(sitePeeringGVR).Informer()
	assignmentInformer := informerFactory.ForResource(siteGatewayPoolAssignmentGVR).Informer()
	poolPeeringInformer := informerFactory.ForResource(gatewayPoolPeeringGVR).Informer()

	// Start the informers
	informerFactory.Start(ctx.Done())

	// Wait for caches to sync
	klog.Info("Waiting for informer caches to sync")

	if !cache.WaitForCacheSync(ctx.Done(), sliceInformer.HasSynced, siteInformer.HasSynced, gatewayPoolInformer.HasSynced, gatewayNodeInformer.HasSynced, sitePeeringInformer.HasSynced, assignmentInformer.HasSynced, poolPeeringInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}

	klog.Info("Informer caches synced")

	// Start the netlink cache (read-only network state snapshot).
	netlinkCache := unboundednetnetlink.NewNetlinkCache(cfg.NetlinkResyncPeriod)
	if err := netlinkCache.Start(ctx); err != nil {
		klog.Fatalf("Failed to start netlink cache: %v", err)
	}

	// Track if CNI is configured for health checks
	cniConfigured := false

	// Create shared health state for health server
	healthState := &nodeHealthState{
		cniConfigured: &cniConfigured,
		informersSynced: []cache.InformerSynced{
			sliceInformer.HasSynced,
			siteInformer.HasSynced,
			gatewayPoolInformer.HasSynced,
			gatewayNodeInformer.HasSynced,
			sitePeeringInformer.HasSynced,
			assignmentInformer.HasSynced,
			poolPeeringInformer.HasSynced,
		},
	}

	// Start health server if enabled (readiness should not wait on site membership)
	if cfg.HealthPort > 0 {
		go startHealthServer(cfg.HealthPort, healthState)
	}

	// Check if this node is a gateway node by checking the informer cache
	isGatewayNode := isGatewayNodeFromCRDs(gatewayPoolInformer, pubKey)
	if isGatewayNode {
		klog.Info("Node is a gateway node (found in GatewayPool status)")
	}

	// Wait for this node to appear in a SiteNodeSlice or GatewayPool
	// This ensures the site controller has processed this node before we continue
	mySiteName, err := waitForSiteMembership(ctx, sliceInformer, gatewayPoolInformer, pubKey)
	if err != nil {
		if err == context.Canceled {
			return nil
		}

		return err
	}

	// Check if this node's site has manageCniPlugin enabled using the informer cache
	manageCniPlugin := getManageCniPluginFromCRDs(siteInformer, mySiteName)

	var nodePodCIDRs []string
	if manageCniPlugin {
		// Wait for podCIDRs and configure CNI
		nodePodCIDRs, err = waitForPodCIDRsAndConfigure(ctx, clientset, cfg, &cniConfigured)
		if err != nil {
			if err == context.Canceled {
				return nil
			}

			return err
		}
	} else {
		klog.Info("manageCniPlugin is false for this site - skipping CNI configuration")
		// Still need to get the node's podCIDRs for WireGuard gateway IP calculation
		node, err := clientset.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{})
		if err != nil {
			klog.Fatalf("Failed to get node: %v", err)
		}

		nodePodCIDRs = node.Spec.PodCIDRs
		// Mark CNI as "configured" since we're intentionally not managing it
		cniConfigured = true
	}

	// After CNI is configured (or skipped), watch Site CRD for WireGuard peers
	// Pass the node's podCIDRs so routes can be configured with preferred source IPs
	// Also pass the informers so they can be reused (already synced)
	return watchSiteAndConfigureWireGuard(ctx, clientset, dynamicClient, cfg, pubKey, nodePodCIDRs, manageCniPlugin, healthState, netlinkCache, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer)
}
