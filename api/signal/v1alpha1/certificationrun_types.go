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

type ParticipantSnapshot struct {
	NodeName               string    `json:"nodeName"`
	NodeUID                types.UID `json:"nodeUID"`
	ObservedGPUCapacity    string    `json:"observedGPUCapacity"`
	ObservedGPUAllocatable string    `json:"observedGPUAllocatable"`
}

type TestInvocation struct {
	ID            string            `json:"id"`
	NodeName      string            `json:"nodeName"`
	NodeUID       types.UID         `json:"nodeUID"`
	ContainerName string            `json:"containerName,omitempty"`
	Parameters    map[string]string `json:"parameters,omitempty"`
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

// AdmissionSnapshot freezes the membership and guards for explicit-set admission.
// Runs without this field retain their original single-node admission contract.
// +kubebuilder:validation:XValidation:rule="has(self.participants.nodeNames)",message="admission requires explicit nodeNames"
// +kubebuilder:validation:XValidation:rule="!has(self.participants.topologyKey) || (has(self.topologyValue) && size(self.topologyValue) > 0)",message="admission requires the observed topology value"
// +kubebuilder:validation:XValidation:rule="self.trigger != 'recurring' || (has(self.recurring) && self.recurring.enabled && has(self.idleSince))",message="recurring admission requires an enabled pinned policy and idle observation"
type AdmissionSnapshot struct {
	Participants ParticipantPolicy `json:"participants"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`
	TopologyValue string `json:"topologyValue,omitempty"`
	// +kubebuilder:validation:Enum=manual;recurring
	Trigger   string           `json:"trigger"`
	Recurring *RecurringPolicy `json:"recurring,omitempty"`
	// IdleSince is the latest continuous idle observation across all members.
	IdleSince *metav1.Time `json:"idleSince,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants == oldSelf.participants",message="participants are immutable"
// +kubebuilder:validation:XValidation:rule="self.script == oldSelf.script",message="script is immutable"
// +kubebuilder:validation:XValidation:rule="self.workflow == oldSelf.workflow",message="workflow is immutable"
// +kubebuilder:validation:XValidation:rule="self.backend == oldSelf.backend",message="backend is immutable"
// +kubebuilder:validation:XValidation:rule="self.runnerImage == oldSelf.runnerImage",message="runnerImage is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.parameters) == has(oldSelf.parameters) && (!has(self.parameters) || self.parameters == oldSelf.parameters)",message="parameters are immutable"
// +kubebuilder:validation:XValidation:rule="self.evidencePath == oldSelf.evidencePath",message="evidencePath is immutable"
// +kubebuilder:validation:XValidation:rule="self.deadline == oldSelf.deadline",message="deadline is immutable"
// +kubebuilder:validation:XValidation:rule="self.maxAttempts == oldSelf.maxAttempts",message="maxAttempts is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.tolerations) == has(oldSelf.tolerations) && (!has(self.tolerations) || self.tolerations == oldSelf.tolerations)",message="tolerations are immutable"
// +kubebuilder:validation:XValidation:rule="self.requestNodeName == oldSelf.requestNodeName",message="requestNodeName is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.purpose) == has(oldSelf.purpose) && (!has(self.purpose) || self.purpose == oldSelf.purpose)",message="purpose is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.stagePlan) == has(oldSelf.stagePlan) && (!has(self.stagePlan) || self.stagePlan == oldSelf.stagePlan)",message="stagePlan is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.participantPlan) == has(oldSelf.participantPlan) && (!has(self.participantPlan) || self.participantPlan == oldSelf.participantPlan)",message="participantPlan is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.testPlan) == has(oldSelf.testPlan) && (!has(self.testPlan) || self.testPlan == oldSelf.testPlan)",message="testPlan is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.admission) == has(oldSelf.admission) && (!has(self.admission) || self.admission == oldSelf.admission)",message="admission is immutable"
type CertificationRunSpec struct {
	Profile         PinnedProfile         `json:"profile"`
	Participants    []Participant         `json:"participants"`
	Admission       *AdmissionSnapshot    `json:"admission,omitempty"`
	ParticipantPlan []ParticipantSnapshot `json:"participantPlan,omitempty"`
	TestPlan        []TestInvocation      `json:"testPlan,omitempty"`
	Script          ScriptSnapshot        `json:"script"`
	Workflow        WorkflowReference     `json:"workflow"`
	Backend         ExecutionBackend      `json:"backend"`
	RunnerImage     string                `json:"runnerImage"`
	// +kubebuilder:validation:Enum=FullCertification;QuickCertification;Preflight;TargetedVerification;Diagnostic
	Purpose         CertificationPurpose `json:"purpose,omitempty"`
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

type TestResult struct {
	TestInvocation `json:",inline"`

	WorkflowName     string `json:"workflowName,omitempty"`
	WorkflowUID      string `json:"workflowUID,omitempty"`
	WorkflowNodeID   string `json:"workflowNodeID,omitempty"`
	WorkflowNodeName string `json:"workflowNodeName,omitempty"`
	TaskName         string `json:"taskName,omitempty"`
	ContainerNodeID  string `json:"containerNodeID,omitempty"`
	Attempt          int32  `json:"attempt"`
	ScriptDigest     string `json:"scriptDigest"`
	ExecutionState   string `json:"executionState"`
	Verdict          string `json:"verdict"`
	Reason           string `json:"reason"`
	// +kubebuilder:validation:MaxLength=16384
	Measurements string         `json:"measurements,omitempty"`
	Evidence     EvidenceStatus `json:"evidence,omitempty"`
}

type AttemptStatus struct {
	Workflow       WorkflowStatus `json:"workflow"`
	ExecutionState string         `json:"executionState"`
	Verdict        string         `json:"verdict"`
	Reason         string         `json:"reason"`
	FinishedAt     metav1.Time    `json:"finishedAt"`
	Evidence       EvidenceStatus `json:"evidence,omitempty"`
}

type CleanupStatus struct {
	ReservationsReleased bool `json:"reservationsReleased,omitempty"`
	SnapshotDeleted      bool `json:"snapshotDeleted,omitempty"`
}

type CertificationRunStatus struct {
	AdmissionReady bool               `json:"admissionReady,omitempty"`
	Phase          RunPhase           `json:"phase,omitempty"`
	ExecutionState string             `json:"executionState,omitempty"`
	Verdict        string             `json:"verdict,omitempty"`
	Reason         string             `json:"reason,omitempty"`
	Workflow       WorkflowStatus     `json:"workflow,omitempty"`
	Evidence       EvidenceStatus     `json:"evidence,omitempty"`
	TestResults    []TestResult       `json:"testResults,omitempty"`
	Attempts       []AttemptStatus    `json:"attempts,omitempty"`
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
