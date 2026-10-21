// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ScriptReference struct {
	Name string `json:"name"`
	Key  string `json:"key"`
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

type WorkloadPolicy struct {
	Resources       corev1.ResourceRequirements  `json:"resources,omitempty"`
	SecurityContext *corev1.SecurityContext      `json:"securityContext,omitempty"`
	WorkingDir      string                       `json:"workingDir,omitempty"`
	Results         *corev1.EmptyDirVolumeSource `json:"results,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	Volumes []WorkloadVolume `json:"volumes,omitempty"`
}

type CertificationProfileSpec struct {
	Participants   ParticipantPolicy `json:"participants"`
	Script         ScriptReference   `json:"script"`
	Workflow       WorkflowReference `json:"workflow"`
	Backend        ExecutionBackend  `json:"backend"`
	RunnerImage    string            `json:"runnerImage"`
	WorkloadPolicy WorkloadPolicy    `json:"workloadPolicy,omitempty"`
	// +kubebuilder:validation:Enum=FullCertification;QuickCertification;Preflight;TargetedVerification;Diagnostic
	Purpose      CertificationPurpose `json:"purpose,omitempty"`
	StagePlan    []CertificationStage `json:"stagePlan,omitempty"`
	Parameters   map[string]string    `json:"parameters,omitempty"`
	EvidencePath string               `json:"evidencePath"`
	Deadline     metav1.Duration      `json:"deadline"`
	MaxAttempts  int32                `json:"maxAttempts,omitempty"`
	Tolerations  []corev1.Toleration  `json:"tolerations,omitempty"`
	Recurring    *RecurringPolicy     `json:"recurring,omitempty"`
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
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend.kind`
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
