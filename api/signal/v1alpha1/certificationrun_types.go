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
	// +kubebuilder:validation:Minimum=0
	Index       int32               `json:"index"`
	NodeName    string              `json:"nodeName"`
	NodeUID     types.UID           `json:"nodeUID"`
	Capacity    corev1.ResourceList `json:"capacity,omitempty"`
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Roles []string `json:"roles,omitempty"`
}

type BackendResourceReference struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Namespace  string    `json:"namespace,omitempty"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	ParticipantIndexes []int32 `json:"participantIndexes,omitempty"`
}

type PinnedProfile struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type ScriptSnapshot struct {
	ConfigMapName        string `json:"configMapName"`
	Key                  string `json:"key"`
	RequiresRuntimeStage bool   `json:"requiresRuntimeStage,omitempty"`
	// Digest is the algorithm-qualified SHA-256 digest of the selected script bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

type LegacyScriptSnapshot struct {
	ConfigMapName        string `json:"configMapName,omitempty"`
	Key                  string `json:"key,omitempty"`
	RequiresRuntimeStage bool   `json:"requiresRuntimeStage,omitempty"`
	Digest               string `json:"digest,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.maxAttempts == 1",message="maxAttempts must be exactly 1"
// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.script.requiresRuntimeStage) || !self.script.requiresRuntimeStage || has(self.runtimeStage)",message="runtimeStage is required when the script requires a runtime stage"
type CertificationCheckSnapshot struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^R(0[1-9]|1[0-5])$`
	ID CheckID `json:"id"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=14
	Order                 int32                  `json:"order"`
	Script                ScriptSnapshot         `json:"script"`
	RunnerImage           string                 `json:"runnerImage"`
	Backend               ExecutionBackend       `json:"backend"`
	WorkloadPolicy        WorkloadPolicy         `json:"workloadPolicy,omitempty"`
	RuntimeStage          *RuntimeStage          `json:"runtimeStage,omitempty"`
	Parameters            map[string]string      `json:"parameters,omitempty"`
	BackendActiveDeadline metav1.Duration        `json:"backendActiveDeadline"`
	ResultBounds          ResultBounds           `json:"resultBounds"`
	ParticipantPolicy     CheckParticipantPolicy `json:"participantPolicy,omitempty"`
	MaxAttempts           int32                  `json:"maxAttempts"`
}

// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants == oldSelf.participants",message="participants are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.participantRoles) == has(oldSelf.participantRoles) && (!has(self.participantRoles) || self.participantRoles == oldSelf.participantRoles)",message="participantRoles are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.topologyConstraints) == has(oldSelf.topologyConstraints) && (!has(self.topologyConstraints) || self.topologyConstraints == oldSelf.topologyConstraints)",message="topologyConstraints are immutable"
// +kubebuilder:validation:XValidation:rule="self.checks == oldSelf.checks",message="checks are immutable"
// +kubebuilder:validation:XValidation:rule="self.executionPolicy == oldSelf.executionPolicy",message="executionPolicy is immutable"
// +kubebuilder:validation:XValidation:rule="self.workflow == oldSelf.workflow",message="workflow is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.purpose) == has(oldSelf.purpose) && (!has(self.purpose) || self.purpose == oldSelf.purpose)",message="purpose is immutable"
// +kubebuilder:validation:XValidation:rule="self.evidencePath == oldSelf.evidencePath",message="evidencePath is immutable"
// +kubebuilder:validation:XValidation:rule="self.deadline == oldSelf.deadline",message="deadline is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.tolerations) == has(oldSelf.tolerations) && (!has(self.tolerations) || self.tolerations == oldSelf.tolerations)",message="tolerations are immutable"
// +kubebuilder:validation:XValidation:rule="self.requestNodeName == oldSelf.requestNodeName",message="requestNodeName is immutable"
// +kubebuilder:validation:XValidation:rule="self.checks.all(c, self.checks.exists_one(x, x.id == c.id))",message="checks must contain unique IDs"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, c.order) == [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14]",message="checks must use immutable contiguous order 0 through 14"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, c.id) == ['R01', 'R02', 'R03', 'R04', 'R05', 'R06', 'R07', 'R08', 'R09', 'R10', 'R11', 'R12', 'R13', 'R14', 'R15']",message="checks must use ordered IDs R01 through R15"
// +kubebuilder:validation:XValidation:rule="self.checks.all(c, duration(self.deadline) - duration(c.backendActiveDeadline) >= duration('60s'))",message="every backendActiveDeadline must leave at least 60 seconds before deadline"
type CertificationRunSpec struct {
	Profile PinnedProfile `json:"profile"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Participants []Participant `json:"participants"`
	// +kubebuilder:validation:MaxItems=16
	ParticipantRoles []ParticipantRoleConstraint `json:"participantRoles,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	TopologyConstraints []ParticipantTopologyConstraint `json:"topologyConstraints,omitempty"`
	// +kubebuilder:validation:MinItems=15
	// +kubebuilder:validation:MaxItems=15
	Checks          []CertificationCheckSnapshot `json:"checks"`
	ExecutionPolicy SuiteExecutionPolicy         `json:"executionPolicy"`
	Workflow        WorkflowReference            `json:"workflow"`
	// +kubebuilder:validation:Enum=FullCertification;QuickCertification;Preflight;TargetedVerification;Diagnostic
	Purpose         CertificationPurpose `json:"purpose,omitempty"`
	EvidencePath    string               `json:"evidencePath"`
	Deadline        metav1.Duration      `json:"deadline"`
	Tolerations     []corev1.Toleration  `json:"tolerations,omitempty"`
	RequestNodeName string               `json:"requestNodeName"`
	Cancel          bool                 `json:"cancel,omitempty"`

	// Deprecated Go-only aliases retained for unit fixtures. They are not part
	// of the v1alpha1 wire contract.
	Script                LegacyScriptSnapshot `json:"-"`
	Backend               ExecutionBackend     `json:"-"`
	RunnerImage           string               `json:"-"`
	WorkloadPolicy        WorkloadPolicy       `json:"-"`
	StagePlan             []CertificationStage `json:"-"`
	RuntimeStage          *RuntimeStage        `json:"-"`
	Parameters            map[string]string    `json:"-"`
	BackendActiveDeadline metav1.Duration      `json:"-"`
	MaxAttempts           int32                `json:"-"`
}

type WorkflowStatus struct {
	Name    string            `json:"name,omitempty"`
	UID     string            `json:"uid,omitempty"`
	Attempt int32             `json:"attempt,omitempty"`
	Phase   string            `json:"phase,omitempty"`
	Outputs map[string]string `json:"outputs,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	BackendResources []BackendResourceReference `json:"backendResources,omitempty"`
}

type EvidenceStatus struct {
	URI string `json:"uri,omitempty"`
	// Digest is the algorithm-qualified SHA-256 digest of the evidence bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
}

type CleanupStatus struct {
	ReservationsReleased bool `json:"reservationsReleased,omitempty"`
	SnapshotDeleted      bool `json:"snapshotDeleted,omitempty"`
}

type CheckPhase string

const (
	CheckPhasePending      CheckPhase = "Pending"
	CheckPhaseRunning      CheckPhase = "Running"
	CheckPhaseSucceeded    CheckPhase = "Succeeded"
	CheckPhaseFailed       CheckPhase = "Failed"
	CheckPhaseInconclusive CheckPhase = "Inconclusive"
	CheckPhaseSkipped      CheckPhase = "Skipped"
)

type BackendEvidenceStatus struct {
	Version  string `json:"version,omitempty"`
	Complete bool   `json:"complete,omitempty"`
	// Digest identifies the complete backend-evidence v1 document retained in
	// the durable evidence artifact.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
	// Content is populated only while constructing the durable evidence
	// artifact and is not persisted in CertificationRun status.
	Content string `json:"content,omitempty"`
}

type CheckCleanupStatus struct {
	Complete bool   `json:"complete,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type CheckStatus struct {
	ID               CheckID                    `json:"id"`
	Order            int32                      `json:"order"`
	WorkflowNodeName string                     `json:"workflowNodeName,omitempty"`
	Phase            CheckPhase                 `json:"phase"`
	Verdict          string                     `json:"verdict,omitempty"`
	Reason           string                     `json:"reason,omitempty"`
	Measurements     string                     `json:"measurements,omitempty"`
	BackendResources []BackendResourceReference `json:"backendResources,omitempty"`
	BackendEvidence  BackendEvidenceStatus      `json:"backendEvidence,omitempty"`
	StartedAt        *metav1.Time               `json:"startedAt,omitempty"`
	FinishedAt       *metav1.Time               `json:"finishedAt,omitempty"`
	Duration         metav1.Duration            `json:"duration,omitempty"`
	Attempt          int32                      `json:"attempt"`
	RetryCount       int32                      `json:"retryCount"`
	Cleanup          CheckCleanupStatus         `json:"cleanup,omitempty"`
	NotRun           bool                       `json:"notRun,omitempty"`
}

type CertificationRunStatus struct {
	Phase          RunPhase       `json:"phase,omitempty"`
	ExecutionState string         `json:"executionState,omitempty"`
	Verdict        string         `json:"verdict,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	Workflow       WorkflowStatus `json:"workflow,omitempty"`
	Evidence       EvidenceStatus `json:"evidence,omitempty"`
	Cleanup        CleanupStatus  `json:"cleanup,omitempty"`
	// +kubebuilder:validation:MaxItems=15
	Checks     []CheckStatus      `json:"checks,omitempty"`
	StartedAt  *metav1.Time       `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time       `json:"finishedAt,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
