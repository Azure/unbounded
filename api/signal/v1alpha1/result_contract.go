// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const InvocationResultSchemaV1 = "signal.unbounded-cloud.io/invocation-result/v1"

// +kubebuilder:validation:Enum=BooleanEquals;StatusEquals;Minimum;Maximum;Range;CountMinimum;ErrorRateMaximum;PercentileMaximum
type AssertionOperator string

const (
	AssertionOperatorBooleanEquals     AssertionOperator = "BooleanEquals"
	AssertionOperatorStatusEquals      AssertionOperator = "StatusEquals"
	AssertionOperatorMinimum           AssertionOperator = "Minimum"
	AssertionOperatorMaximum           AssertionOperator = "Maximum"
	AssertionOperatorRange             AssertionOperator = "Range"
	AssertionOperatorCountMinimum      AssertionOperator = "CountMinimum"
	AssertionOperatorErrorRateMaximum  AssertionOperator = "ErrorRateMaximum"
	AssertionOperatorPercentileMaximum AssertionOperator = "PercentileMaximum"
)

// +kubebuilder:validation:XValidation:rule="self.operator != 'Range' || (has(self.minimum) && has(self.maximum) && !has(self.expected))",message="Range assertions require minimum and maximum and forbid expected"
// +kubebuilder:validation:XValidation:rule="self.operator == 'Range' || !has(self.minimum) || !has(self.expected)",message="minimum and expected are mutually exclusive outside Range"
// +kubebuilder:validation:XValidation:rule="self.operator == 'Range' || !has(self.maximum) || !has(self.expected)",message="maximum and expected are mutually exclusive outside Range"
// +kubebuilder:validation:XValidation:rule="self.operator == 'Range' || has(self.expected)",message="non-Range assertions require expected"
// +kubebuilder:validation:XValidation:rule="self.operator != 'PercentileMaximum' || has(self.percentile)",message="PercentileMaximum assertions require percentile"
// +kubebuilder:validation:XValidation:rule="self.operator == 'PercentileMaximum' || !has(self.percentile)",message="percentile is valid only for PercentileMaximum assertions"
type TestAssertion struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Metric names must exist in the referenced CertificationTest metric schema.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Metric   string            `json:"metric"`
	Operator AssertionOperator `json:"operator"`
	Expected *TypedValue       `json:"expected,omitempty"`
	Minimum  *TypedValue       `json:"minimum,omitempty"`
	Maximum  *TypedValue       `json:"maximum,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	Percentile *int32 `json:"percentile,omitempty"`
}

// +kubebuilder:validation:Enum=Passed;Failed;Inconclusive;Canceled
type InvocationOutcome string

const (
	InvocationOutcomePassed       InvocationOutcome = "Passed"
	InvocationOutcomeFailed       InvocationOutcome = "Failed"
	InvocationOutcomeInconclusive InvocationOutcome = "Inconclusive"
	InvocationOutcomeCanceled     InvocationOutcome = "Canceled"
)

// +kubebuilder:validation:Enum=Completed;AssertionFailed;InfrastructureFailure;InvalidResult;IdentityMismatch;EvidenceIntegrityFailure;Timeout;Canceled;TestHarnessFailure
type ResultReasonCode string

const (
	ResultReasonCompleted                ResultReasonCode = "Completed"
	ResultReasonAssertionFailed          ResultReasonCode = "AssertionFailed"
	ResultReasonInfrastructureFailure    ResultReasonCode = "InfrastructureFailure"
	ResultReasonInvalidResult            ResultReasonCode = "InvalidResult"
	ResultReasonIdentityMismatch         ResultReasonCode = "IdentityMismatch"
	ResultReasonEvidenceIntegrityFailure ResultReasonCode = "EvidenceIntegrityFailure"
	ResultReasonTimeout                  ResultReasonCode = "Timeout"
	ResultReasonCanceled                 ResultReasonCode = "Canceled"
	ResultReasonTestHarnessFailure       ResultReasonCode = "TestHarnessFailure"
)

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type TestIdentitySnapshot struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string                      `json:"contentDigest"`
	Trust         CertificationTestTrustState `json:"trust"`
}

type MetricMeasurement struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name  string     `json:"name"`
	Value TypedValue `json:"value"`
	// +kubebuilder:validation:MaxLength=32
	Unit string `json:"unit,omitempty"`
}

type ResultArtifactReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=2048
	URI string `json:"uri"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

type InvocationParticipantResult struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=7
	Index int32 `json:"index"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Role    string            `json:"role"`
	Outcome InvocationOutcome `json:"outcome"`
	Start   NodeFingerprint   `json:"start"`
	End     NodeFingerprint   `json:"end"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=128
	VisibleDevices []string `json:"visibleDevices,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=128
	CoveredDevices []string `json:"coveredDevices,omitempty"`
}

type WorkloadActivitySummary struct {
	// SampleCount counts workload-produced samples, not Kubernetes API polls.
	// +kubebuilder:validation:Minimum=0
	SampleCount int32 `json:"sampleCount"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="maxSampleGap must not be negative"
	MaxSampleGap *metav1.Duration `json:"maxSampleGap,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SampleSequenceDigest string `json:"sampleSequenceDigest,omitempty"`
}

// +kubebuilder:validation:Enum=NotRequired;Succeeded;Failed
type CleanupOutcome string

const (
	CleanupOutcomeNotRequired CleanupOutcome = "NotRequired"
	CleanupOutcomeSucceeded   CleanupOutcome = "Succeeded"
	CleanupOutcomeFailed      CleanupOutcome = "Failed"
)

type InvocationCleanupResult struct {
	Outcome CleanupOutcome `json:"outcome"`
	// +kubebuilder:validation:MaxLength=256
	Message string `json:"message,omitempty"`
}

// InvocationResultEnvelope is the canonical durable result emitted once per
// runner-owned invocation. CR status stores only bounded references and
// summaries; the complete envelope remains in immutable durable evidence.
// +kubebuilder:validation:XValidation:rule="size(self.runUID) > 0",message="runUID is required"
// +kubebuilder:validation:XValidation:rule="self.finishedAt >= self.startedAt",message="finishedAt must not precede startedAt"
// +kubebuilder:validation:XValidation:rule="self.cleanup.outcome != 'Failed' || self.outcome != 'Passed'",message="an invocation with failed cleanup cannot pass"
type InvocationResultEnvelope struct {
	// +kubebuilder:validation:Enum=signal.unbounded-cloud.io/invocation-result/v1
	SchemaVersion string `json:"schemaVersion"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	InvocationID string    `json:"invocationID"`
	RunUID       types.UID `json:"runUID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	StageID string `json:"stageID"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	StageOrder int32 `json:"stageOrder"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string               `json:"planDigest"`
	Test       TestIdentitySnapshot `json:"test"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	ResolvedInputs []ResolvedInputIdentity `json:"resolvedInputs,omitempty"`
	StartedAt      metav1.Time             `json:"startedAt"`
	FinishedAt     metav1.Time             `json:"finishedAt"`
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s')",message="monotonicDuration must not be negative"
	MonotonicDuration metav1.Duration   `json:"monotonicDuration"`
	Outcome           InvocationOutcome `json:"outcome"`
	ReasonCode        ResultReasonCode  `json:"reasonCode"`
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
	// +kubebuilder:validation:Minimum=0
	ExitCode *int32 `json:"exitCode,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Measurements []MetricMeasurement `json:"measurements,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=index
	Participants []InvocationParticipantResult `json:"participants"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Artifacts []ResultArtifactReference `json:"artifacts,omitempty"`
	Activity  WorkloadActivitySummary   `json:"activity,omitempty"`
	Cleanup   InvocationCleanupResult   `json:"cleanup"`
}
