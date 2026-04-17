// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// NodeStatusResponse is the top-level status response for a node.
type NodeStatusResponse struct {
	Timestamp    time.Time          `json:"timestamp"`
	NodeInfo     NodeInfo           `json:"nodeInfo"`
	Peers        []PeerStatus       `json:"peers"`
	RoutingTable RoutingTableInfo   `json:"routingTable"`
	HealthCheck  *HealthCheckStatus `json:"healthCheck,omitempty"`
	NodeErrors   []NodeError        `json:"nodeErrors,omitempty"`
	FetchError   string             `json:"fetchError,omitempty"`
	LastPushTime *time.Time         `json:"lastPushTime,omitempty"`
	StatusSource string             `json:"statusSource,omitempty"`
	NodePodInfo  *NodePodInfo       `json:"nodePodInfo,omitempty"`
	BpfEntries   []BpfEntry         `json:"bpfEntries,omitempty"`
}

// BpfEntry represents a single entry from the eBPF LPM trie tunnel maps.
type BpfEntry struct {
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

// NodeError describes a node-level error surfaced in status payloads.
type NodeError struct {
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

// NodePodInfo contains information about the unbounded-net-node pod running on a node.
type NodePodInfo struct {
	PodName   string    `json:"podName"`
	StartTime time.Time `json:"startTime"`
	Restarts  int32     `json:"restarts"`
}

// BuildInfo contains build-time information for a binary.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

// RoutingTableInfo contains routing table entries.
type RoutingTableInfo struct {
	Routes            []RouteEntry `json:"routes"`
	ManagedRouteCount int          `json:"managedRouteCount,omitempty"`
	PendingRouteCount int          `json:"pendingRouteCount,omitempty"`
}

// RouteEntry represents a deduplicated route.
type RouteEntry struct {
	Destination string    `json:"destination"`
	Family      string    `json:"family"` // "IPv4" or "IPv6"
	Table       int       `json:"table,omitempty"`
	NextHops    []NextHop `json:"nextHops"`
}

// NextHop represents a single next-hop in a route.
type NextHop struct {
	Gateway          string       `json:"gateway,omitempty"`
	Device           string       `json:"device"`
	Distance         int          `json:"distance,omitempty"`
	Weight           int          `json:"weight,omitempty"`
	MTU              int          `json:"mtu,omitempty"`
	RouteTypes       []RouteType  `json:"routeTypes"`
	Expected         *bool        `json:"expected,omitempty"`
	Present          *bool        `json:"present,omitempty"`
	PeerDestinations []string     `json:"peerDestinations,omitempty"`
	Info             *NextHopInfo `json:"info,omitempty"`
}

// NextHopInfo contains metadata describing how a next-hop was derived.
type NextHopInfo struct {
	ObjectName string `json:"objectName,omitempty"`
	ObjectType string `json:"objectType,omitempty"`
	RouteType  string `json:"routeType,omitempty"`
}

// RouteType contains the protocol type and attributes for one contributor to a nexthop.
type RouteType struct {
	Type       string   `json:"type"`
	Attributes []string `json:"attributes,omitempty"`
}

// NodeInfo contains basic node information.
type NodeInfo struct {
	Name         string               `json:"name"`
	SiteName     string               `json:"siteName"`
	IsGateway    bool                 `json:"isGateway"`
	PodCIDRs     []string             `json:"podCIDRs"`
	InternalIPs  []string             `json:"internalIPs,omitempty"`
	ExternalIPs  []string             `json:"externalIPs,omitempty"`
	BuildInfo    *BuildInfo           `json:"buildInfo,omitempty"`
	WireGuard    *WireGuardStatusInfo `json:"wireGuard,omitempty"`
	K8sReady     string               `json:"k8sReady,omitempty"`
	ProviderID   string               `json:"providerId,omitempty"`
	OSImage      string               `json:"osImage,omitempty"`
	Kernel       string               `json:"kernel,omitempty"`
	Kubelet      string               `json:"kubelet,omitempty"`
	Arch         string               `json:"arch,omitempty"`
	NodeOS       string               `json:"nodeOs,omitempty"`
	K8sLabels    map[string]string    `json:"k8sLabels,omitempty"`
	K8sUpdatedAt *time.Time           `json:"k8sUpdatedAt,omitempty"`
}

// WireGuardStatusInfo contains WireGuard interface information.
type WireGuardStatusInfo struct {
	Interface  string `json:"interface"`
	PublicKey  string `json:"publicKey"`
	ListenPort int    `json:"listenPort"`
	PeerCount  int    `json:"peerCount"`
}

// HealthCheckPeerStatus contains health check session status for a peer.
type HealthCheckPeerStatus struct {
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
	Uptime  string `json:"uptime,omitempty"`
	RTT     string `json:"rtt,omitempty"`
}

// HealthCheckStatus contains aggregate health check service information.
type HealthCheckStatus struct {
	Healthy   bool      `json:"healthy"`
	Summary   string    `json:"summary,omitempty"`
	PeerCount int       `json:"peerCount,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// PeerTunnelStatus contains per-link tunnel transport metrics.
type PeerTunnelStatus struct {
	Protocol      string    `json:"protocol,omitempty"`
	Interface     string    `json:"interface"`
	PublicKey     string    `json:"publicKey,omitempty"`
	Endpoint      string    `json:"endpoint,omitempty"`
	AllowedIPs    []string  `json:"allowedIPs,omitempty"`
	RxBytes       int64     `json:"rxBytes,omitempty"`
	TxBytes       int64     `json:"txBytes,omitempty"`
	LastHandshake time.Time `json:"lastHandshake,omitempty"`
}

// WireGuardPeerLinkStatus is a deprecated alias for PeerTunnelStatus.
type WireGuardPeerLinkStatus = PeerTunnelStatus

// PeerStatus contains information about a tunnel peer.
type PeerStatus struct {
	Name              string                 `json:"name"`
	PeerType          string                 `json:"peerType"`
	SiteName          string                 `json:"siteName,omitempty"`
	PodCIDRGateways   []string               `json:"podCidrGateways,omitempty"`
	SkipPodCIDRRoutes bool                   `json:"skipPodCidrRoutes,omitempty"`
	RouteDistances    map[string]int         `json:"routeDistances,omitempty"`
	Tunnel            PeerTunnelStatus       `json:"tunnel"`
	RouteDestinations []string               `json:"routeDestinations,omitempty"`
	HealthCheck       *HealthCheckPeerStatus `json:"healthCheck,omitempty"`
}

// Deprecated aliases for backward compatibility during transition.
// These will be removed in a future version.

// WireGuardPeerStatus is a deprecated alias for PeerStatus.
type WireGuardPeerStatus = PeerStatus
