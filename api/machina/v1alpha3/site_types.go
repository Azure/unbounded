// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
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
// +kubebuilder:printcolumn:name="Network",type=boolean,JSONPath=".spec.components.net.enabled"
// +kubebuilder:printcolumn:name="Machina",type=boolean,JSONPath=".spec.components.machina.enabled"
// +kubebuilder:printcolumn:name="Metalman",type=boolean,JSONPath=".spec.components.metalman.enabled"
// +kubebuilder:printcolumn:name="Storage",type=boolean,JSONPath=".spec.components.unboundedStorage.enabled"
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=".status.nodeCount"
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
type SiteComponents struct {
	// Net configures unbounded-net for this site.
	// +optional
	Net *SiteComponentSpec `json:"net,omitempty"`

	// Machina configures the machina controller for this site.
	// +optional
	Machina *SiteComponentSpec `json:"machina,omitempty"`

	// Metalman configures the Metalman PXE controller for this site.
	// +optional
	Metalman *MetalmanComponentSpec `json:"metalman,omitempty"`

	// UnboundedStorage configures the unbounded-storage supervisor for this site.
	// +optional
	UnboundedStorage *UnboundedStorageComponentSpec `json:"unboundedStorage,omitempty"`
}

// SiteComponentSpec contains common component configuration.
type SiteComponentSpec struct {
	// Enabled controls whether the component is reconciled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Namespace is the namespace where namespaced component resources are deployed.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Image overrides the default component image.
	// +optional
	Image string `json:"image,omitempty"`
}

// MetalmanComponentSpec configures Metalman for a site.
type MetalmanComponentSpec struct {
	SiteComponentSpec `json:",inline"`

	// DHCPAutoInterface lets Metalman choose the DHCP interface automatically.
	// +optional
	DHCPAutoInterface *bool `json:"dhcpAutoInterface,omitempty"`
}

// UnboundedStorageComponentSpec configures unbounded-storage for a site.
type UnboundedStorageComponentSpec struct {
	SiteComponentSpec `json:",inline"`

	// Config overrides the supervisor config.yaml content.
	// +optional
	Config string `json:"config,omitempty"`
}

// SiteStatus defines the observed state of Site.
type SiteStatus struct {
	// NodeCount is the number of nodes matched to this site by unbounded-net.
	// +optional
	NodeCount int `json:"nodeCount,omitempty"`

	// SliceCount is the number of SiteNodeSlice objects for this site.
	// +optional
	SliceCount int `json:"sliceCount,omitempty"`

	// Components reports the last observed state of site components.
	// +optional
	Components map[string]SiteComponentStatus `json:"components,omitempty"`
}

// SiteComponentStatus reports the observed state of a component.
type SiteComponentStatus struct {
	// Ready indicates whether the component was reconciled successfully.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Message contains a short human-readable status message.
	// +optional
	Message string `json:"message,omitempty"`
}

// ComponentEnabled reports whether a component spec enables a component.
// Components default to disabled unless explicitly enabled.
func ComponentEnabled(component *SiteComponentSpec) bool {
	return component != nil && component.Enabled != nil && *component.Enabled
}
