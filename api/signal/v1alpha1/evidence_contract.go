// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:validation:Enum=Requester;Participant
type ParticipantRole string

const (
	ParticipantRoleRequester   ParticipantRole = "Requester"
	ParticipantRoleParticipant ParticipantRole = "Participant"
)

// +kubebuilder:validation:Enum=Pass;Fail;Inconclusive
type QualificationVerdict string

const (
	QualificationVerdictPass         QualificationVerdict = "Pass"
	QualificationVerdictFail         QualificationVerdict = "Fail"
	QualificationVerdictInconclusive QualificationVerdict = "Inconclusive"
)

// +kubebuilder:validation:XValidation:rule="size(self.nodeUID) > 0",message="nodeUID is required"
type NodeFingerprint struct {
	NodeUID types.UID `json:"nodeUID"`
	// +kubebuilder:validation:MaxLength=128
	BootID string `json:"bootID,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	KernelVersion string `json:"kernelVersion,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	OSImage string `json:"osImage,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	KubeletVersion string `json:"kubeletVersion,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	SystemUUID string `json:"systemUUID,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	DriverVersion string `json:"driverVersion,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	GPUInventoryDigest string `json:"gpuInventoryDigest,omitempty"`
}

type ParticipantEvidenceIdentity struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	Index   int32                `json:"index"`
	Role    ParticipantRole      `json:"role"`
	Verdict QualificationVerdict `json:"verdict"`
	Start   NodeFingerprint      `json:"start"`
	End     NodeFingerprint      `json:"end"`
}

type CheckContinuitySummary struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	Order      int32       `json:"order"`
	StartedAt  metav1.Time `json:"startedAt"`
	FinishedAt metav1.Time `json:"finishedAt"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="observedDuration must not be negative"
	ObservedDuration metav1.Duration `json:"observedDuration"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="maxSampleGap must not be negative"
	MaxSampleGap      *metav1.Duration `json:"maxSampleGap,omitempty"`
	Completed         bool             `json:"completed"`
	SameBootID        bool             `json:"sameBootID,omitempty"`
	PerDeviceCoverage bool             `json:"perDeviceCoverage,omitempty"`
}

type EvidenceProvenance struct {
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage"`
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	AdapterImage string `json:"adapterImage"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	WorkflowTemplateDigest string `json:"workflowTemplateDigest"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	RunnerImages []string `json:"runnerImages"`
}

type QualificationEvidenceIdentity struct {
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Participants []ParticipantEvidenceIdentity `json:"participants"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks     []CheckContinuitySummary `json:"checks"`
	Provenance EvidenceProvenance       `json:"provenance"`
}
