// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Site{}, &SiteList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=st
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node CIDRs",type=string,JSONPath=".spec.nodeCidrs"
// +kubebuilder:printcolumn:name="Pod CIDR Assignments",type=string,JSONPath=".spec.podCidrAssignments"
// +kubebuilder:printcolumn:name="Machina",type=boolean,JSONPath=".spec.components.machina.enabled",priority=1
// +kubebuilder:printcolumn:name="Metalman",type=boolean,JSONPath=".spec.components.metalman.enabled",priority=1
// +kubebuilder:printcolumn:name="Storage",type=boolean,JSONPath=".spec.components.storage.enabled",priority=1
// +kubebuilder:printcolumn:name="Gantry",type=boolean,JSONPath=".spec.components.gantry.enabled",priority=1
// +kubebuilder:printcolumn:name="Racer",type=boolean,JSONPath=".spec.components.racer.enabled",priority=1
// +kubebuilder:printcolumn:name="Snapshotter",type=boolean,JSONPath=".spec.components.gantrySnapshotter.enabled",priority=1
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.nodeCount"
// +kubebuilder:printcolumn:name="Slices",type=integer,JSONPath=".status.sliceCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Site defines a top-level Unbounded location and the optional components
// enabled for that location.
type Site struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SiteSpec   `json:"spec,omitempty"`
	Status SiteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SiteList contains a list of Site.
type SiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Site `json:"items"`
}

// SiteSpec defines the desired state of Site.
type SiteSpec struct {
	// NodeCidrs are the CIDRs that contain the internal IPs of nodes at this site.
	// +kubebuilder:validation:MinItems=1
	NodeCidrs []string `json:"nodeCidrs"`

	// PodCidrAssignments define how pod CIDRs are allocated to nodes in this site.
	// +kubebuilder:validation:MinItems=1
	PodCidrAssignments []unboundednetv1alpha1.PodCidrAssignment `json:"podCidrAssignments"`

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
	// will use the pod's actual IP address.
	// +optional
	NonMasqueradeCIDRs []string `json:"nonMasqueradeCIDRs,omitempty"`

	// LocalCIDRs are CIDR blocks that are considered local to this site.
	// Traffic to these CIDRs should never be routed via gateway pools.
	// +optional
	LocalCIDRs []string `json:"localCidrs,omitempty"`

	// HealthCheckSettings controls health check settings for inter-site tunnel peers.
	// +optional
	HealthCheckSettings *unboundednetv1alpha1.HealthCheckSettings `json:"healthCheckSettings,omitempty"`

	// TunnelProtocol selects the tunnel protocol for this site.
	// Valid values are "WireGuard", "IPIP", "GENEVE", "VXLAN", "None", or "Auto".
	// Defaults to "Auto" when unset.
	// +kubebuilder:validation:Enum=WireGuard;IPIP;GENEVE;VXLAN;None;Auto
	// +optional
	TunnelProtocol *unboundednetv1alpha1.TunnelProtocol `json:"tunnelProtocol,omitempty"`

	// TunnelMTU is the MTU to set on routes through tunnels for this site.
	// +kubebuilder:validation:Minimum=576
	// +kubebuilder:validation:Maximum=9000
	// +optional
	TunnelMTU *int32 `json:"tunnelMTU,omitempty"`

	// Components declares optional Unbounded components managed for this site.
	// +optional
	Components SiteComponents `json:"components,omitempty"`
}

// SiteComponents declares optional components managed by the unbounded operator.
// Networking (unbounded-net) is not a component: it is a cluster singleton the
// operator deploys whenever at least one Site exists.
//
// Each field corresponds to a component reconciler registered with the operator.
// To add a new component, add a typed field here and implement
// component.ClusterComponent or component.SiteComponent in a package under
// internal/operator/components, then register it in operator.DefaultRegistry.
type SiteComponents struct {
	// Machina configures the machina controller for this site.
	// +optional
	Machina *MachinaComponentSpec `json:"machina,omitempty"`

	// Metalman configures the Metalman PXE controller for this site.
	// +optional
	Metalman *MetalmanComponentSpec `json:"metalman,omitempty"`

	// Storage configures the unbounded-storage supervisor for this site.
	// +optional
	Storage *StorageComponentSpec `json:"storage,omitempty"`

	// Gantry configures the gantry peer-to-peer OCI distribution agent for this
	// site. Unlike the other components, gantry defaults to enabled; set
	// gantry.enabled to false to opt a site out. The apiserver defaults an
	// omitted gantry block to enabled=true so the intent is explicit on read
	// (for example the Gantry print column) whenever a components block is
	// present. A site that omits the components block entirely is not defaulted.
	// +optional
	// +kubebuilder:default={enabled: true}
	Gantry *GantryComponentSpec `json:"gantry,omitempty"`

	// Racer configures the racer distributed block device for this site. Racer
	// defaults to disabled: it exports block devices and claims a store file on
	// every node enrolled with the racer.unbounded-cloud.io/enabled label, which
	// is not something to switch on by omission.
	// +optional
	Racer *RacerComponentSpec `json:"racer,omitempty"`

	// GantrySnapshotter configures the containerd snapshotter that serves
	// image layers out of racer instead of pulling them. It requires racer,
	// and defaults to disabled: it takes over containerd's snapshotter on
	// every node it runs on, which is not something to switch on by omission.
	// +optional
	GantrySnapshotter *GantrySnapshotterComponentSpec `json:"gantrySnapshotter,omitempty"`
}

// SiteComponentSpec contains common component configuration. Components install
// into the unbounded-system namespace at the operator's own version, so neither
// namespace nor image is configurable per component.
type SiteComponentSpec struct {
	// Enabled controls whether the component is reconciled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// MachinaComponentSpec configures the machina controller for a site.
type MachinaComponentSpec struct {
	SiteComponentSpec `json:",inline"`
}

// MetalmanComponentSpec configures Metalman for a site.
type MetalmanComponentSpec struct {
	SiteComponentSpec `json:",inline"`

	// DHCPAutoInterface lets Metalman choose the DHCP interface automatically.
	// +optional
	DHCPAutoInterface *bool `json:"dhcpAutoInterface,omitempty"`

	// Replicas is the desired number of Metalman replicas. Defaults to 1 when
	// omitted.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
}

// StorageComponentSpec configures unbounded-storage for a site. Storage daemon
// config is held in the operator-managed ConfigMap
// unbounded-storage-config-<site>: the operator creates it from the embedded
// default when absent and preserves/adopts it when present.
type StorageComponentSpec struct {
	SiteComponentSpec `json:",inline"`
}

// GantryComponentSpec configures the gantry peer-to-peer OCI distribution agent
// for a site. Gantry is a cluster-wide singleton and, unlike the other
// components, defaults to enabled: it is reconciled unless a site explicitly
// sets enabled to false.
type GantryComponentSpec struct {
	SiteComponentSpec `json:",inline"`
}

// RacerComponentSpec configures racer for a site. Racer holds data, so the
// operator never uninstalls it automatically: disabling it stops reconciliation
// but leaves the node agents and their stores in place.
type RacerComponentSpec struct {
	SiteComponentSpec `json:",inline"`
}

// GantrySnapshotterComponentSpec configures the gantry snapshotter for a site.
//
// The snapshotter needs somewhere to put layers, and that somewhere is a set of
// ordinary racer volumes the operator provisions: one catalog and some number
// of segments. Their geometry is here rather than inferred because it cannot be
// changed afterwards. A segment is a single immutable extent, and an extent's
// size is frozen when it is allocated; growing the image address space means
// adding segments, never resizing one.
type GantrySnapshotterComponentSpec struct {
	SiteComponentSpec `json:",inline"`

	// Segments is how many image segments to provision. Defaults to 4.
	//
	// Layers are packed into segments in whole 4 MiB pages, so the usable
	// capacity is Segments times SegmentSize. More, smaller segments cost
	// nothing at rest and bound how much a single lost extent takes with it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	// +optional
	Segments *int32 `json:"segments,omitempty"`

	// SegmentSize is the size of each image segment. Defaults to 8Gi.
	//
	// It must be a multiple of 4Mi: a segment is an IMMUTABLE_4M extent and
	// that is its page size.
	// +optional
	SegmentSize *resource.Quantity `json:"segmentSize,omitempty"`

	// CatalogSize is the size of the image catalog. Defaults to 256Mi.
	//
	// The catalog is an append-only log of one record per layer, so this bounds
	// how many distinct layers the cluster can hold, not how large they are.
	// +optional
	CatalogSize *resource.Quantity `json:"catalogSize,omitempty"`
}

// SiteStatus defines the observed state of Site.
type SiteStatus struct {
	// NodeCount is the number of nodes matched to this site by unbounded-net.
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`

	// SliceCount is the number of SiteNodeSlice objects for this site.
	// +optional
	SliceCount int `json:"sliceCount,omitempty"`

	// Conditions report the last observed state of site components. One
	// condition is published per component (for example NetReady, MachinaReady,
	// MetalmanReady, StorageReady) so callers can `kubectl wait` on a Site.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ComponentEnabled reports whether a component spec enables a component.
// Components default to disabled unless explicitly enabled.
func ComponentEnabled(component *SiteComponentSpec) bool {
	return component != nil && component.Enabled != nil && *component.Enabled
}
