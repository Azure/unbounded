// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &NetbootEndpoint{}, &NetbootEndpointList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

// NetbootEndpointType identifies where an endpoint edge runs.
// +kubebuilder:validation:Enum=ManagedL2;ExternalL2;HTTP
type NetbootEndpointType string

const (
	NetbootEndpointTypeManagedL2  NetbootEndpointType = "ManagedL2"
	NetbootEndpointTypeExternalL2 NetbootEndpointType = "ExternalL2"
	NetbootEndpointTypeHTTP       NetbootEndpointType = "HTTP"
)

// NetbootEndpointTrust identifies the network trust boundary of an endpoint.
// +kubebuilder:validation:Enum=TrustedLAN;Public
type NetbootEndpointTrust string

const (
	NetbootEndpointTrustTrustedLAN NetbootEndpointTrust = "TrustedLAN"
	NetbootEndpointTrustPublic     NetbootEndpointTrust = "Public"
)

// NetbootEndpointTLSMode identifies where TLS is terminated.
// +kubebuilder:validation:Enum=Disabled;Secret;External
type NetbootEndpointTLSMode string

const (
	NetbootEndpointTLSDisabled NetbootEndpointTLSMode = "Disabled"
	NetbootEndpointTLSSecret   NetbootEndpointTLSMode = "Secret"
	NetbootEndpointTLSExternal NetbootEndpointTLSMode = "External"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=nbe
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Site",type="string",JSONPath=".spec.siteRef"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="URL",type="string",JSONPath=".spec.externalURL"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NetbootEndpoint declares a stable client-facing netboot endpoint and its edge
// placement. The endpoint URL is snapshotted into each NetbootSession.
type NetbootEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetbootEndpointSpec   `json:"spec,omitempty"`
	Status NetbootEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetbootEndpointList contains a list of NetbootEndpoint resources.
type NetbootEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetbootEndpoint `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self.tls.trust != 'Public' || (self.externalURL.startsWith('https://') && self.tls.mode != 'Disabled')",message="public endpoints require HTTPS"
// +kubebuilder:validation:XValidation:rule="self.type == 'ManagedL2' ? has(self.managedL2) : !has(self.managedL2)",message="managedL2 configuration must be set only for ManagedL2 endpoints"
// +kubebuilder:validation:XValidation:rule="self.type == 'HTTP' ? has(self.http) : !has(self.http)",message="http configuration must be set only for HTTP endpoints"

// NetbootEndpointSpec defines a stable edge endpoint.
type NetbootEndpointSpec struct {
	// SiteRef names the Site whose Machines may use this endpoint.
	// +kubebuilder:validation:MinLength=1
	SiteRef string `json:"siteRef"`

	// Type identifies how the endpoint edge is operated.
	Type NetbootEndpointType `json:"type"`

	// ExternalURL is the stable base URL advertised to firmware and installers.
	// +kubebuilder:validation:Pattern=`^https?://`
	ExternalURL string `json:"externalURL"`

	// TLS defines the endpoint trust boundary and TLS termination mode.
	TLS NetbootEndpointTLS `json:"tls"`

	// ManagedL2 configures an operator-managed edge on a provisioning LAN.
	// +optional
	ManagedL2 *NetbootManagedL2Spec `json:"managedL2,omitempty"`

	// HTTP configures an operator-managed HTTP edge Service.
	// +optional
	HTTP *NetbootHTTPEndpointSpec `json:"http,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.mode == 'Secret' ? has(self.secretRef) : !has(self.secretRef)",message="secretRef must be set only when TLS mode is Secret"

// NetbootEndpointTLS defines transport security for an endpoint.
type NetbootEndpointTLS struct {
	// Trust identifies whether the endpoint is confined to a trusted LAN or is
	// reachable across an untrusted network.
	Trust NetbootEndpointTrust `json:"trust"`

	// Mode identifies where TLS is terminated.
	Mode NetbootEndpointTLSMode `json:"mode"`

	// SecretRef references the serving certificate when mode is Secret.
	// +optional
	SecretRef *NamespacedSecretReference `json:"secretRef,omitempty"`
}

// NetbootManagedL2Spec places a managed edge on a provisioning network.
type NetbootManagedL2Spec struct {
	// NodeSelector selects nodes attached to the provisioning network.
	NodeSelector metav1.LabelSelector `json:"nodeSelector"`

	// Interface is the host interface used for DHCP and TFTP.
	// +kubebuilder:validation:MinLength=1
	Interface string `json:"interface"`

	// Address is the stable address advertised by DHCP and used by the edge.
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
}

// NetbootHTTPEndpointSpec configures the Service for an HTTP-only edge.
type NetbootHTTPEndpointSpec struct {
	// ServiceType controls how the edge Service is exposed.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
}

// NetbootEndpointStatus reports edge ownership and readiness.
type NetbootEndpointStatus struct {
	// ObservedGeneration is the latest spec generation processed by the edge.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Claim identifies the edge currently responsible for this endpoint.
	// +optional
	Claim *NetbootEndpointClaim `json:"claim,omitempty"`

	// Conditions report endpoint readiness and degradation.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NetbootEndpointClaim records the current endpoint edge claimant.
type NetbootEndpointClaim struct {
	// HolderIdentity uniquely identifies the claiming edge process.
	HolderIdentity string `json:"holderIdentity"`

	// RenewedAt records the latest successful claim heartbeat.
	RenewedAt metav1.Time `json:"renewedAt"`
}
