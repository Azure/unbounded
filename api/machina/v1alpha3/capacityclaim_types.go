// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &CapacityClaim{}, &CapacityClaimList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

const (
	CapacityClaimFinalizer = "unbounded-cloud.io/capacityclaim"

	CapacityClaimConditionBound              = "Bound"
	CapacityClaimConditionConfigurationReady = "ConfigurationReady"
	CapacityClaimConditionReady              = "Ready"
	CapacityClaimConditionReleased           = "Released"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".status.machine.name"
// +kubebuilder:printcolumn:name="Consumer",type="string",JSONPath=".spec.consumer.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CapacityClaim represents unsatisfied capacity that must bind exactly one
// existing Machine.
type CapacityClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CapacityClaimSpec   `json:"spec,omitempty"`
	Status CapacityClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CapacityClaimList contains a list of CapacityClaim objects.
type CapacityClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CapacityClaim `json:"items"`
}

// CapacityClaimSpec defines the capacity, placement, and configuration
// requirements for a claim.
// +kubebuilder:validation:XValidation:rule="!has(self.imagePolicy) || !self.imagePolicy.requireNVIDIA || (has(self.imagePolicy.requiredImage) && size(self.imagePolicy.requiredImage) > 0)",message="requireNVIDIA requires an exact requiredImage trust anchor"
// +kubebuilder:validation:XValidation:rule="!has(self.gpu) || self.gpu.model != 'GB200' || (self.gpu.vendor == 'NVIDIA' && self.gpu.count == 4 && self.imagePolicy.requireNVIDIA && has(self.imagePolicy.requiredImage) && size(self.imagePolicy.requiredImage) > 0)",message="GB200 requires NVIDIA, exactly 4 GPUs, and an exact requiredImage"
type CapacityClaimSpec struct {
	// MachineSelector selects hardware and capacity labels on Machines.
	// +kubebuilder:validation:Required
	MachineSelector metav1.LabelSelector `json:"machineSelector"`

	// SiteSelector further restricts eligible Machines by site labels.
	// +optional
	SiteSelector *metav1.LabelSelector `json:"siteSelector,omitempty"`

	// DomainSelector restricts eligible Machines to allocation domains.
	// A selected Machine must carry unbounded-cloud.io/capacity-domain.
	// +optional
	DomainSelector *metav1.LabelSelector `json:"domainSelector,omitempty"`

	// Consumer identifies the workload or adapter requesting capacity.
	// +kubebuilder:validation:Required
	Consumer CapacityConsumerReference `json:"consumer"`

	// ReleasePolicy controls whether the Kubernetes Node is deleted during
	// release. All other release barriers still apply.
	// +kubebuilder:default=DeleteNode
	ReleasePolicy CapacityReleasePolicy `json:"releasePolicy,omitempty"`

	// ConfigurationRef selects the MachineConfiguration applied to the bound
	// Machine.
	// +kubebuilder:validation:Required
	ConfigurationRef MachineConfigurationRef `json:"configurationRef"`

	// ImagePolicy constrains the image selected by MachineConfiguration.
	// +optional
	ImagePolicy CapacityImagePolicy `json:"imagePolicy,omitempty"`

	// GPU constrains accelerator capacity and health.
	// +optional
	GPU *CapacityGPUPolicy `json:"gpu,omitempty"`
}

// CapacityConsumerReference identifies the consumer of allocated capacity.
type CapacityConsumerReference struct {
	// APIGroup is empty for core Kubernetes resources.
	// +optional
	APIGroup string `json:"apiGroup,omitempty"`

	// Kind is the consumer resource kind.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Namespace is empty for cluster-scoped consumers.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name is the consumer resource name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID prevents a recreated consumer from inheriting an old allocation.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`
}

// +kubebuilder:validation:Enum=DeleteNode;RetainNode
type CapacityReleasePolicy string

const (
	CapacityReleaseDeleteNode CapacityReleasePolicy = "DeleteNode"
	CapacityReleaseRetainNode CapacityReleasePolicy = "RetainNode"
)

// CapacityImagePolicy constrains the configured and observed worker image.
type CapacityImagePolicy struct {
	// RequiredImage requires an exact OCI image reference when set.
	// It is the trust anchor for RequireNVIDIA because image-name substring
	// matching is not a capability check.
	// +optional
	RequiredImage string `json:"requiredImage,omitempty"`

	// RequireNVIDIA requires an NVIDIA-enabled image.
	// +optional
	RequireNVIDIA bool `json:"requireNVIDIA,omitempty"`
}

// CapacityGPUPolicy defines required GPU capacity.
type CapacityGPUPolicy struct {
	// Vendor is the required GPU vendor.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=NVIDIA
	Vendor string `json:"vendor"`

	// Model is the required GPU model, for example GB200.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// Count is the exact required healthy GPU count.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	Count int32 `json:"count"`

	// RequireFabric requires the observed GPU fabric to be healthy.
	// +optional
	RequireFabric bool `json:"requireFabric,omitempty"`
}

// CapacityClaimStatus defines the observed allocation state.
type CapacityClaimStatus struct {
	// Phase is the human-readable claim phase.
	Phase CapacityClaimPhase `json:"phase,omitempty"`

	// ObservedGeneration is the latest spec generation evaluated.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Machine identifies the one Machine bound to this claim.
	// +optional
	Machine *BoundMachineReference `json:"machine,omitempty"`

	// ReleaseStage is the next or current release barrier.
	// +optional
	ReleaseStage CapacityReleaseStage `json:"releaseStage,omitempty"`

	// Conditions are the authoritative allocation observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// BoundMachineReference identifies a bound Machine by immutable Kubernetes
// object identity.
type BoundMachineReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

// +kubebuilder:validation:Enum=Pending;Bound;Provisioning;Ready;Releasing;Failed
type CapacityClaimPhase string

const (
	CapacityClaimPhasePending      CapacityClaimPhase = "Pending"
	CapacityClaimPhaseBound        CapacityClaimPhase = "Bound"
	CapacityClaimPhaseProvisioning CapacityClaimPhase = "Provisioning"
	CapacityClaimPhaseReady        CapacityClaimPhase = "Ready"
	CapacityClaimPhaseReleasing    CapacityClaimPhase = "Releasing"
	CapacityClaimPhaseFailed       CapacityClaimPhase = "Failed"
)

// +kubebuilder:validation:Enum=CordonDrain;RevokeCredentials;ResetWorker;DeleteNode;ReleaseReservation;Complete
type CapacityReleaseStage string

const (
	CapacityReleaseStageCordonDrain        CapacityReleaseStage = "CordonDrain"
	CapacityReleaseStageRevokeCredentials  CapacityReleaseStage = "RevokeCredentials"
	CapacityReleaseStageResetWorker        CapacityReleaseStage = "ResetWorker"
	CapacityReleaseStageDeleteNode         CapacityReleaseStage = "DeleteNode"
	CapacityReleaseStageReleaseReservation CapacityReleaseStage = "ReleaseReservation"
	CapacityReleaseStageComplete           CapacityReleaseStage = "Complete"
)
