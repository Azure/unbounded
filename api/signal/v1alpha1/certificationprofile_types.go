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

type CheckID string

type ParticipantRole struct {
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
	Roles []string `json:"roles,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.scriptEnvironment) || self.scriptEnvironment.all(k, size(k) <= 253 && size(self.scriptEnvironment[k]) <= 4096)",message="scriptEnvironment names must not exceed 253 characters and values must not exceed 4096 characters"
type CertificationCheck struct {
	// ID and Order are derived from the check's position and are never serialized.
	ID             CheckID          `json:"-"`
	Order          int32            `json:"-"`
	Script         ScriptReference  `json:"script"`
	RunnerImage    string           `json:"runnerImage"`
	Backend        ExecutionBackend `json:"backend"`
	WorkloadPolicy WorkloadPolicy   `json:"workloadPolicy,omitempty"`
	// +kubebuilder:validation:Minimum=0
	GPUCount int32 `json:"gpuCount,omitempty"`
	// +kubebuilder:validation:MaxProperties=32
	ScriptEnvironment     map[string]string      `json:"scriptEnvironment,omitempty"`
	BackendActiveDeadline metav1.Duration        `json:"backendActiveDeadline"`
	ParticipantPolicy     CheckParticipantPolicy `json:"participantPolicy,omitempty"`
}

// +kubebuilder:validation:Enum=Pod;MPIJob
type ExecutionBackend string

const (
	ExecutionBackendPod    ExecutionBackend = "Pod"
	ExecutionBackendMPIJob ExecutionBackend = "MPIJob"
)

type ParticipantPolicy struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	Count int32 `json:"count"`

	Selector metav1.LabelSelector `json:"selector"`

	MinimumAllocatable corev1.ResourceList `json:"minimumAllocatable,omitempty"`
}

type WorkloadHostPath struct {
	MountPath string `json:"mountPath"`
	Path      string `json:"path"`
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

// +kubebuilder:validation:XValidation:rule="self.checks.all(c, duration(self.deadline) - duration(c.backendActiveDeadline) >= duration('60s'))",message="every backendActiveDeadline must leave at least 60 seconds before deadline"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, duration(c.backendActiveDeadline) + duration('60s')).sum() <= duration(self.deadline)",message="deadline must cover every sequential backendActiveDeadline plus 60 seconds of evidence time per check"
type CertificationProfileSpec struct {
	Participants ParticipantPolicy `json:"participants"`
	// +kubebuilder:validation:MaxItems=16
	ParticipantRoles []ParticipantRole `json:"participantRoles,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	TopologyConstraints []ParticipantTopologyConstraint `json:"topologyConstraints,omitempty"`
	// +kubebuilder:validation:MinItems=15
	// +kubebuilder:validation:MaxItems=15
	Checks   []CertificationCheck `json:"checks"`
	Deadline metav1.Duration      `json:"deadline"`
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
