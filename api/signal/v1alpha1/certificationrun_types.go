// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Inconclusive;Canceled
type RunPhase string

const (
	ReservationNodeUIDLabel = "signal.unbounded-cloud.io/reserved-node-uid"
	RunConditionCordoned    = "Cordoned"
	RunConditionCleanup     = "WorkloadCleanupComplete"
	RunConditionReleased    = "Released"

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
	// Index is derived from the participant's position and is never serialized.
	Index       int32               `json:"-"`
	NodeName    string              `json:"nodeName"`
	NodeUID     types.UID           `json:"nodeUID"`
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`
	Role        string              `json:"role,omitempty"`
	// +kubebuilder:validation:MaxProperties=16
	TopologyValues map[string]string `json:"topologyValues,omitempty"`
}

type BackendResourceReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	ParticipantIndexes []int32 `json:"participantIndexes"`
}

type PinnedProfile struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type ScriptBundleReference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type ScriptSnapshot struct {
	Key string `json:"key"`
	// Digest is the algorithm-qualified SHA-256 digest of the selected script bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.scriptEnvironment) || self.scriptEnvironment.all(k, size(k) <= 253 && size(self.scriptEnvironment[k]) <= 4096)",message="scriptEnvironment names must not exceed 253 characters and values must not exceed 4096 characters"
type CertificationCheckSnapshot struct {
	// ID and Order are derived from the check's position and are never serialized.
	ID             CheckID          `json:"-"`
	Order          int32            `json:"-"`
	Script         ScriptSnapshot   `json:"script"`
	RunnerImage    string           `json:"runnerImage"`
	Backend        ExecutionBackend `json:"backend"`
	WorkloadPolicy WorkloadPolicy   `json:"workloadPolicy,omitempty"`
	GPUCount       int32            `json:"gpuCount,omitempty"`
	// +kubebuilder:validation:MaxProperties=32
	ScriptEnvironment     map[string]string      `json:"scriptEnvironment,omitempty"`
	BackendActiveDeadline metav1.Duration        `json:"backendActiveDeadline"`
	ParticipantPolicy     CheckParticipantPolicy `json:"participantPolicy,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants == oldSelf.participants",message="participants are immutable"
// +kubebuilder:validation:XValidation:rule="self.scriptBundle == oldSelf.scriptBundle",message="scriptBundle is immutable"
// +kubebuilder:validation:XValidation:rule="self.checks == oldSelf.checks",message="checks are immutable"
// +kubebuilder:validation:XValidation:rule="self.deadline == oldSelf.deadline",message="deadline is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancel) || !oldSelf.cancel || (has(self.cancel) && self.cancel)",message="cancel cannot be cleared"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.release) || !oldSelf.release || (has(self.release) && self.release)",message="release cannot be cleared"
// +kubebuilder:validation:XValidation:rule="self.checks.all(c, duration(self.deadline) - duration(c.backendActiveDeadline) >= duration('60s'))",message="every backendActiveDeadline must leave at least 60 seconds before deadline"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, duration(c.backendActiveDeadline) + duration('60s')).sum() <= duration(self.deadline)",message="deadline must cover every sequential backendActiveDeadline plus 60 seconds of evidence time per check"
type CertificationRunSpec struct {
	Profile PinnedProfile `json:"profile"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Participants []Participant         `json:"participants"`
	ScriptBundle ScriptBundleReference `json:"scriptBundle"`
	// +kubebuilder:validation:MinItems=15
	// +kubebuilder:validation:MaxItems=15
	Checks   []CertificationCheckSnapshot `json:"checks"`
	Deadline metav1.Duration              `json:"deadline"`
	Cancel   bool                         `json:"cancel,omitempty"`
	Release  bool                         `json:"release,omitempty"`
}

type WorkflowStatus struct {
	Name  string `json:"name,omitempty"`
	UID   string `json:"uid,omitempty"`
	Phase string `json:"phase,omitempty"`
}

type EvidenceStatus struct {
	URI string `json:"uri,omitempty"`
	// Digest is the algorithm-qualified SHA-256 digest of the evidence bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Inconclusive;Skipped
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
	Complete bool `json:"complete,omitempty"`
	// Digest identifies the complete backend-evidence v1 document retained in
	// the durable evidence artifact.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
	// Content is transient input for the durable evidence artifact.
	Content string `json:"-"`
}

type CheckCleanupStatus struct {
	Complete bool   `json:"-"`
	Reason   string `json:"-"`
}

type CheckStatus struct {
	// Internal fields are derived or transient and are never persisted in CR status.
	ID               CheckID    `json:"-"`
	Order            int32      `json:"-"`
	WorkflowNodeName string     `json:"-"`
	Phase            CheckPhase `json:"phase"`
	Verdict          string     `json:"-"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=16384
	Measurements string `json:"measurements,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	BackendResources []BackendResourceReference `json:"backendResources,omitempty"`
	BackendEvidence  BackendEvidenceStatus      `json:"backendEvidence,omitempty"`
	StartedAt        *metav1.Time               `json:"startedAt,omitempty"`
	FinishedAt       *metav1.Time               `json:"finishedAt,omitempty"`
	Cleanup          CheckCleanupStatus         `json:"-"`
}

type CertificationRunStatus struct {
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	Phase              RunPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason   string         `json:"reason,omitempty"`
	Workflow WorkflowStatus `json:"workflow,omitempty"`
	Evidence EvidenceStatus `json:"evidence,omitempty"`
	// +kubebuilder:validation:MaxItems=15
	Checks     []CheckStatus `json:"checks,omitempty"`
	StartedAt  *metav1.Time  `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time  `json:"finishedAt,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=certrun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
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
