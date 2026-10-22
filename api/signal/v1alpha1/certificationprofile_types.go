// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ScriptReference struct {
	Name                 string `json:"name"`
	Key                  string `json:"key"`
	RequiresRuntimeStage bool   `json:"requiresRuntimeStage,omitempty"`
	// Digest is the approved algorithm-qualified SHA-256 digest of the script bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

type WorkflowReference struct {
	Name string `json:"name"`
}

type CertificationPurpose string

const (
	PurposeFullCertification    CertificationPurpose = "FullCertification"
	PurposeQuickCertification   CertificationPurpose = "QuickCertification"
	PurposePreflight            CertificationPurpose = "Preflight"
	PurposeTargetedVerification CertificationPurpose = "TargetedVerification"
	PurposeDiagnostic           CertificationPurpose = "Diagnostic"
)

type CertificationStage struct {
	ID               string          `json:"id"`
	DisplayName      string          `json:"displayName"`
	Category         string          `json:"category"`
	Description      string          `json:"description,omitempty"`
	ExpectedDuration metav1.Duration `json:"expectedDuration,omitempty"`
	Required         bool            `json:"required"`
	Conditional      bool            `json:"conditional,omitempty"`
}

type CheckID string

type ResultBounds struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4096
	ReasonBytes int32 `json:"reasonBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16384
	MeasurementsBytes int32 `json:"measurementsBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16384
	BackendResourcesBytes int32 `json:"backendResourcesBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65536
	BackendEvidenceBytes int32 `json:"backendEvidenceBytes"`
}

type ParticipantRoleConstraint struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count int32 `json:"count"`
	// Selector further constrains participants assigned to this role.
	Selector metav1.LabelSelector `json:"selector,omitempty"`
}

type ParticipantTopologyConstraint struct {
	// Role names the participant role whose selected Nodes are evaluated.
	Role string `json:"role"`
	// LabelKey is the Node label used to identify topology domains.
	LabelKey string `json:"labelKey"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MinDistinctValues int32 `json:"minDistinctValues"`
}

type CheckParticipantPolicy struct {
	// +kubebuilder:validation:MaxItems=16
	RequiredRoles []string `json:"requiredRoles,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	ExcludedRoles []string `json:"excludedRoles,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.maxAttempts == 1",message="maxAttempts must be exactly 1"
// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.script.requiresRuntimeStage) || !self.script.requiresRuntimeStage || has(self.runtimeStage)",message="runtimeStage is required when the script requires a runtime stage"
type CertificationCheck struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^R(0[1-9]|1[0-5])$`
	ID CheckID `json:"id"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=14
	Order                 int32                  `json:"order"`
	Script                ScriptReference        `json:"script"`
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

type SuiteFailurePolicy string

const (
	SuiteFailurePolicyStop     SuiteFailurePolicy = "Stop"
	SuiteFailurePolicyContinue SuiteFailurePolicy = "Continue"
)

type SuiteExecutionPolicy struct {
	// +kubebuilder:validation:Enum=Stop;Continue
	OnFailure SuiteFailurePolicy `json:"onFailure"`
}

type RuntimeStage struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

type ExecutionBackend struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Queue      string `json:"queue,omitempty"`
}

type ParticipantPolicy struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count int32 `json:"count"`

	Selector metav1.LabelSelector `json:"selector"`

	RequiredResources corev1.ResourceList `json:"requiredResources,omitempty"`
}

type WorkloadVolume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`

	HostPath              *corev1.HostPathVolumeSource              `json:"hostPath,omitempty"`
	EmptyDir              *corev1.EmptyDirVolumeSource              `json:"emptyDir,omitempty"`
	PersistentVolumeClaim *corev1.PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty"`
}

type KueueTopologyPolicy struct {
	// RequiredLevel is the Node label key for the minimum topology domain in which
	// Kueue must place the participant PodSet.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=316
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`
	RequiredLevel string `json:"requiredLevel"`
}

type WorkloadPolicy struct {
	Resources       corev1.ResourceRequirements  `json:"resources,omitempty"`
	SecurityContext *corev1.SecurityContext      `json:"securityContext,omitempty"`
	HostNetwork     bool                         `json:"hostNetwork,omitempty"`
	KueueTopology   *KueueTopologyPolicy         `json:"kueueTopology,omitempty"`
	WorkingDir      string                       `json:"workingDir,omitempty"`
	Results         *corev1.EmptyDirVolumeSource `json:"results,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Volumes []WorkloadVolume `json:"volumes,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.checks.all(c, self.checks.exists_one(x, x.id == c.id))",message="checks must contain unique IDs"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, c.order) == [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14]",message="checks must use immutable contiguous order 0 through 14"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, c.id) == ['R01', 'R02', 'R03', 'R04', 'R05', 'R06', 'R07', 'R08', 'R09', 'R10', 'R11', 'R12', 'R13', 'R14', 'R15']",message="checks must use ordered IDs R01 through R15"
// +kubebuilder:validation:XValidation:rule="self.checks.all(c, duration(self.deadline) - duration(c.backendActiveDeadline) >= duration('60s'))",message="every backendActiveDeadline must leave at least 60 seconds before deadline"
// +kubebuilder:validation:XValidation:rule="!has(self.recurring) || !self.recurring.enabled",message="recurring certification is not supported for grouped suites"
type CertificationProfileSpec struct {
	Participants ParticipantPolicy `json:"participants"`
	// +kubebuilder:validation:MaxItems=16
	ParticipantRoles []ParticipantRoleConstraint `json:"participantRoles,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	TopologyConstraints []ParticipantTopologyConstraint `json:"topologyConstraints,omitempty"`
	Workflow            WorkflowReference               `json:"workflow"`
	// +kubebuilder:validation:MinItems=15
	// +kubebuilder:validation:MaxItems=15
	Checks          []CertificationCheck `json:"checks"`
	ExecutionPolicy SuiteExecutionPolicy `json:"executionPolicy"`
	// +kubebuilder:validation:Enum=FullCertification;QuickCertification;Preflight;TargetedVerification;Diagnostic
	Purpose      CertificationPurpose `json:"purpose,omitempty"`
	EvidencePath string               `json:"evidencePath"`
	Deadline     metav1.Duration      `json:"deadline"`
	Tolerations  []corev1.Toleration  `json:"tolerations,omitempty"`
	Recurring    *RecurringPolicy     `json:"recurring,omitempty"`

	// Deprecated Go-only aliases retained for unit fixtures. They are not part
	// of the v1alpha1 wire contract.
	Script                ScriptReference      `json:"-"`
	Backend               ExecutionBackend     `json:"-"`
	RunnerImage           string               `json:"-"`
	WorkloadPolicy        WorkloadPolicy       `json:"-"`
	StagePlan             []CertificationStage `json:"-"`
	RuntimeStage          *RuntimeStage        `json:"-"`
	Parameters            map[string]string    `json:"-"`
	BackendActiveDeadline metav1.Duration      `json:"-"`
	MaxAttempts           int32                `json:"-"`
}

type RecurringPolicy struct {
	Enabled                       bool            `json:"enabled"`
	NodeNames                     []string        `json:"nodeNames"`
	MinIdleDuration               metav1.Duration `json:"minIdleDuration"`
	ValidFor                      metav1.Duration `json:"validFor"`
	NotBefore                     *metav1.Time    `json:"notBefore,omitempty"`
	ExpiresAt                     metav1.Time     `json:"expiresAt"`
	EvaluationInterval            metav1.Duration `json:"evaluationInterval,omitempty"`
	FailureBackoff                metav1.Duration `json:"failureBackoff"`
	MaxConsecutiveFailures        int32           `json:"maxConsecutiveFailures"`
	MaxConcurrent                 int32           `json:"maxConcurrent"`
	FailureDomainLabel            string          `json:"failureDomainLabel,omitempty"`
	MaxConcurrentPerFailureDomain int32           `json:"maxConcurrentPerFailureDomain,omitempty"`
	ExclusiveProfiles             []string        `json:"exclusiveProfiles,omitempty"`
}

type RecurringNodeStatus struct {
	NodeName            string       `json:"nodeName"`
	IdleSince           *metav1.Time `json:"idleSince,omitempty"`
	LastEvaluatedAt     *metav1.Time `json:"lastEvaluatedAt,omitempty"`
	LastSuccessfulRun   string       `json:"lastSuccessfulRun,omitempty"`
	LastSuccessfulAt    *metav1.Time `json:"lastSuccessfulAt,omitempty"`
	NextDueAt           *metav1.Time `json:"nextDueAt,omitempty"`
	ConsecutiveFailures int32        `json:"consecutiveFailures,omitempty"`
	Suspended           bool         `json:"suspended,omitempty"`
	Reason              string       `json:"reason,omitempty"`
}

type CertificationProfileStatus struct {
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	Nodes              []RecurringNodeStatus `json:"nodes,omitempty"`
	Conditions         []metav1.Condition    `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certprofile
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Checks",type=integer,JSONPath=`.spec.checks.size()`
// +kubebuilder:printcolumn:name="Participants",type=integer,JSONPath=`.spec.participants.count`
// +kubebuilder:printcolumn:name="Recurring",type=boolean,JSONPath=`.spec.recurring.enabled`
type CertificationProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertificationProfileSpec   `json:"spec"`
	Status CertificationProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CertificationProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationProfile `json:"items"`
}
