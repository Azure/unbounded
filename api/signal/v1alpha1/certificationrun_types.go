// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type RunPhase string

const (
	RunPhasePending      RunPhase = "Pending"
	RunPhaseRunning      RunPhase = "Running"
	RunPhaseSucceeded    RunPhase = "Succeeded"
	RunPhaseFailed       RunPhase = "Failed"
	RunPhaseInconclusive RunPhase = "Inconclusive"
	RunPhaseCancelled    RunPhase = "Canceled"
)

func (p RunPhase) Terminal() bool {
	switch p {
	case RunPhaseSucceeded, RunPhaseFailed, RunPhaseInconclusive, RunPhaseCancelled:
		return true
	default:
		return false
	}
}

type Participant struct {
	NodeName string    `json:"nodeName"`
	NodeUID  types.UID `json:"nodeUID"`
}

type PinnedProfile struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type ScriptSnapshot struct {
	ConfigMapName string `json:"configMapName"`
	Key           string `json:"key"`
	Digest        string `json:"digest"`
}

type CertificationRunSpec struct {
	Profile      PinnedProfile     `json:"profile"`
	Participants []Participant     `json:"participants"`
	Script       ScriptSnapshot    `json:"script"`
	Workflow     WorkflowReference `json:"workflow"`
	Backend      ExecutionBackend  `json:"backend"`
	RunnerImage  string            `json:"runnerImage"`
	// +kubebuilder:validation:Enum=FullCertification;QuickCertification;Preflight;TargetedVerification;Diagnostic
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="purpose is immutable"
	Purpose CertificationPurpose `json:"purpose,omitempty"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="stagePlan is immutable"
	StagePlan       []CertificationStage `json:"stagePlan,omitempty"`
	Parameters      map[string]string    `json:"parameters,omitempty"`
	EvidencePath    string               `json:"evidencePath"`
	Deadline        metav1.Duration      `json:"deadline"`
	MaxAttempts     int32                `json:"maxAttempts"`
	Tolerations     []corev1.Toleration  `json:"tolerations,omitempty"`
	RequestNodeName string               `json:"requestNodeName"`
	Cancel          bool                 `json:"cancel,omitempty"`
}

type WorkflowStatus struct {
	Name    string `json:"name,omitempty"`
	UID     string `json:"uid,omitempty"`
	Attempt int32  `json:"attempt,omitempty"`
	Phase   string `json:"phase,omitempty"`
}

type EvidenceStatus struct {
	URI    string `json:"uri,omitempty"`
	Digest string `json:"digest,omitempty"`
}

type CleanupStatus struct {
	ReservationsReleased bool `json:"reservationsReleased,omitempty"`
	SnapshotDeleted      bool `json:"snapshotDeleted,omitempty"`
}

type CertificationRunStatus struct {
	Phase          RunPhase           `json:"phase,omitempty"`
	ExecutionState string             `json:"executionState,omitempty"`
	Verdict        string             `json:"verdict,omitempty"`
	Reason         string             `json:"reason,omitempty"`
	Workflow       WorkflowStatus     `json:"workflow,omitempty"`
	Evidence       EvidenceStatus     `json:"evidence,omitempty"`
	Cleanup        CleanupStatus      `json:"cleanup,omitempty"`
	StartedAt      *metav1.Time       `json:"startedAt,omitempty"`
	FinishedAt     *metav1.Time       `json:"finishedAt,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=certrun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Verdict",type=string,JSONPath=`.status.verdict`
// +kubebuilder:printcolumn:name="Workflow",type=string,JSONPath=`.status.workflow.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CertificationRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertificationRunSpec   `json:"spec"`
	Status CertificationRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CertificationRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationRun `json:"items"`
}
