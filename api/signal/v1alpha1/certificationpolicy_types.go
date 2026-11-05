// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	NodeQualificationLabel = "node-restriction.kubernetes.io/project-signal-qualified"
	NodeQualificationValue = "true"
)

type PolicyProfileReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// UID optionally restricts credit to one Profile identity.
	UID types.UID `json:"uid,omitempty"`
}

// +kubebuilder:validation:Enum=Current;Listed
type AcceptedPlanMode string

const (
	AcceptedPlanModeCurrent AcceptedPlanMode = "Current"
	AcceptedPlanModeListed  AcceptedPlanMode = "Listed"
)

// +kubebuilder:validation:XValidation:rule="self.mode != 'Listed' || (has(self.digests) && size(self.digests) > 0)",message="Listed plan acceptance requires at least one digest"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Listed' || !has(self.digests) || size(self.digests) == 0",message="plan digests are only valid in Listed mode"
type AcceptedPlanPolicy struct {
	Mode AcceptedPlanMode `json:"mode"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^sha256:[0-9a-f]{64}$`
	Digests []string `json:"digests,omitempty"`
}

type QualificationVariation struct {
	// MinDistinctPlanDigests requires qualification credit to span distinct plans.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	MinDistinctPlanDigests int32 `json:"minDistinctPlanDigests,omitempty"`
	// MinRunSeparation is the minimum start-time separation between credited Runs.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="minRunSeparation must be positive"
	MinRunSeparation *metav1.Duration `json:"minRunSeparation,omitempty"`
}

// +kubebuilder:validation:Enum=Requester;AllParticipants;PerParticipant
type QualificationCreditRule string

const (
	QualificationCreditRequester       QualificationCreditRule = "Requester"
	QualificationCreditAllParticipants QualificationCreditRule = "AllParticipants"
	QualificationCreditPerParticipant  QualificationCreditRule = "PerParticipant"
)

type CertificationRequirement struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name    string                 `json:"name"`
	Profile PolicyProfileReference `json:"profile"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	RequiredPassCount int32                   `json:"requiredPassCount"`
	CreditRule        QualificationCreditRule `json:"creditRule"`
	AcceptedPlans     AcceptedPlanPolicy      `json:"acceptedPlans"`
	Variation         *QualificationVariation `json:"variation,omitempty"`
}

// +kubebuilder:validation:Enum=NodeUID;BootID;KernelVersion;OSImage;KubeletVersion;SystemUUID;DriverVersion;GPUInventory
type FingerprintField string

const (
	FingerprintFieldNodeUID      FingerprintField = "NodeUID"
	FingerprintFieldBootID       FingerprintField = "BootID"
	FingerprintFieldKernel       FingerprintField = "KernelVersion"
	FingerprintFieldOSImage      FingerprintField = "OSImage"
	FingerprintFieldKubelet      FingerprintField = "KubeletVersion"
	FingerprintFieldSystemUUID   FingerprintField = "SystemUUID"
	FingerprintFieldDriver       FingerprintField = "DriverVersion"
	FingerprintFieldGPUInventory FingerprintField = "GPUInventory"
)

// +kubebuilder:validation:Enum=Ignore;Reset;Revoke
type NonPassBehavior string

const (
	NonPassBehaviorIgnore NonPassBehavior = "Ignore"
	NonPassBehaviorReset  NonPassBehavior = "Reset"
	NonPassBehaviorRevoke NonPassBehavior = "Revoke"
)

// +kubebuilder:validation:Enum=None;ProtectedLabel
type GateProjectionMode string

const (
	GateProjectionNone           GateProjectionMode = "None"
	GateProjectionProtectedLabel GateProjectionMode = "ProtectedLabel"
)

type GateProjection struct {
	// Mode ProtectedLabel projects only the fixed
	// node-restriction.kubernetes.io/project-signal-qualified=true contract.
	// It requires the Node authorizer and NodeRestriction admission plugin.
	// Controllers must fail closed instead of projecting the label when that
	// protection is unavailable.
	Mode GateProjectionMode `json:"mode"`
}

// +kubebuilder:validation:XValidation:rule="self.invalidationFingerprintFields.exists(x, x == 'NodeUID')",message="invalidationFingerprintFields must include NodeUID"
// +kubebuilder:validation:XValidation:rule="duration(self.validity) > duration('0s')",message="validity must be positive"
// +kubebuilder:validation:XValidation:rule="duration(self.renewBefore) > duration('0s')",message="renewBefore must be positive"
// +kubebuilder:validation:XValidation:rule="duration(self.renewBefore) < duration(self.validity)",message="renewBefore must be less than validity"
type CertificationPolicySpec struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Requirements []CertificationRequirement `json:"requirements"`
	Validity     metav1.Duration            `json:"validity"`
	RenewBefore  metav1.Duration            `json:"renewBefore"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	InvalidationFingerprintFields []FingerprintField `json:"invalidationFingerprintFields"`
	NonPassBehavior               NonPassBehavior    `json:"nonPassBehavior"`
	GateProjection                GateProjection     `json:"gateProjection"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certpolicy
// +kubebuilder:printcolumn:name="Requirements",type=integer,JSONPath=`.spec.requirements.size()`
// +kubebuilder:printcolumn:name="Validity",type=string,JSONPath=`.spec.validity`
// +kubebuilder:printcolumn:name="Gate",type=string,JSONPath=`.spec.gateProjection.mode`
type CertificationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CertificationPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true
type CertificationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationPolicy `json:"items"`
}
