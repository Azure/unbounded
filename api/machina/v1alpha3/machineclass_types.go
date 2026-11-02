// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &MachineClass{}, &MachineClassList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

const (
	// MachineClassConditionReady reports whether the class inputs resolved for
	// the current generation and capacity can be requested from it.
	MachineClassConditionReady = "Ready"

	// MachineClassConditionOfferingsCurrent reports whether the published
	// offerings were observed recently enough to be trusted.
	MachineClassConditionOfferingsCurrent = "OfferingsCurrent"

	// MachineClassBootstrapRunKey is the required key in the bootstrap files
	// ConfigMap that holds the script run before the agent installer.
	MachineClassBootstrapRunKey = "run"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mcls
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Site",type="string",JSONPath=".spec.siteRef.name"
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineClass describes a provider-neutral template from which capacity
// consumers request MachineAllocations. Provider-specific settings live in an
// object referenced by providerConfigRef and are interpreted only by the
// provider adapter.
type MachineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   MachineClassSpec   `json:"spec"`
	Status MachineClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineClassList contains a list of MachineClass.
type MachineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineClass `json:"items"`
}

// MachineClassSpec defines the inputs shared by allocations from this class.
// +kubebuilder:validation:XValidation:rule="self.provider == oldSelf.provider",message="provider is immutable"
// +kubebuilder:validation:XValidation:rule="self.siteRef == oldSelf.siteRef",message="siteRef is immutable"
type MachineClassSpec struct {
	// SiteRef identifies the Site that allocated machines join. It is
	// immutable after creation.
	// +kubebuilder:validation:Required
	SiteRef LocalObjectReference `json:"siteRef"`

	// Provider selects the registered provider adapter that interprets
	// providerConfigRef and executes allocation operations. It is immutable
	// after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider"`

	// ProviderConfigRef references provider-specific configuration. Only a
	// namespaced ConfigMap in the core API group is accepted today.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="!has(self.apiGroup) || self.apiGroup == ''",message="providerConfigRef.apiGroup must be the core group"
	// +kubebuilder:validation:XValidation:rule="self.kind == 'ConfigMap'",message="providerConfigRef.kind must be ConfigMap"
	// +kubebuilder:validation:XValidation:rule="self.kind != 'ConfigMap' || has(self.namespace)",message="providerConfigRef.namespace is required for a ConfigMap"
	ProviderConfigRef TypedObjectReference `json:"providerConfigRef"`

	// ConfigurationRef pins the MachineConfiguration version whose content is
	// frozen into each allocation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="has(self.version)",message="configurationRef.version is required"
	ConfigurationRef MachineConfigurationRef `json:"configurationRef"`

	// Bootstrap optionally customizes first-boot artifact delivery.
	// +optional
	Bootstrap *MachineClassBootstrap `json:"bootstrap,omitempty"`
}

// TypedObjectReference identifies an object by API group, kind, name, and
// optional namespace.
type TypedObjectReference struct {
	// APIGroup is the API group of the referenced object. An absent or empty
	// value means the core API group.
	// +optional
	APIGroup string `json:"apiGroup,omitempty"`

	// Kind is the kind of the referenced object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the name of the referenced object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace of the referenced object. It is absent for
	// cluster-scoped kinds.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace,omitempty"`
}

// NamespacedObjectReference identifies a namespaced object.
type NamespacedObjectReference struct {
	// Namespace is the referenced object namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Name is the referenced object name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// MachineClassBootstrap customizes first-boot artifact delivery. It must not
// contain credentials; the referenced files become part of host user data.
type MachineClassBootstrap struct {
	// FilesRef references a ConfigMap of bootstrap files for hosts launched
	// from this class. Its required "run" key is the script that runs as root
	// on the host before the agent installer.
	// +optional
	FilesRef *NamespacedObjectReference `json:"filesRef,omitempty"`

	// AgentArtifact overrides the agent tarball location and pins its digest.
	// +optional
	AgentArtifact *AgentArtifact `json:"agentArtifact,omitempty"`
}

// AgentArtifact identifies a digest-pinned agent tarball.
type AgentArtifact struct {
	// URL is the agent tarball location: an https URL whose host is a DNS name
	// or dotted IPv4 address with an optional port from 1 to 65535, or a local
	// file:/// path. It must not embed credentials, a query, or a fragment.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^(https://[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*(:(6553[0-5]|655[0-2][0-9]|65[0-4][0-9]{2}|6[0-4][0-9]{3}|[1-5][0-9]{4}|[1-9][0-9]{0,3}))?|file://)(/[A-Za-z0-9._~%+=,:-]+)+$`
	URL string `json:"url"`

	// SHA256 is the lowercase hex SHA-256 digest of the tarball.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	SHA256 string `json:"sha256"`
}

// MachineClassStatus reports the normalized, observed class state.
type MachineClassStatus struct {
	// ObservedGeneration is the class generation whose inputs were resolved.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedRevision identifies the resolved spec, provider config,
	// bootstrap files, and configuration content. Allocations whose frozen
	// revision differs are drifted.
	// +optional
	ResolvedRevision string `json:"resolvedRevision,omitempty"`

	// ProviderConfig records the provider config observed for the revision.
	// +optional
	ProviderConfig *ObjectContentSnapshot `json:"providerConfig,omitempty"`

	// BootstrapFiles records the bootstrap files observed for the revision.
	// +optional
	BootstrapFiles *ObjectContentSnapshot `json:"bootstrapFiles,omitempty"`

	// Configuration records the configuration version observed for the revision.
	// +optional
	Configuration *ConfigurationContentSnapshot `json:"configuration,omitempty"`

	// Site is the exact Site instance the revision was resolved for. A Site
	// deleted and recreated under the same name is a different authority.
	// +optional
	Site *UIDReference `json:"site,omitempty"`

	// Credential is the exact MachineOperationCredential instance the
	// revision was resolved with.
	// +optional
	Credential *UIDReference `json:"credential,omitempty"`

	// Offerings are the normalized capacity offerings for this class.
	// +optional
	// +listType=map
	// +listMapKey=name
	Offerings []MachineClassOffering `json:"offerings,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ObjectContentSnapshot identifies the exact content of an object.
type ObjectContentSnapshot struct {
	// Namespace is the object namespace.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Name is the object name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID detects deletion and recreation of the object.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// Hash is the SHA-256 of the object content.
	// +kubebuilder:validation:Required
	Hash string `json:"hash"`
}

// ConfigurationContentSnapshot identifies the exact configuration content.
type ConfigurationContentSnapshot struct {
	// Name is the MachineConfiguration name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Version is the MachineConfiguration version.
	// +kubebuilder:validation:Required
	Version int32 `json:"version"`

	// VersionName is the MachineConfigurationVersion object name.
	// +kubebuilder:validation:Required
	VersionName string `json:"versionName"`

	// UID is the MachineConfigurationVersion UID.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// Hash is the SHA-256 of the configuration template.
	// +kubebuilder:validation:Required
	Hash string `json:"hash"`
}

// CapacityType identifies the purchase model of an offering.
// +kubebuilder:validation:Enum=OnDemand;Spot
type CapacityType string

const (
	// CapacityTypeOnDemand is regular, non-preemptible capacity.
	CapacityTypeOnDemand CapacityType = "OnDemand"

	// CapacityTypeSpot is preemptible capacity.
	CapacityTypeSpot CapacityType = "Spot"
)

// OfferingAvailability is the observed availability of an offering.
// +kubebuilder:validation:Enum=Available;Unavailable;Unknown
type OfferingAvailability string

const (
	// OfferingAvailable means the provider reported the offering as usable.
	OfferingAvailable OfferingAvailability = "Available"

	// OfferingUnavailable means the provider reported the offering as unusable.
	OfferingUnavailable OfferingAvailability = "Unavailable"

	// OfferingAvailabilityUnknown means availability could not be observed.
	OfferingAvailabilityUnknown OfferingAvailability = "Unknown"
)

// MachineClassOffering is one normalized purchasable capacity shape.
type MachineClassOffering struct {
	// Name uniquely identifies the offering within the class.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// InstanceType is the provider instance type.
	// +kubebuilder:validation:Required
	InstanceType string `json:"instanceType"`

	// Zone is the provider zone, or empty for regional capacity.
	// +optional
	Zone string `json:"zone,omitempty"`

	// CapacityType is the purchase model.
	// +kubebuilder:validation:Required
	CapacityType CapacityType `json:"capacityType"`

	// Labels are the well-known labels Nodes from this offering register with.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Capacity is the machine capacity before overhead.
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`

	// Overhead is the capacity reserved from workloads.
	// +optional
	Overhead OfferingOverhead `json:"overhead,omitempty"`

	// Availability is the observed availability.
	// +kubebuilder:validation:Required
	Availability OfferingAvailability `json:"availability"`

	// Price is the observed or configured price, when known.
	// +optional
	Price *OfferingPrice `json:"price,omitempty"`

	// ObservedAt is when availability and capacity were observed.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// ValidUntil is when this observation expires. An expired offering must
	// not be used to launch a machine.
	// +optional
	ValidUntil *metav1.Time `json:"validUntil,omitempty"`
}

// OfferingOverhead is the capacity unavailable to workloads.
type OfferingOverhead struct {
	// KubeReserved is reserved for Kubernetes node components.
	// +optional
	KubeReserved corev1.ResourceList `json:"kubeReserved,omitempty"`

	// SystemReserved is reserved for operating system daemons.
	// +optional
	SystemReserved corev1.ResourceList `json:"systemReserved,omitempty"`

	// EvictionThreshold is the hard eviction threshold.
	// +optional
	EvictionThreshold corev1.ResourceList `json:"evictionThreshold,omitempty"`
}

// OfferingPrice is a normalized price for an offering.
type OfferingPrice struct {
	// Currency is the ISO 4217 currency code.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[A-Z]{3}$`
	Currency string `json:"currency"`

	// PerHour is the decimal price per instance hour.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	PerHour string `json:"perHour"`

	// Source documents where the price came from and what it includes.
	// +optional
	Source string `json:"source,omitempty"`

	// ObservedAt is when the price was observed.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}
