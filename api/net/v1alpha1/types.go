// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=st
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node CIDRs",type=string,JSONPath=".spec.nodeCidrs"
// +kubebuilder:printcolumn:name="Pod CIDR Assignments",type=string,JSONPath=".spec.podCidrAssignments"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.nodeCount"
// +kubebuilder:printcolumn:name="Slices",type=integer,JSONPath=".status.sliceCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Site defines a network location containing nodes
type Site struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteSpec   `json:"spec"`
	Status SiteStatus `json:"status,omitempty"`
}

// SiteSpec defines the desired state of Site
// +k8s:deepcopy-gen=true
type SiteSpec struct {
	// NodeCidrs are the CIDRs that contain the internal IPs of nodes at this site
	// +kubebuilder:validation:MinItems=1
	NodeCidrs []string `json:"nodeCidrs"`

	// PodCidrAssignments define how pod CIDRs are allocated to nodes in this site.
	// +kubebuilder:validation:MinItems=1
	PodCidrAssignments []PodCidrAssignment `json:"podCidrAssignments"`

	// ManageCniPlugin controls whether the node agent writes CNI configuration
	// and creates tunnel endpoints for same-site nodes. When true (the default),
	// CNI config is written and all same-site nodes are tunnel peers.
	// When false, only tunnel links to gateway pools for other sites are created,
	// allowing an external CNI plugin to manage intra-site networking.
	// Pod CIDR assignment is also disabled when manageCniPlugin is false,
	// regardless of the assignmentEnabled setting on individual podCidrAssignments
	// rules. The podCidrAssignments are still required to define the CIDR ranges
	// used for inter-site routing.
	// +optional
	ManageCniPlugin *bool `json:"manageCniPlugin,omitempty"`

	// NonMasqueradeCIDRs are CIDR blocks that should NOT be masqueraded when
	// traffic leaves the node via the default gateway. Traffic to these CIDRs
	// will use the pod's actual IP address. This is useful for preserving source
	// IPs when communicating with external networks (e.g., corporate networks, VPNs).
	// If nodes are Azure VMs/VMSS instances, NIC ipForwarding must be enabled
	// for this setting to work correctly.
	// +optional
	NonMasqueradeCIDRs []string `json:"nonMasqueradeCIDRs,omitempty"`

	// LocalCIDRs are CIDR blocks that are considered local to this site.
	// Traffic to these CIDRs should never be routed via gateway pools.
	// +optional
	LocalCIDRs []string `json:"localCidrs,omitempty"`

	// HealthCheckSettings controls health check settings for inter-site tunnel peers.
	// +optional
	HealthCheckSettings *HealthCheckSettings `json:"healthCheckSettings,omitempty"`

	// TunnelProtocol selects the tunnel protocol for this scope.
	// Valid values are "WireGuard", "IPIP", "GENEVE", "VXLAN", "None", or "Auto".
	// Defaults to "Auto" when unset. When "Auto", links using external IPs
	// use WireGuard and links using only internal IPs use GENEVE.
	// +kubebuilder:validation:Enum=WireGuard;IPIP;GENEVE;VXLAN;None;Auto
	// +optional
	TunnelProtocol *TunnelProtocol `json:"tunnelProtocol,omitempty"`

	// TunnelMTU is the MTU to set on routes through tunnels for this scope.
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	// +optional
	TunnelMTU *int32 `json:"tunnelMTU,omitempty"`
}

// PodCidrAssignment defines a pod CIDR allocation rule for nodes in a site.
// +k8s:deepcopy-gen=true
type PodCidrAssignment struct {
	// AssignmentEnabled controls whether this assignment is active. Defaults to true.
	// +optional
	AssignmentEnabled *bool `json:"assignmentEnabled,omitempty"`

	// CidrBlocks are the CIDR pools to allocate from.
	// +kubebuilder:validation:MinItems=1
	CidrBlocks []string `json:"cidrBlocks"`

	// NodeBlockSizes define the per-node allocation sizes for IPv4 and IPv6.
	// +optional
	NodeBlockSizes *NodeBlockSizes `json:"nodeBlockSizes,omitempty"`

	// NodeRegex is a list of regex patterns for matching node names. If empty, no regex filtering is applied.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:items:MaxLength=1024
	NodeRegex []string `json:"nodeRegex,omitempty"`

	// Priority controls which assignment wins when multiple match. Lower values win.
	// Defaults to 100.
	// +optional
	Priority *int32 `json:"priority,omitempty"`
}

// NodeBlockSizes defines per-node CIDR block sizes.
// +k8s:deepcopy-gen=true
type NodeBlockSizes struct {
	// IPv4 is the IPv4 subnet mask size (e.g., 24 for /24).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	IPv4 int `json:"ipv4,omitempty"`

	// IPv6 is the IPv6 subnet mask size (e.g., 80 for /80).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	IPv6 int `json:"ipv6,omitempty"`
}

// HealthCheckSettings configures health check parameters for tunnel-based routing.
// +k8s:deepcopy-gen=true
type HealthCheckSettings struct {
	// Enabled controls whether health check is enabled for this scope.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// DetectMultiplier is the multiplier used to determine session down detection.
	// Valid range is 1-255.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=255
	// +optional
	DetectMultiplier *int32 `json:"detectMultiplier,omitempty"`

	// ReceiveInterval is the minimum interval between received health check packets.
	// Accepts either a duration string (e.g. "300ms") or an integer interpreted as milliseconds.
	// +kubebuilder:validation:XIntOrString
	// +optional
	ReceiveInterval *intstr.IntOrString `json:"receiveInterval,omitempty"`

	// TransmitInterval is the minimum interval between transmitted health check packets.
	// Accepts either a duration string (e.g. "300ms") or an integer interpreted as milliseconds.
	// +kubebuilder:validation:XIntOrString
	// +optional
	TransmitInterval *intstr.IntOrString `json:"transmitInterval,omitempty"`
}

// SpecEnabled reports whether an optional spec.enabled flag is enabled.
// Nil defaults to true for backward compatibility.
func SpecEnabled(enabled *bool) bool {
	if enabled == nil {
		return true
	}

	return *enabled
}

// SiteStatus defines the observed state of Site
type SiteStatus struct {
	// NodeCount is the number of nodes matched to this site
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`

	// SliceCount is the number of SiteNodeSlice objects for this site
	// +optional
	SliceCount int `json:"sliceCount,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SiteList contains a list of Site
type SiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Site `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=sns
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=".siteName"
// +kubebuilder:printcolumn:name="Index",type=integer,JSONPath=".sliceIndex"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".nodeCount",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SiteNodeSlice contains a slice of nodes belonging to a site
// Each slice can contain up to 500 nodes to limit object size
type SiteNodeSlice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// SiteName is the name of the Site this slice belongs to
	SiteName string `json:"siteName"`

	// SliceIndex is the index of this slice (0-based)
	SliceIndex int `json:"sliceIndex"`

	// Nodes contains detailed information about nodes in this slice
	Nodes []NodeInfo `json:"nodes,omitempty"`

	// NodeCount is the number of nodes in this slice.
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SiteNodeSliceList contains a list of SiteNodeSlice
type SiteNodeSliceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteNodeSlice `json:"items"`
}

// NodeInfo contains detailed information about a node in a site
// +k8s:deepcopy-gen=true
type NodeInfo struct {
	// Name is the node name
	Name string `json:"name"`

	// WireGuardPublicKey is the node's WireGuard public key
	// +optional
	WireGuardPublicKey string `json:"wireGuardPublicKey,omitempty"`

	// InternalIPs are the node's internal IP addresses
	// +optional
	InternalIPs []string `json:"internalIPs,omitempty"`

	// PodCIDRs are the pod CIDRs assigned to this node
	// +optional
	PodCIDRs []string `json:"podCIDRs,omitempty"`
}

// MaxNodesPerSlice is the maximum number of nodes per SiteNodeSlice
const MaxNodesPerSlice = 500

// TunnelProtocol specifies the tunnel protocol used for a link scope.
type TunnelProtocol string

const (
	// TunnelProtocolWireGuard selects WireGuard (encrypted) tunneling.
	TunnelProtocolWireGuard TunnelProtocol = "WireGuard"

	// TunnelProtocolIPIP selects IPIP (IP-in-IP) tunneling.
	// IPIP has lower overhead than GENEVE and is preferred for private networks.
	TunnelProtocolIPIP TunnelProtocol = "IPIP"

	// TunnelProtocolGENEVE selects GENEVE (unencrypted) tunneling.
	TunnelProtocolGENEVE TunnelProtocol = "GENEVE"

	// TunnelProtocolVXLAN selects VXLAN tunneling using a single external/
	// flow-based vxlan0 interface with per-route lwt encap ip directives.
	TunnelProtocolVXLAN TunnelProtocol = "VXLAN"

	// TunnelProtocolNone selects direct routing with no tunnel encapsulation.
	// Routes are programmed on the default route interface using the peer's
	// internal IP as the gateway. Requires L3 reachability between nodes.
	TunnelProtocolNone TunnelProtocol = "None"

	// TunnelProtocolAuto lets the system choose based on link characteristics
	// and the configured preferred encapsulation settings. By default, links
	// using external/public IPs use WireGuard; links using only internal IPs
	// use IPIP.
	TunnelProtocolAuto TunnelProtocol = "Auto"
)

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=gp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Selector",type=string,JSONPath=".spec.nodeSelector"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.nodeCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GatewayPool defines a pool of gateway nodes selected by labels
type GatewayPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayPoolSpec   `json:"spec"`
	Status GatewayPoolStatus `json:"status,omitempty"`
}

// GatewayPoolSpec defines the desired state of GatewayPool
// +k8s:deepcopy-gen=true
type GatewayPoolSpec struct {
	// Type controls how gateway nodes are selected and connected.
	// Allowed values are "External" or "Internal".
	// +kubebuilder:validation:Enum=External;Internal;""
	// +optional
	Type string `json:"type,omitempty"`

	// NodeSelector selects nodes to include in this gateway pool
	// Only nodes with external IPs and WireGuard public keys will be included
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`

	// RoutedCidrs are the CIDR blocks that should be routed through this gateway pool
	// +optional
	RoutedCidrs []string `json:"routedCidrs,omitempty"`

	// HealthCheckSettings controls health check settings for routes to peers in this gateway pool.
	// +optional
	HealthCheckSettings *HealthCheckSettings `json:"healthCheckSettings,omitempty"`

	// TunnelProtocol selects the tunnel protocol for this scope.
	// Valid values are "WireGuard", "IPIP", "GENEVE", "VXLAN", "None", or "Auto".
	// Defaults to "Auto" when unset. When "Auto", links using external IPs
	// use WireGuard and links using only internal IPs use GENEVE.
	// +kubebuilder:validation:Enum=WireGuard;IPIP;GENEVE;VXLAN;None;Auto
	// +optional
	TunnelProtocol *TunnelProtocol `json:"tunnelProtocol,omitempty"`

	// TunnelMTU is the MTU to set on routes through tunnels for this scope.
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	// +optional
	TunnelMTU *int32 `json:"tunnelMTU,omitempty"`
}

// GatewayPoolStatus defines the observed state of GatewayPool
// +k8s:deepcopy-gen=true
type GatewayPoolStatus struct {
	// Nodes contains information about nodes in this gateway pool
	// +optional
	Nodes []GatewayNodeInfo `json:"nodes,omitempty"`

	// NodeCount is the number of nodes in this gateway pool
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`

	// ConnectedSites is the list of sites directly peered to this gateway pool.
	// +optional
	ConnectedSites []string `json:"connectedSites,omitempty"`

	// ReachableSites is the list of sites reachable via this gateway pool.
	// +optional
	ReachableSites []string `json:"reachableSites,omitempty"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=gpn
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Gateway Pool",type=string,JSONPath=".spec.gatewayPool"
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=".spec.site"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GatewayPoolNode represents route advertisements from a gateway pool node.
type GatewayPoolNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayNodeSpec   `json:"spec"`
	Status GatewayNodeStatus `json:"status,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GatewayPoolNodeList contains a list of GatewayPoolNode.
type GatewayPoolNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayPoolNode `json:"items"`
}

// GatewayNodeSpec defines immutable identity metadata for a GatewayNode.
// +k8s:deepcopy-gen=true
type GatewayNodeSpec struct {
	// NodeName is the Kubernetes node name publishing this GatewayNode.
	NodeName string `json:"nodeName"`

	// GatewayPool is the owning gateway pool name.
	GatewayPool string `json:"gatewayPool"`

	// Site is the site label of this gateway node.
	// +optional
	Site string `json:"site,omitempty"`
}

// GatewayNodeStatus defines the advertised routes and lease heartbeat for a gateway node.
// +k8s:deepcopy-gen=true
type GatewayNodeStatus struct {
	// LastUpdated is the heartbeat timestamp updated by the node agent.
	// +optional
	LastUpdated metav1.Time `json:"lastUpdated,omitempty"`

	// Routes is a map of reachable CIDRs advertised by this gateway node.
	// The map key is the CIDR prefix.
	// +optional
	// +kubebuilder:validation:MaxProperties=1000
	Routes map[string]GatewayNodeRoute `json:"routes,omitempty"`
}

// GatewayNodeRoute describes a single advertised route.
// +k8s:deepcopy-gen=true
type GatewayNodeRoute struct {
	// Type identifies the route source type (NodeCidr, PodCidr, RoutedCidr).
	Type string `json:"type"`

	// Source identifies the originating object for this route.
	// +optional
	Source *GatewayNodePathHop `json:"source,omitempty"`

	// IntermediateHops identifies additional path objects between source and destination.
	// +optional
	IntermediateHops []GatewayNodePathHop `json:"intermediateHops,omitempty"`

	// Paths is a list of full end-to-end paths.
	// Each item is one complete ordered hop sequence from origin to local advertiser.
	// +optional
	// +kubebuilder:validation:MaxItems=100
	Paths [][]GatewayNodePathHop `json:"paths,omitempty"`
}

// GatewayNodePathHop is a single path hop object.
// +k8s:deepcopy-gen=true
type GatewayNodePathHop struct {
	// Type is the hop object kind (for example: Site, GatewayPool).
	Type string `json:"type"`

	// Name is the hop object name.
	Name string `json:"name"`
}

// GatewayPoolRoute represents a reachable routed CIDR and its aggregate weight.
// +k8s:deepcopy-gen=true
type GatewayPoolRoute struct {
	// CIDR is the reachable route prefix.
	CIDR string `json:"cidr"`

	// Weight is the aggregate route weight for the path to this CIDR.
	Weight int `json:"weight"`

	// Type classifies the route source (for example: podCidr, nodeCidr, routedCidr).
	// +optional
	Type string `json:"type,omitempty"`

	// Origin identifies the source site and/or gateway pool for this route.
	// +optional
	Origin GatewayPoolRouteOrigin `json:"origin,omitempty"`

	// Description explains the source of the reachable CIDR.
	// +optional
	Description string `json:"description,omitempty"`
}

// GatewayPoolRouteOrigin describes where a reachable route originated.
// +k8s:deepcopy-gen=true
type GatewayPoolRouteOrigin struct {
	// Site is the originating site name, when applicable.
	// +optional
	Site string `json:"site,omitempty"`

	// GatewayPool is the originating gateway pool name, when applicable.
	// +optional
	GatewayPool string `json:"gatewayPool,omitempty"`
}

// GatewayNodeInfo contains information about a gateway node
// +k8s:deepcopy-gen=true
type GatewayNodeInfo struct {
	// Name is the node name
	Name string `json:"name"`

	// SiteName is the name of the site this gateway node belongs to
	// +optional
	SiteName string `json:"siteName,omitempty"`

	// InternalIPs are the node's internal IP addresses (for same-site connections)
	// +optional
	InternalIPs []string `json:"internalIPs,omitempty"`

	// ExternalIPs are the node's external IP addresses (for cross-site connections)
	// +optional
	ExternalIPs []string `json:"externalIPs,omitempty"`

	// HealthEndpoints are health check IP addresses (e.g., 10.0.1.1 or fd00::1)
	// +optional
	HealthEndpoints []string `json:"healthEndpoints,omitempty"`

	// WireGuardPublicKey is the node's WireGuard public key
	WireGuardPublicKey string `json:"wireGuardPublicKey"`

	// GatewayWireguardPort is the WireGuard listen port assigned to this
	// gateway node for gateway-to-gateway peering. Ports are allocated by the
	// controller starting at 51821 and are unique across all gateway pools.
	// +optional
	GatewayWireguardPort int32 `json:"gatewayWireguardPort,omitempty"`

	// PodCIDRs are the pod CIDRs assigned to this gateway node
	// +optional
	PodCIDRs []string `json:"podCIDRs,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GatewayPoolList contains a list of GatewayPool
type GatewayPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayPool `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=spr
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Sites",type=integer,JSONPath=".status.peeredSiteCount"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.totalNodeCount"
// +kubebuilder:printcolumn:name="Mesh Nodes",type=boolean,JSONPath=".spec.meshNodes"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SitePeering defines direct peering between sites.
type SitePeering struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SitePeeringSpec   `json:"spec"`
	Status SitePeeringStatus `json:"status,omitempty"`
}

// SitePeeringSpec defines desired state of SitePeering.
// +k8s:deepcopy-gen=true
type SitePeeringSpec struct {
	// Enabled controls whether this peering is active.
	// Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Sites is the list of site names that should be directly peered.
	// +kubebuilder:validation:MinItems=2
	Sites []string `json:"sites,omitempty"`

	// MeshNodes controls whether nodes in listed sites mesh directly.
	// +optional
	MeshNodes *bool `json:"meshNodes,omitempty"`

	// HealthCheckSettings controls health check settings for inter-site peering.
	// +optional
	HealthCheckSettings *HealthCheckSettings `json:"healthCheckSettings,omitempty"`

	// TunnelProtocol selects the tunnel protocol for this scope.
	// Valid values are "WireGuard", "IPIP", "GENEVE", "VXLAN", "None", or "Auto".
	// Defaults to "Auto" when unset. When "Auto", links using external IPs
	// use WireGuard and links using only internal IPs use GENEVE.
	// +kubebuilder:validation:Enum=WireGuard;IPIP;GENEVE;VXLAN;None;Auto
	// +optional
	TunnelProtocol *TunnelProtocol `json:"tunnelProtocol,omitempty"`

	// TunnelMTU is the MTU to set on routes through tunnels for this scope.
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	// +optional
	TunnelMTU *int32 `json:"tunnelMTU,omitempty"`
}

// SitePeeringStatus defines the observed state of SitePeering.
type SitePeeringStatus struct {
	// PeeredSiteCount is the number of sites in this peering that exist.
	// +optional
	PeeredSiteCount int `json:"peeredSiteCount,omitempty"`

	// TotalNodeCount is the total number of nodes across all peered sites.
	// +optional
	TotalNodeCount int `json:"totalNodeCount,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SitePeeringList contains a list of SitePeering.
type SitePeeringList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SitePeering `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=sgpa
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SiteGatewayPoolAssignment links sites to gateway pools.
type SiteGatewayPoolAssignment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SiteGatewayPoolAssignmentSpec `json:"spec"`
}

// SiteGatewayPoolAssignmentSpec defines desired state for site to gateway-pool assignments.
// +k8s:deepcopy-gen=true
type SiteGatewayPoolAssignmentSpec struct {
	// Enabled controls whether this assignment is active.
	// Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Sites is the list of site names.
	// +kubebuilder:validation:MinItems=1
	Sites []string `json:"sites,omitempty"`

	// GatewayPools is the list of gateway pool names.
	// +kubebuilder:validation:MinItems=1
	GatewayPools []string `json:"gatewayPools,omitempty"`

	// HealthCheckSettings controls health check settings for this site-to-pool assignment.
	// +optional
	HealthCheckSettings *HealthCheckSettings `json:"healthCheckSettings,omitempty"`

	// TunnelProtocol selects the tunnel protocol for this scope.
	// Valid values are "WireGuard", "IPIP", "GENEVE", "VXLAN", "None", or "Auto".
	// Defaults to "Auto" when unset. When "Auto", links using external IPs
	// use WireGuard and links using only internal IPs use GENEVE.
	// +kubebuilder:validation:Enum=WireGuard;IPIP;GENEVE;VXLAN;None;Auto
	// +optional
	TunnelProtocol *TunnelProtocol `json:"tunnelProtocol,omitempty"`

	// TunnelMTU is the MTU to set on routes through tunnels for this scope.
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	// +optional
	TunnelMTU *int32 `json:"tunnelMTU,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SiteGatewayPoolAssignmentList contains a list of SiteGatewayPoolAssignment.
type SiteGatewayPoolAssignmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SiteGatewayPoolAssignment `json:"items"`
}

// +genclient
// +genclient:nonNamespaced
// +kubebuilder:resource:scope=Cluster,shortName=gpp
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GatewayPoolPeering links gateway pools for routed connectivity.
type GatewayPoolPeering struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec GatewayPoolPeeringSpec `json:"spec"`
}

// GatewayPoolPeeringSpec defines desired state for gateway-pool peerings.
// +k8s:deepcopy-gen=true
type GatewayPoolPeeringSpec struct {
	// Enabled controls whether this peering is active.
	// Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// GatewayPools is the list of gateway pool names.
	// +kubebuilder:validation:MinItems=2
	GatewayPools []string `json:"gatewayPools,omitempty"`

	// HealthCheckSettings controls health check settings for this pool-to-pool peering.
	// +optional
	HealthCheckSettings *HealthCheckSettings `json:"healthCheckSettings,omitempty"`

	// TunnelProtocol selects the tunnel protocol for this scope.
	// Valid values are "WireGuard", "IPIP", "GENEVE", "VXLAN", "None", or "Auto".
	// Defaults to "Auto" when unset. When "Auto", links using external IPs
	// use WireGuard and links using only internal IPs use GENEVE.
	// +kubebuilder:validation:Enum=WireGuard;IPIP;GENEVE;VXLAN;None;Auto
	// +optional
	TunnelProtocol *TunnelProtocol `json:"tunnelProtocol,omitempty"`

	// TunnelMTU is the MTU to set on routes through tunnels for this scope.
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	// +optional
	TunnelMTU *int32 `json:"tunnelMTU,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// GatewayPoolPeeringList contains a list of GatewayPoolPeering.
type GatewayPoolPeeringList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayPoolPeering `json:"items"`
}
