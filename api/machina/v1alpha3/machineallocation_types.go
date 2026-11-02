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
		s.AddKnownTypes(GroupVersion, &MachineAllocation{}, &MachineAllocationList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

const (
	// AllocationProviderIDPrefix prefixes the allocation UID in Node providerIDs.
	AllocationProviderIDPrefix = "unbounded:///machineallocations/"

	// MachineAllocationConditionAllocated reports whether the host launched.
	MachineAllocationConditionAllocated = "Allocated"

	// MachineAllocationConditionRegistered reports whether the expected Node
	// registered with the allocation providerID.
	MachineAllocationConditionRegistered = "Registered"

	// MachineAllocationConditionDrifted reports whether the frozen class
	// revision differs from the class's current resolved revision.
	MachineAllocationConditionDrifted = "Drifted"

	// MachineAllocationConditionFenced reports whether the host may no longer
	// repave or rejoin. Fencing never releases infrastructure by itself.
	MachineAllocationConditionFenced = "Fenced"

	// MachineAllocationConditionReleased reports whether every remote effect and
	// the per-allocation bootstrap credential were confirmed released.
	MachineAllocationConditionReleased = "Released"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=malloc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type="string",JSONPath=".spec.classRef.name"
// +kubebuilder:printcolumn:name="Lifecycle",type="string",JSONPath=".spec.lifecycle"
// +kubebuilder:printcolumn:name="Allocated",type="string",JSONPath=".status.conditions[?(@.type=='Allocated')].status"
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".status.nodeRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineAllocation is one consumer-owned request for one machine from a
// MachineClass. Its UID owns every remote effect made on its behalf.
type MachineAllocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   MachineAllocationSpec   `json:"spec"`
	Status MachineAllocationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineAllocationList contains a list of MachineAllocation.
type MachineAllocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineAllocation `json:"items"`
}

// MachineAllocationSpec defines the requested machine. Every field except
// lifecycle is immutable.
// +kubebuilder:validation:XValidation:rule="self.consumerRef == oldSelf.consumerRef",message="consumerRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.classRef == oldSelf.classRef",message="classRef is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.resources) == has(oldSelf.resources) && (!has(self.resources) || self.resources == oldSelf.resources)",message="resources is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.requirements) == has(oldSelf.requirements) && (!has(self.requirements) || self.requirements == oldSelf.requirements)",message="requirements is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.nodeTemplate) == has(oldSelf.nodeTemplate) && (!has(self.nodeTemplate) || self.nodeTemplate == oldSelf.nodeTemplate)",message="nodeTemplate is immutable"
type MachineAllocationSpec struct {
	// ConsumerRef identifies the object that requested this allocation.
	// +kubebuilder:validation:Required
	ConsumerRef MachineAllocationConsumerReference `json:"consumerRef"`

	// ClassRef identifies the exact MachineClass the allocation is made from.
	// +kubebuilder:validation:Required
	ClassRef UIDReference `json:"classRef"`

	// Resources are the minimum requested resources.
	// +optional
	Resources corev1.ResourceList `json:"resources,omitempty"`

	// Requirements constrain the offering labels the allocation may use.
	// +optional
	Requirements []MachineAllocationRequirement `json:"requirements,omitempty"`

	// NodeTemplate is applied to the Node at registration.
	// +optional
	NodeTemplate *MachineAllocationNodeTemplate `json:"nodeTemplate,omitempty"`

	// Lifecycle authorizes release. It moves only from Active to Released.
	// +optional
	// +kubebuilder:default=Active
	// +kubebuilder:validation:XValidation:rule="oldSelf == 'Active' || self == 'Released'",message="lifecycle cannot return to Active"
	Lifecycle MachineAllocationLifecycle `json:"lifecycle,omitempty"`
}

// MachineAllocationLifecycle is the consumer's lifecycle intent.
// +kubebuilder:validation:Enum=Active;Released
type MachineAllocationLifecycle string

const (
	// MachineAllocationActive keeps the allocation's machine.
	MachineAllocationActive MachineAllocationLifecycle = "Active"

	// MachineAllocationReleased authorizes releasing the machine. Consumers set
	// it only after their workload and storage safety steps complete.
	MachineAllocationReleased MachineAllocationLifecycle = "Released"
)

// MachineAllocationConsumerReference identifies the requesting object.
type MachineAllocationConsumerReference struct {
	// APIGroup is the consumer API group. An empty value means the core API
	// group.
	// +kubebuilder:validation:Required
	APIGroup string `json:"apiGroup"`

	// Kind is the consumer kind.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the consumer name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID is the consumer UID. A consumer with another UID is a different
	// object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`
}

// UIDReference identifies one object incarnation by name and UID.
type UIDReference struct {
	// Name is the object name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID is the object UID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	UID types.UID `json:"uid"`
}

// ProviderOwnedResource is a provider resource owned by one allocation.
type ProviderOwnedResource struct {
	// ID is the provider resource identifier.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// NativeID is the provider-assigned instance identity. A resource
	// recreated under the same ID has a different NativeID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	NativeID string `json:"nativeID"`
}

// RequirementOperator is a label requirement operator.
// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist;Gt;Lt
type RequirementOperator string

const (
	// RequirementOpIn requires the label value to be one of the values.
	RequirementOpIn RequirementOperator = "In"

	// RequirementOpNotIn requires the label value to be none of the values.
	RequirementOpNotIn RequirementOperator = "NotIn"

	// RequirementOpExists requires the label to be present.
	RequirementOpExists RequirementOperator = "Exists"

	// RequirementOpDoesNotExist requires the label to be absent.
	RequirementOpDoesNotExist RequirementOperator = "DoesNotExist"

	// RequirementOpGt requires the label value, as an integer, to be greater
	// than the single value.
	RequirementOpGt RequirementOperator = "Gt"

	// RequirementOpLt requires the label value, as an integer, to be less than
	// the single value.
	RequirementOpLt RequirementOperator = "Lt"
)

// MachineAllocationRequirement constrains one offering label.
type MachineAllocationRequirement struct {
	// Key is the label key.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	// Operator is the requirement operator.
	// +kubebuilder:validation:Required
	Operator RequirementOperator `json:"operator"`

	// Values are the operator values.
	// +optional
	Values []string `json:"values,omitempty"`

	// MinValues is the consumer's flexibility floor: at least this many
	// distinct values of Key must remain among the current offerings that
	// satisfy every requirement, or the allocation fails with
	// InsufficientCapacity. The allocation then resolves to one of them.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinValues *int32 `json:"minValues,omitempty"`
}

// MachineAllocationNodeTemplate is applied to the Node at registration.
type MachineAllocationNodeTemplate struct {
	// Labels are added to the Node at registration.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Taints are added to the Node at registration.
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`

	// StartupTaints are added at registration and removed by their owners
	// once the Node is initialized.
	// +optional
	StartupTaints []corev1.Taint `json:"startupTaints,omitempty"`
}

// MachineAllocationFailureReason is a normalized terminal launch failure.
// +kubebuilder:validation:Enum=InsufficientCapacity;QuotaExceeded;InvalidRequest;AuthFailed;ProviderConflict;ProviderFailed
type MachineAllocationFailureReason string

const (
	// FailureInsufficientCapacity means no offering could satisfy the request.
	FailureInsufficientCapacity MachineAllocationFailureReason = "InsufficientCapacity"

	// FailureQuotaExceeded means the provider refused the launch for quota.
	FailureQuotaExceeded MachineAllocationFailureReason = "QuotaExceeded"

	// FailureInvalidRequest means the allocation or its class cannot be
	// satisfied as written.
	FailureInvalidRequest MachineAllocationFailureReason = "InvalidRequest"

	// FailureAuthFailed means the provider rejected the credential.
	FailureAuthFailed MachineAllocationFailureReason = "AuthFailed"

	// FailureProviderConflict means a provider resource with the planned
	// identity already exists and is not owned by this allocation.
	FailureProviderConflict MachineAllocationFailureReason = "ProviderConflict"

	// FailureProviderFailed means the provider failed for another reason.
	FailureProviderFailed MachineAllocationFailureReason = "ProviderFailed"
)

// MachineAllocationStatus reports the observed allocation state.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.snapshot) || has(self.snapshot)",message="snapshot cannot be removed"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.providerID) || (has(self.providerID) && self.providerID == oldSelf.providerID)",message="providerID is immutable once set"
type MachineAllocationStatus struct {
	// ObservedGeneration is the allocation generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Snapshot is the frozen input for every remote effect made for this
	// allocation. It is set by the allocation controller and cannot change or
	// be removed once set.
	// +optional
	Snapshot *MachineAllocationSnapshot `json:"snapshot,omitempty"`

	// ProviderID is the exact Node.spec.providerID the machine registers with.
	// It cannot change or be removed once set.
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// ProviderResourceID is the provider-native host identifier.
	// +optional
	ProviderResourceID string `json:"providerResourceID,omitempty"`

	// InstanceType is the launched instance type.
	// +optional
	InstanceType string `json:"instanceType,omitempty"`

	// Zone is the launched zone.
	// +optional
	Zone string `json:"zone,omitempty"`

	// CapacityType is the launched purchase model.
	// +optional
	CapacityType CapacityType `json:"capacityType,omitempty"`

	// Image is the launched host image.
	// +optional
	Image string `json:"image,omitempty"`

	// Capacity is the machine capacity.
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`

	// Allocatable is observed from the Node once registered, otherwise
	// capacity minus overhead.
	// +optional
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`

	// Labels are the labels the Node registers with.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// OwnedResources are the provider resources the allocate operation
	// confirmed as created for this allocation, with provider-native identity
	// proving ownership. Release is limited to these resources.
	// +optional
	// +listType=map
	// +listMapKey=id
	OwnedResources []ProviderOwnedResource `json:"ownedResources,omitempty"`

	// MachineRef identifies the Machine created for this allocation.
	// +optional
	MachineRef *UIDReference `json:"machineRef,omitempty"`

	// NodeRef identifies the first Node registered for this allocation.
	// +optional
	NodeRef *UIDReference `json:"nodeRef,omitempty"`

	// Operations identifies the durable allocation operations.
	// +optional
	Operations MachineAllocationOperations `json:"operations,omitempty"`

	// FailureReason is a normalized terminal launch failure category.
	// +optional
	FailureReason MachineAllocationFailureReason `json:"failureReason,omitempty"`

	// FailureMessage describes the terminal launch failure.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachineAllocationOperations identifies allocation MachineOperations.
type MachineAllocationOperations struct {
	// Allocate is the MachineOperation that launches the host.
	// +optional
	Allocate *UIDReference `json:"allocate,omitempty"`

	// Release is the MachineOperation that releases the host.
	// +optional
	Release *UIDReference `json:"release,omitempty"`
}

// MachineAllocationSnapshot is the protected, immutable allocation input.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="snapshot is immutable"
type MachineAllocationSnapshot struct {
	// ClassUID is the class UID the snapshot was taken from.
	// +kubebuilder:validation:Required
	ClassUID types.UID `json:"classUID"`

	// ClassGeneration is the class generation the snapshot was taken from.
	// +kubebuilder:validation:Required
	ClassGeneration int64 `json:"classGeneration"`

	// ClassRevision is the class resolved revision.
	// +kubebuilder:validation:Required
	ClassRevision string `json:"classRevision"`

	// Provider is the provider adapter name.
	// +kubebuilder:validation:Required
	Provider string `json:"provider"`

	// SiteName is the Site the machine joins.
	// +kubebuilder:validation:Required
	SiteName string `json:"siteName"`

	// SiteUID is the exact Site instance frozen before any effect. A Site
	// recreated under the same name has a different UID.
	// +kubebuilder:validation:Required
	SiteUID types.UID `json:"siteUID"`

	// Credential is the exact MachineOperationCredential instance frozen
	// before any effect. A credential recreated under the same name has a
	// different UID.
	// +kubebuilder:validation:Required
	Credential UIDReference `json:"credential"`

	// Offering is the selected offering.
	// +kubebuilder:validation:Required
	Offering MachineClassOffering `json:"offering"`

	// ProviderConfig is the exact provider config content.
	// +kubebuilder:validation:Required
	ProviderConfig ObjectContentSnapshot `json:"providerConfig"`

	// BootstrapFiles is the exact bootstrap files content.
	// +optional
	BootstrapFiles *ObjectContentSnapshot `json:"bootstrapFiles,omitempty"`

	// AgentArtifact is the digest-pinned agent tarball.
	// +optional
	AgentArtifact *AgentArtifact `json:"agentArtifact,omitempty"`

	// Configuration identifies the configuration version content.
	// +kubebuilder:validation:Required
	Configuration ConfigurationContentSnapshot `json:"configuration"`

	// ConfigurationTemplate is the frozen configuration content for Machines
	// of this allocation, copied from the configuration version.
	// +kubebuilder:validation:Required
	ConfigurationTemplate MachineConfigurationTemplate `json:"configurationTemplate"`

	// HostImage is the provider-interpreted host image.
	// +kubebuilder:validation:Required
	HostImage string `json:"hostImage"`

	// HostProviderID is the provider-specific host ID planned before the
	// host exists. It becomes the Machine's host.external.providerID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	HostProviderID string `json:"hostProviderID"`
}
