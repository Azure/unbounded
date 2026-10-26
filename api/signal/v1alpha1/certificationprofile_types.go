// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CheckID string

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
type CertificationCheck struct {
	// ID and Order are derived from the check's position and are never serialized.
	ID    CheckID `json:"-"`
	Order int32   `json:"-"`
	// +kubebuilder:validation:MinLength=1
	ScriptKey      string           `json:"scriptKey"`
	RunnerImage    string           `json:"runnerImage"`
	Backend        ExecutionBackend `json:"backend"`
	WorkloadPolicy WorkloadPolicy   `json:"workloadPolicy,omitempty"`
	// +kubebuilder:validation:Minimum=0
	GPUCount              int32           `json:"gpuCount,omitempty"`
	BackendActiveDeadline metav1.Duration `json:"backendActiveDeadline"`
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
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
	Privileged  bool                        `json:"privileged,omitempty"`
	IPCLock     bool                        `json:"ipcLock,omitempty"`
	HostNetwork bool                        `json:"hostNetwork,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	HostPaths []WorkloadHostPath `json:"hostPaths,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.checks.all(c, duration(self.deadline) - duration(c.backendActiveDeadline) >= duration('60s'))",message="every backendActiveDeadline must leave at least 60 seconds before deadline"
// +kubebuilder:validation:XValidation:rule="self.checks.map(c, duration(c.backendActiveDeadline) + duration('60s')).sum() <= duration(self.deadline)",message="deadline must cover every sequential backendActiveDeadline plus 60 seconds of evidence time per check"
type CertificationProfileSpec struct {
	Participants ParticipantPolicy `json:"participants"`
	// +kubebuilder:validation:MinLength=1
	ScriptConfigMap string `json:"scriptConfigMap"`
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
