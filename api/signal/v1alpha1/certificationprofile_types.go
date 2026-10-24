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

type CheckID string

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

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.script.requiresRuntimeStage) || !self.script.requiresRuntimeStage || has(self.runtimeStage)",message="runtimeStage is required when the script requires a runtime stage"
type CertificationCheck struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^R(0[1-9]|1[0-5])$`
	ID CheckID `json:"id"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=14
	Order          int32            `json:"order"`
	Script         ScriptReference  `json:"script"`
	RunnerImage    string           `json:"runnerImage"`
	Backend        ExecutionBackend `json:"backend"`
	WorkloadPolicy WorkloadPolicy   `json:"workloadPolicy,omitempty"`
	RuntimeStage   *RuntimeStage    `json:"runtimeStage,omitempty"`
	// +kubebuilder:validation:Minimum=0
	GPUCount              int32                  `json:"gpuCount,omitempty"`
	ScriptEnvironment     map[string]string      `json:"scriptEnvironment,omitempty"`
	BackendActiveDeadline metav1.Duration        `json:"backendActiveDeadline"`
	ParticipantPolicy     CheckParticipantPolicy `json:"participantPolicy,omitempty"`
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
}

type ParticipantPolicy struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count int32 `json:"count"`

	Selector metav1.LabelSelector `json:"selector"`

	RequiredResources corev1.ResourceList `json:"requiredResources,omitempty"`
}

type WorkloadHostPath struct {
	Name      string              `json:"name"`
	MountPath string              `json:"mountPath"`
	Path      string              `json:"path"`
	Type      corev1.HostPathType `json:"type,omitempty"`
}

type WorkloadPolicy struct {
	Resources    corev1.ResourceRequirements `json:"resources,omitempty"`
	Privileged   bool                        `json:"privileged,omitempty"`
	Capabilities []corev1.Capability         `json:"capabilities,omitempty"`
	HostNetwork  bool                        `json:"hostNetwork,omitempty"`
	WorkingDir   string                      `json:"workingDir,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	HostPaths []WorkloadHostPath `json:"hostPaths,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.checks.all(c, self.checks.exists_one(x, x.id == c.id))",message="checks must contain unique IDs"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, c.order) == [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14]",message="checks must use immutable contiguous order 0 through 14"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, c.id) == ['R01', 'R02', 'R03', 'R04', 'R05', 'R06', 'R07', 'R08', 'R09', 'R10', 'R11', 'R12', 'R13', 'R14', 'R15']",message="checks must use ordered IDs R01 through R15"
// +kubebuilder:validation:XValidation:rule="self.checks.all(c, duration(self.deadline) - duration(c.backendActiveDeadline) >= duration('60s'))",message="every backendActiveDeadline must leave at least 60 seconds before deadline"
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
	EvidencePath    string               `json:"evidencePath"`
	Deadline        metav1.Duration      `json:"deadline"`
	Tolerations     []corev1.Toleration  `json:"tolerations,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certprofile
// +kubebuilder:printcolumn:name="Checks",type=integer,JSONPath=`.spec.checks.size()`
// +kubebuilder:printcolumn:name="Participants",type=integer,JSONPath=`.spec.participants.count`
type CertificationProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CertificationProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type CertificationProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationProfile `json:"items"`
}
