// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/api/net/v1alpha1"
	ebpfpkg "github.com/Azure/unbounded-kube/internal/net/ebpf"
	"github.com/Azure/unbounded-kube/internal/net/healthcheck"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// wireGuardState tracks the current WireGuard configuration state
type wireGuardState struct {
	peers            []meshPeerInfo    // All mesh peers (intra-site, remote, same-pool gateways)
	gatewayPeers     []gatewayPeerInfo // Peers from gateway pools for dedicated gateway interfaces
	sitePodCIDRs     []string
	sitePodCIDRPools []string // Pod CIDR pool supernets from the local site's PodCidrAssignments
	nodePodCIDRs     []string // This node's own podCIDRs (for cbr0 gateway IPs)
	nodeInternalIPs  []string // This node's internal IPs (cached from node object)
	nodeExternalIPs  []string // This node's external IPs (cached from node object)
	siteName         string   // Site name this node belongs to
	manageCniPlugin  bool     // Whether this node's site manages CNI plugin and full podCIDR mesh routes
	mu               sync.Mutex

	// Kubernetes client for tainting nodes
	clientset kubernetes.Interface
	nodeName  string

	// Netlink managers for the main intra-site WireGuard mesh interface
	linkManager      *unboundednetnetlink.LinkManager
	wireguardManager *unboundednetnetlink.WireGuardManager

	// Unified route manager -- all routes are programmed via netlink.
	// Healthcheck handles health-based nexthop removal/restoration; the agent
	// adds/removes routes when nodes join or leave the mesh.
	routeManager *unboundednetnetlink.UnifiedRouteManager

	// routeTableID is the dedicated routing table used for managed routes.
	routeTableID int

	// reconcileCount tracks how many reconciliations have completed.
	// Used to avoid tearing down tunnels on the first reconcile before
	// informers have synced.
	reconcileCount int

	// Healthcheck manager -- UDP-based health monitoring for peers.
	healthCheckManager *healthcheck.Manager

	// statusTransportWg tracks WS and HTTP push goroutines so the shutdown
	// path can wait for graceful close before tearing down tunnel interfaces.
	statusTransportWg *sync.WaitGroup

	// Gateway interface managers - one per gateway peer
	// Key is the interface name (wg<port>, e.g. wg51822)
	gatewayLinkManagers      map[string]*unboundednetnetlink.LinkManager
	gatewayWireguardManagers map[string]*unboundednetnetlink.WireGuardManager
	gatewayPolicyManager     *unboundednetnetlink.GatewayPolicyManager //nolint:staticcheck // intentional use of deprecated type for backward compat

	// Gateway metadata for status
	gatewayHealthEndpoints         map[string]string                          // interface name -> overlay IP (used for healthcheck matching)
	gatewayRoutes                  map[string][]string                        // interface name -> all routes
	gatewayRouteDistances          map[string]map[string]int                  // interface name -> destination CIDR -> distance
	gatewaySiteCIDRs               map[string][]string                        // interface name -> site CIDRs
	gatewayPodCIDRs                map[string][]string                        // interface name -> pod CIDRs
	meshPeerHealthCheckEnabled     map[string]bool                            // peer public key -> whether healthcheck is enabled for this peer
	gatewayPeerHealthCheckEnabled  map[string]bool                            // interface name -> whether healthcheck is enabled for this gateway peer
	healthCheckProfiles            map[string]healthcheck.HealthCheckSettings // Last applied healthcheck settings by profile name
	siteHealthCheckProfileNames    map[string]string                          // Last resolved site name -> healthcheck profile name
	peeringSiteHealthCheckProfiles map[string]string                          // Last resolved peered site name -> healthcheck profile name
	assignmentSiteHealthCheckNames map[string]string                          // Last resolved assignment site name -> healthcheck profile name
	assignmentPoolHealthCheckNames map[string]string                          // Last resolved assignment pool name -> healthcheck profile name
	poolHealthCheckProfileNames    map[string]string                          // Last resolved gateway pool name -> healthcheck profile name
	siteTunnelMTUs                 map[string]int                             // Last resolved site name -> tunnelMTU override
	peeringSiteTunnelMTUs          map[string]int                             // Last resolved peered site name -> tunnelMTU override
	assignmentSiteTunnelMTUs       map[string]int                             // Last resolved assignment site name -> tunnelMTU override
	assignmentPoolTunnelMTUs       map[string]int                             // Last resolved assignment site|pool key -> tunnelMTU override
	poolTunnelMTUs                 map[string]int                             // Last resolved gateway pool name -> tunnelMTU override
	siteTunnelProtocols            map[string]string                          // Last resolved site name -> tunnelProtocol override
	peeringSiteTunnelProtocols     map[string]string                          // Last resolved peered site name -> tunnelProtocol override
	assignmentSiteTunnelProtocols  map[string]string                          // Last resolved assignment site name -> tunnelProtocol override
	assignmentPoolTunnelProtocols  map[string]string                          // Last resolved assignment site|pool key -> tunnelProtocol override
	poolTunnelProtocols            map[string]string                          // Last resolved gateway pool name -> tunnelProtocol override
	gatewayCIDRs                   []string                                   // shared CIDRs routed through all gateways
	gatewayNames                   map[string]string                          // interface name -> gateway node name
	gatewaySiteNames               map[string]string                          // interface name -> gateway site name

	// Gateway node state
	isGatewayNode       bool      // Whether this node is a gateway node
	myGatewayPort       int32     // This node's assigned gateway WireGuard port (0 if not a gateway)
	localGatewayPools   []string  // Gateway pools this node belongs to
	gatewayTaintApplied sync.Once // Ensures gateway taint is applied only once

	// Masquerade manager for iptables rules
	masqueradeManager *unboundednetnetlink.MasqueradeManager

	// MSS clamp manager for TCP MSS clamping on WireGuard interfaces
	mssClampManager *unboundednetnetlink.MSSClampManager

	// GENEVE tunnel managers -- per-peer interfaces with fixed Remote IP
	geneveInterfaces map[string]*unboundednetnetlink.LinkManager // per-peer GENEVE interface managers keyed by iface name

	// eBPF tunnel maps -- when tunnelDataplane=ebpf, manages the LPM tries
	// on flow-based tunnel interfaces. Keyed by interface name (e.g. geneve0, vxlan0).
	tunnelMaps map[string]*ebpfpkg.TunnelMap

	// pendingBPFEntries accumulates BPF map entries across GENEVE, VXLAN,
	// and WG config phases. A single Reconcile is done after all entries
	// are collected to avoid protocol paths overwriting each other.
	pendingBPFEntries map[string]ebpfpkg.TunnelEndpoint

	// allGatewayPeersForSupernets holds the full gateway peer list (all
	// protocols) so collectSupernets can create supernet routes for
	// cross-site CIDRs regardless of the tunnel protocol used.
	allGatewayPeersForSupernets []gatewayPeerInfo

	// Prometheus collector for WireGuard per-peer stats (rx/tx bytes, last handshake)
	wgCollector *unboundednetnetlink.WireGuardCollector

	// healthFlapMaxBackoff is the node-level maximum backoff for health check flap dampening.
	healthFlapMaxBackoff time.Duration

	otherSitesNodeCIDRs []string

	// Node-level errors surfaced in node status for troubleshooting and health aggregation.
	nodeErrors []NodeError

	// netlinkCache provides cached reads of links, addresses, and routes.
	// When non-nil, read-path code uses the cache instead of direct netlink
	// syscalls. Writes still go directly to netlink.
	netlinkCache *unboundednetnetlink.NetlinkCache

	// Link stats monitor for detecting incrementing error/drop counters.
	linkStatsMonitor *linkStatsMonitor

	// kube-proxy health monitor for detecting kube-proxy failures.
	kubeProxyMonitor *kubeProxyMonitor

	// Persistent logging guard for repeated config-problem warnings.
	lastConfigProblemsLogTime      time.Time
	lastConfigProblemsLogSignature string
}

// gatewayPeerInfo contains information about a gateway peer for inter-site routing
type gatewayPeerInfo struct {
	Name                   string
	SiteName               string // Site this gateway belongs to
	PoolName               string // GatewayPool this peer was discovered from
	HealthCheckProfileName string // Optional resolved healthcheck profile for this specific link
	PoolType               string // Gateway pool type (External or Internal)
	WireGuardPublicKey     string
	GatewayWireguardPort   int32    // Assigned WireGuard port for gateway-to-gateway peering
	InternalIPs            []string // For same-site connections
	ExternalIPs            []string // For cross-site connections
	HealthEndpoints        []string // Full health check URLs (e.g., http://10.0.1.1:9998/healthz)
	RoutedCidrs            []string // CIDRs that should be routed through this gateway (includes global podCIDR pools)
	RouteDistances         map[string]int
	LearnedRoutes          map[string]unboundednetv1alpha1.GatewayNodeRoute
	PodCIDRs               []string // Gateway node's podCIDRs for specific routing
	SkipPodCIDRRoutes      bool     // Keep tunnel reachability host routes but skip explicit PodCIDR routes
	TunnelProtocol         string   // Resolved tunnel protocol for this peer (WireGuard or GENEVE)
}

// meshPeerInfo contains information about a peer on the main mesh interface.
// Endpoint and routing decisions are driven by SiteName and peerSites membership:
//   - Peered site (including same site): endpoint = InternalIPs[0], no internal IP routes
//   - Non-peered site: endpoint = "" (peer initiates connection), add internal IP routes
type meshPeerInfo struct {
	Name                   string
	SiteName               string // Site this peer belongs to -- drives endpoint/routing logic
	HealthCheckProfileName string // Optional resolved healthcheck profile for this specific link
	WireGuardPublicKey     string
	InternalIPs            []string
	PodCIDRs               []string
	SkipPodCIDRRoutes      bool   // Keep tunnel reachability host routes but skip explicit PodCIDR routes
	TunnelProtocol         string // Resolved tunnel protocol for this peer (WireGuard or GENEVE)
}

// Status types for /status and /status/json endpoints

// NodeStatusResponse provides test support.
type NodeStatusResponse = statusv1alpha1.NodeStatusResponse

// BuildInfo aliases shared build metadata in node status responses.
type BuildInfo = statusv1alpha1.BuildInfo

// RoutingTableInfo aliases shared routing table details in node status responses.
type RoutingTableInfo = statusv1alpha1.RoutingTableInfo

// RouteEntry aliases shared route entries in node status responses.
type RouteEntry = statusv1alpha1.RouteEntry

// NextHop aliases shared next hop data in node status responses.
type NextHop = statusv1alpha1.NextHop

// NextHopInfo aliases shared next hop metadata in node status responses.
type NextHopInfo = statusv1alpha1.NextHopInfo

// RouteType aliases shared route type values in node status responses.
type RouteType = statusv1alpha1.RouteType

// NodeInfo aliases shared node identity and runtime status details.
type NodeInfo = statusv1alpha1.NodeInfo

// WireGuardStatusInfo aliases shared WireGuard status details.
type WireGuardStatusInfo = statusv1alpha1.WireGuardStatusInfo

// HealthCheckPeerStatus aliases shared health check peer status details.
type HealthCheckPeerStatus = statusv1alpha1.HealthCheckPeerStatus

// HealthCheckStatus aliases shared health check status details.
type HealthCheckStatus = statusv1alpha1.HealthCheckStatus

// NodeError aliases shared node error details.
type NodeError = statusv1alpha1.NodeError

// PeerTunnelStatus aliases shared tunnel link status details.
type PeerTunnelStatus = statusv1alpha1.PeerTunnelStatus

// WireGuardPeerLinkStatus aliases shared WireGuard link status details.
type WireGuardPeerLinkStatus = statusv1alpha1.WireGuardPeerLinkStatus

// WireGuardPeerStatus aliases shared WireGuard peer status details.
type WireGuardPeerStatus = statusv1alpha1.WireGuardPeerStatus

// BpfEntry aliases shared BPF trie entry details.
type BpfEntry = statusv1alpha1.BpfEntry
