// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CheckID string

const (
	// MinCertificationChecks keeps certification suites non-empty.
	MinCertificationChecks = 1
	// MaxCertificationChecks bounds CEL immutability cost and persisted Run
	// status size without making the suite length part of the domain model.
	MaxCertificationChecks = 256
)

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="self.backend != 'MPIJob' || (has(self.gpuCount) && self.gpuCount > 0)",message="MPIJob checks require a positive gpuCount"
type CertificationCheck struct {
	// ID and Order are derived from the check's position and are never serialized.
	ID    CheckID `json:"-"`
	Order int32   `json:"-"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	ScriptKey      string           `json:"scriptKey"`
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

// +kubebuilder:validation:XValidation:rule="self.mountPath.startsWith('/') && !self.mountPath.contains('//') && !self.mountPath.contains('/./') && !self.mountPath.contains('/../') && !self.mountPath.endsWith('/.') && !self.mountPath.endsWith('/..')",message="mountPath must be a clean absolute path"
// +kubebuilder:validation:XValidation:rule="self.mountPath != '/scripts' && self.mountPath != '/results'",message="mountPath must not replace a reserved Project Signal mount"
// +kubebuilder:validation:XValidation:rule="(self.path == '/proc' || self.path.startsWith('/proc/') || self.path == '/sys' || self.path.startsWith('/sys/') || self.path == '/var/lib/project-signal' || self.path.startsWith('/var/lib/project-signal/')) && !self.path.contains('//') && !self.path.contains('/./') && !self.path.contains('/../') && !self.path.endsWith('/.') && !self.path.endsWith('/..')",message="path must be a clean absolute path under /proc, /sys, or /var/lib/project-signal"
type WorkloadHostPath struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	MountPath string `json:"mountPath"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Path string `json:"path"`
}

type WorkloadPolicy struct {
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
	Privileged  bool                        `json:"privileged,omitempty"`
	IPCLock     bool                        `json:"ipcLock,omitempty"`
	HostNetwork bool                        `json:"hostNetwork,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=mountPath
	HostPaths []WorkloadHostPath `json:"hostPaths,omitempty"`
}

type ScriptBundleSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// Digest is the aggregate algorithm-qualified SHA-256 digest derived from
	// the selected script bytes in check order.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

type CertificationSuiteSpec struct {
	ScriptBundle ScriptBundleSource `json:"scriptBundle"`
	// Checks is the ordered executable test plan. A check's position defines
	// its stable ID.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks []CertificationCheck `json:"checks"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certsuite
// +kubebuilder:printcolumn:name="Checks",type=integer,JSONPath=`.spec.checks.size()`
// +kubebuilder:printcolumn:name="Script Bundle",type=string,JSONPath=`.spec.scriptBundle.name`
type CertificationSuite struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CertificationSuiteSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type CertificationSuiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationSuite `json:"items"`
}
