// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
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

// +kubebuilder:validation:XValidation:rule="size(self.nodeUID) > 0",message="nodeUID is required"
type Participant struct {
	// Index is derived from the participant's position and is never serialized.
	Index int32 `json:"-"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	NodeName string    `json:"nodeName"`
	NodeUID  types.UID `json:"nodeUID"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type BackendResourceReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=0
	// +kubebuilder:validation:items:Maximum=7
	// +listType=set
	ParticipantIndexes []int32 `json:"participantIndexes"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type PinnedProfile struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type PinnedSuite struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type ScriptBundleReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// Digest is the aggregate algorithm-qualified SHA-256 digest derived from
	// the selected script bytes in check order.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

type ScriptSnapshot struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
	// Digest is the algorithm-qualified SHA-256 digest of the selected script bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="self.backend != 'MPIJob' || (has(self.gpuCount) && self.gpuCount > 0)",message="MPIJob checks require a positive gpuCount"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || duration(self.minDuration) > duration('0s')",message="minDuration must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || duration(self.minDuration) <= duration(self.backendActiveDeadline)",message="minDuration must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.backendActiveDeadline)",message="maxSampleGap must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || !has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.minDuration)",message="maxSampleGap must not exceed minDuration"
type CertificationCheckSnapshot struct {
	// ID and Order are derived from the check's position and are never serialized.
	ID     CheckID        `json:"-"`
	Order  int32          `json:"-"`
	Script ScriptSnapshot `json:"script"`
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	RunnerImage    string           `json:"runnerImage"`
	Backend        ExecutionBackend `json:"backend"`
	WorkloadPolicy WorkloadPolicy   `json:"workloadPolicy,omitempty"`
	// +kubebuilder:validation:Minimum=0
	GPUCount              int32             `json:"gpuCount,omitempty"`
	BackendActiveDeadline metav1.Duration   `json:"backendActiveDeadline"`
	MinDuration           *metav1.Duration  `json:"minDuration,omitempty"`
	Continuity            *ContinuityPolicy `json:"continuity,omitempty"`
}

type ExecutionPlanProvenance struct {
	// AdapterImage is the immutable adapter image included in the execution plan.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	AdapterImage string `json:"adapterImage"`
	// WorkflowTemplateDigest identifies the rendered workflow/template semantics.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	WorkflowTemplateDigest string `json:"workflowTemplateDigest"`
}

// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.suite == oldSelf.suite",message="suite is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants == oldSelf.participants",message="participants are immutable"
// +kubebuilder:validation:XValidation:rule="self.scriptBundle == oldSelf.scriptBundle",message="scriptBundle is immutable"
// +kubebuilder:validation:XValidation:rule="self.checks == oldSelf.checks",message="checks are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.planDigest) == has(oldSelf.planDigest) && (!has(self.planDigest) || self.planDigest == oldSelf.planDigest)",message="planDigest is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.planProvenance) == has(oldSelf.planProvenance) && (!has(self.planProvenance) || self.planProvenance == oldSelf.planProvenance)",message="planProvenance is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants.all(x, self.participants.filter(y, y.nodeName == x.nodeName).size() == 1)",message="participants must use unique nodeName values"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancel) || !oldSelf.cancel || (has(self.cancel) && self.cancel)",message="cancel cannot be cleared"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.release) || !oldSelf.release || (has(self.release) && self.release)",message="release cannot be cleared"
type CertificationRunSpec struct {
	Profile PinnedProfile `json:"profile"`
	Suite   PinnedSuite   `json:"suite"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Participants []Participant         `json:"participants"`
	ScriptBundle ScriptBundleReference `json:"scriptBundle"`
	// Checks is the immutable ordered snapshot of the Suite. A check's position
	// defines its stable ID.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks []CertificationCheckSnapshot `json:"checks"`
	// PlanDigest is the canonical digest of all execution-affecting inputs.
	// It is optional only for backward decoding of Runs created before this contract.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest,omitempty"`
	// PlanProvenance pins shared execution inputs not carried by individual checks.
	PlanProvenance *ExecutionPlanProvenance `json:"planProvenance,omitempty"`
	Cancel         bool                     `json:"cancel,omitempty"`
	Release        bool                     `json:"release,omitempty"`
}

type WorkflowStatus struct {
	Name  string `json:"name,omitempty"`
	UID   string `json:"uid,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.uri) || oldSelf.uri.size() == 0 || (has(self.uri) && self.uri == oldSelf.uri)",message="evidence URI is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.digest) || oldSelf.digest.size() == 0 || (has(self.digest) && self.digest == oldSelf.digest)",message="evidence digest is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.identity) || (has(self.identity) && self.identity == oldSelf.identity)",message="evidence identity is write-once"
type EvidenceStatus struct {
	// +kubebuilder:validation:MaxLength=2048
	URI string `json:"uri,omitempty"`
	// Digest is the algorithm-qualified SHA-256 digest of the evidence bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
	// Identity contains bounded qualification inputs. Large measurements remain
	// in the durable evidence document addressed by URI and Digest.
	Identity *QualificationEvidenceIdentity `json:"identity,omitempty"`
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
	// Measurements, backend identities, backend evidence, and timestamps are
	// transient inputs to the durable evidence document, not CR status.
	Measurements     string                     `json:"-"`
	BackendResources []BackendResourceReference `json:"-"`
	BackendEvidence  BackendEvidenceStatus      `json:"-"`
	StartedAt        *metav1.Time               `json:"-"`
	FinishedAt       *metav1.Time               `json:"-"`
	Cleanup          CheckCleanupStatus         `json:"-"`
}

type CertificationRunStatus struct {
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	Phase              RunPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason   string         `json:"reason,omitempty"`
	Workflow WorkflowStatus `json:"workflow,omitempty"`
	Evidence EvidenceStatus `json:"evidence,omitempty"`
	// Checks remains positionally aligned with spec.checks.
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks []CheckStatus `json:"checks,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.evidence) || !has(self.status.evidence.identity) || (has(self.spec.planDigest) && self.status.evidence.identity.planDigest == self.spec.planDigest)",message="evidence identity requires and must match spec.planDigest"
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
