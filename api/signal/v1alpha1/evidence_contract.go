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

const MaxDeviceIntervalCoverageEntries = 64

// +kubebuilder:validation:XValidation:rule="self.coveredIntervals <= self.requiredIntervals",message="coveredIntervals must not exceed requiredIntervals"
type DeviceIntervalCoverageSummary struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	ParticipantIndex int32 `json:"participantIndex"`
	// DeviceID is the stable device identity used by the durable evidence.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DeviceID string `json:"deviceID"`
	// +kubebuilder:validation:Minimum=1
	RequiredIntervals int32 `json:"requiredIntervals"`
	// +kubebuilder:validation:Minimum=0
	CoveredIntervals int32 `json:"coveredIntervals"`
}

// +kubebuilder:validation:XValidation:rule="self.finishedAt >= self.startedAt",message="execution window finishedAt must not precede startedAt"
// +kubebuilder:validation:XValidation:rule="self.firstInvocationAt >= self.startedAt && self.firstInvocationAt <= self.finishedAt",message="firstInvocationAt must be inside the execution window"
// +kubebuilder:validation:XValidation:rule="self.lastInvocationAt >= self.firstInvocationAt && self.lastInvocationAt <= self.finishedAt",message="lastInvocationAt must be inside the execution window"
// +kubebuilder:validation:XValidation:rule="self.successfulInvocations + self.timedOutInvocations + self.failedInvocations == self.invocationCount",message="invocation outcome counts must equal invocationCount"
// +kubebuilder:validation:XValidation:rule="self.mode != 'RunOnce' || (self.invocationCount == 1 && !has(self.configuredCoverageWindow) && !has(self.maxStartGap))",message="RunOnce evidence must describe exactly one invocation without repeated-execution fields"
// +kubebuilder:validation:XValidation:rule="self.mode == 'RunOnce' || (has(self.configuredCoverageWindow) && has(self.maxStartGap))",message="Continuous and Periodic evidence requires configuredCoverageWindow and maxStartGap"
type CheckExecutionSummary struct {
	Role CheckStageRole     `json:"role"`
	Mode CheckExecutionMode `json:"mode"`
	// ConfiguredCoverageWindow is copied from the immutable Run snapshot.
	ConfiguredCoverageWindow *metav1.Duration `json:"configuredCoverageWindow,omitempty"`
	StartedAt                metav1.Time      `json:"startedAt"`
	FinishedAt               metav1.Time      `json:"finishedAt"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="observedCoverageWindow must not be negative"
	ObservedCoverageWindow metav1.Duration `json:"observedCoverageWindow"`
	// +kubebuilder:validation:Minimum=1
	InvocationCount int32 `json:"invocationCount"`
	// +kubebuilder:validation:Minimum=0
	SuccessfulInvocations int32 `json:"successfulInvocations"`
	// +kubebuilder:validation:Minimum=0
	TimedOutInvocations int32 `json:"timedOutInvocations"`
	// +kubebuilder:validation:Minimum=0
	FailedInvocations int32       `json:"failedInvocations"`
	FirstInvocationAt metav1.Time `json:"firstInvocationAt"`
	LastInvocationAt  metav1.Time `json:"lastInvocationAt"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="maxStartGap must not be negative"
	MaxStartGap *metav1.Duration `json:"maxStartGap,omitempty"`
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
	// Execution is absent for evidence produced by legacy one-shot Runs.
	Execution *CheckExecutionSummary `json:"execution,omitempty"`
}

// +kubebuilder:validation:Enum=Passed;Failed;Inconclusive;Canceled;Skipped
type StageVerdict string

const (
	StageVerdictPassed       StageVerdict = "Passed"
	StageVerdictFailed       StageVerdict = "Failed"
	StageVerdictInconclusive StageVerdict = "Inconclusive"
	StageVerdictCanceled     StageVerdict = "Canceled"
	StageVerdictSkipped      StageVerdict = "Skipped"
)

// +kubebuilder:validation:XValidation:rule="has(self.actual) || (!self.passed && has(self.message) && size(self.message.trim()) > 0)",message="an assertion without an actual value must fail with a reason"
type AssertionResult struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// Actual is absent only when the metric could not be produced or observed.
	// The immutable Run assertion retains the typed expected value or threshold.
	Actual *TypedValue `json:"actual,omitempty"`
	// Message explains the failure and is required when Actual is absent.
	// +kubebuilder:validation:MaxLength=256
	Message string `json:"message,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.finishedAt >= self.startedAt",message="invocation finishedAt must not precede startedAt"
type InvocationEvidenceReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InvocationID string `json:"invocationID"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest     string            `json:"digest"`
	Outcome    InvocationOutcome `json:"outcome"`
	ReasonCode ResultReasonCode  `json:"reasonCode"`
	StartedAt  metav1.Time       `json:"startedAt"`
	FinishedAt metav1.Time       `json:"finishedAt"`
}

// +kubebuilder:validation:XValidation:rule="self.finishedAt >= self.startedAt",message="stage finishedAt must not precede startedAt"
// +kubebuilder:validation:XValidation:rule="self.successfulInvocations + self.timedOutInvocations + self.failedInvocations + self.inconclusiveInvocations + self.canceledInvocations == self.invocationCount",message="invocation outcome counts must equal invocationCount"
// +kubebuilder:validation:XValidation:rule="!self.qualificationEligible || (self.test.trust == 'Approved' && (!has(self.diagnosticOnly) || !self.diagnosticOnly))",message="qualification-eligible evidence requires an approved test and must not be diagnostic-only"
type StageEvidenceSummary struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=15
	PhaseOrder int32 `json:"phaseOrder"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=63
	StageOrder int32 `json:"stageOrder"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name                     string               `json:"name"`
	Role                     CheckStageRole       `json:"role"`
	Test                     TestIdentitySnapshot `json:"test"`
	Mode                     CheckExecutionMode   `json:"mode"`
	Verdict                  StageVerdict         `json:"verdict"`
	StartedAt                metav1.Time          `json:"startedAt"`
	FinishedAt               metav1.Time          `json:"finishedAt"`
	ConfiguredCoverageWindow *metav1.Duration     `json:"configuredCoverageWindow,omitempty"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="observedCoverageWindow must not be negative"
	ObservedCoverageWindow metav1.Duration `json:"observedCoverageWindow"`
	// +kubebuilder:validation:Minimum=0
	InvocationCount int32 `json:"invocationCount"`
	// +kubebuilder:validation:Minimum=0
	SuccessfulInvocations int32 `json:"successfulInvocations"`
	// +kubebuilder:validation:Minimum=0
	TimedOutInvocations int32 `json:"timedOutInvocations"`
	// +kubebuilder:validation:Minimum=0
	FailedInvocations int32 `json:"failedInvocations"`
	// +kubebuilder:validation:Minimum=0
	InconclusiveInvocations int32 `json:"inconclusiveInvocations"`
	// +kubebuilder:validation:Minimum=0
	CanceledInvocations int32        `json:"canceledInvocations"`
	FirstInvocationAt   *metav1.Time `json:"firstInvocationAt,omitempty"`
	LastInvocationAt    *metav1.Time `json:"lastInvocationAt,omitempty"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="maxStartGap must not be negative"
	MaxStartGap *metav1.Duration `json:"maxStartGap,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=participantIndex
	// +listMapKey=deviceID
	DeviceIntervalCoverage []DeviceIntervalCoverageSummary `json:"deviceIntervalCoverage,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=index
	Participants []ParticipantEvidenceIdentity `json:"participants,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	Assertions []AssertionResult `json:"assertions,omitempty"`
	// Measurements is the bounded typed projection from the last completed
	// invocation. Complete per-invocation values remain in durable evidence.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Measurements []MetricMeasurement `json:"measurements,omitempty"`
	// InvocationEvidence is a bounded index into the durable invocation
	// envelopes. The durable stage artifact remains authoritative.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=invocationID
	InvocationEvidence []InvocationEvidenceReference `json:"invocationEvidence,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	DurableEvidenceDigest string `json:"durableEvidenceDigest"`
	QualificationEligible bool   `json:"qualificationEligible"`
	DiagnosticOnly        bool   `json:"diagnosticOnly,omitempty"`
}

type EvidenceProvenance struct {
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage"`
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	AdapterImage string `json:"adapterImage"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	WorkflowTemplateDigest string `json:"workflowTemplateDigest"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	RunnerImages []string `json:"runnerImages"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	ResolverImages []string `json:"resolverImages,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.stages) && size(self.stages) > 0) != (has(self.checks) && size(self.checks) > 0)",message="exactly one of stages or legacy checks must be present"
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
	Checks []CheckContinuitySummary `json:"checks,omitempty"`
	// Stages contains bounded phase/stage verdict summaries. Complete
	// invocation envelopes remain in durable evidence.
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Stages     []StageEvidenceSummary `json:"stages,omitempty"`
	Provenance EvidenceProvenance     `json:"provenance"`
}
