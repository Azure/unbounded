// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&Operation{}, &OperationList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=op
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineRef"
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Operation represents a discrete operation to be performed on a Machine.
// Operations are created by CLI commands or controllers and processed by
// the agent daemon running on the target machine.
type Operation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OperationSpec   `json:"spec,omitempty"`
	Status OperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OperationList contains a list of Operation.
type OperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Operation `json:"items"`
}

// OperationType identifies the kind of operation to perform.
// +kubebuilder:validation:Enum=SoftReboot;HardReboot
type OperationType string

const (
	// OperationTypeSoftReboot restarts the nspawn machine in place without
	// reprovisioning the rootfs. Services are stopped, the nspawn container
	// is restarted, and services are brought back up.
	OperationTypeSoftReboot OperationType = "SoftReboot"

	// OperationTypeHardReboot triggers a full hardware reboot of the host
	// via Redfish BMC. Reserved for future use by the machina controller.
	OperationTypeHardReboot OperationType = "HardReboot"
)

// OperationPhase represents the current phase of an Operation.
type OperationPhase string

const (
	OperationPhasePending    OperationPhase = "Pending"
	OperationPhaseInProgress OperationPhase = "InProgress"
	OperationPhaseCompleted  OperationPhase = "Completed"
	OperationPhaseFailed     OperationPhase = "Failed"
)

// OperationSpec defines the desired state of an Operation.
type OperationSpec struct {
	// MachineRef is the name of the Machine CR this operation targets.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	MachineRef string `json:"machineRef"`

	// Type is the operation to perform on the target machine.
	// +kubebuilder:validation:Required
	Type OperationType `json:"type"`

	// TTLSecondsAfterFinished limits the lifetime of a completed or failed
	// Operation. If set, the agent deletes the Operation this many seconds
	// after it reaches a terminal phase. If unset, the Operation is kept
	// indefinitely.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// OperationStatus defines the observed state of an Operation.
type OperationStatus struct {
	// Phase is the current phase of the operation.
	Phase OperationPhase `json:"phase,omitempty"`

	// Message is a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the agent began executing the operation.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation reached a terminal state
	// (Completed or Failed).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Conditions represent the latest available observations of the
	// operation's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsTerminal returns true if the operation phase is Completed or Failed.
func (s *OperationStatus) IsTerminal() bool {
	return s.Phase == OperationPhaseCompleted || s.Phase == OperationPhaseFailed
}
