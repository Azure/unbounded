// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:XValidation:rule="(has(self.selector.matchLabels) && size(self.selector.matchLabels) > 0) || (has(self.selector.matchExpressions) && size(self.selector.matchExpressions) > 0)",message="selector must contain at least one matchLabels or matchExpressions term"
type ParticipantPolicy struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	Count int32 `json:"count"`

	Selector metav1.LabelSelector `json:"selector"`
}

type CertificationSuiteReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

type ProfileParticipantRole struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	Count int32 `json:"count"`
	// Selector narrows the Profile participant selector for this role.
	Selector metav1.LabelSelector `json:"selector,omitempty"`
}

type StorageEnvironmentBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	StorageClassName string `json:"storageClassName"`
	// Capacity uses Kubernetes quantity syntax.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Capacity string `json:"capacity"`
	// +kubebuilder:validation:MaxItems=4
	// +listType=set
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	MountPath  string `json:"mountPath"`
	Disposable bool   `json:"disposable,omitempty"`
}

type NetworkEnvironmentBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InterfaceClass string `json:"interfaceClass"`
	// +kubebuilder:validation:MaxLength=253
	AttachmentDefinition string `json:"attachmentDefinition,omitempty"`
}

type NamedEnvironmentReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Reference string `json:"reference"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.resources.limits) || size(self.resources.limits) <= 16",message="environment resource limits must not exceed 16 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resources.requests) || size(self.resources.requests) <= 16",message="environment resource requests must not exceed 16 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resources.claims) || size(self.resources.claims) <= 16",message="environment resource claims must not exceed 16 entries"
type CertificationEnvironmentBindings struct {
	// Namespace is the approved execution namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MaxLength=253
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Storage []StorageEnvironmentBinding `json:"storage,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Networks []NetworkEnvironmentBinding `json:"networks,omitempty"`
	// SecretRefs contain names only. Secret values must never enter Profile,
	// Run, plan digest, or evidence objects.
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	SecretRefs []corev1.LocalObjectReference `json:"secretRefs,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Resolvers []NamedEnvironmentReference `json:"resolvers,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	ModelCaches []NamedEnvironmentReference `json:"modelCaches,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	ObjectStores []NamedEnvironmentReference `json:"objectStores,omitempty"`
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.participantRoles) || size(self.participantRoles) == 0 || self.participantRoles.map(x, x.count).sum() == self.participants.count",message="participant role counts must equal participants.count"
type CertificationProfileSpec struct {
	Participants ParticipantPolicy           `json:"participants"`
	Suite        CertificationSuiteReference `json:"suite"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=role
	ParticipantRoles []ProfileParticipantRole          `json:"participantRoles,omitempty"`
	Environment      *CertificationEnvironmentBindings `json:"environment,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certprofile
// +kubebuilder:printcolumn:name="Suite",type=string,JSONPath=`.spec.suite.name`
// +kubebuilder:printcolumn:name="Participants",type=integer,JSONPath=`.spec.participants.count`
type CertificationProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CertificationProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type CertificationProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationProfile `json:"items"`
}
