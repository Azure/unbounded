// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&MachineOperation{}, &MachineOperationList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mop
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineRef"
// +kubebuilder:printcolumn:name="Operation",type="string",JSONPath=".spec.operationKind"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineOperation represents a discrete operation to be performed on a
// Machine. MachineOperations are created by CLI commands or controllers and
// processed by the appropriate agent. The in-VM agent handles operations
// like MachineReboot, while cloud or PXE controllers handle operations like
// HostReboot, HostPowerOff, and HostPowerOn.
type MachineOperation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineOperationSpec   `json:"spec,omitempty"`
	Status MachineOperationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineOperationList contains a list of MachineOperation.
type MachineOperationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineOperation `json:"items"`
}

// OperationKind identifies the kind of operation to perform. Predefined
// operations cover common lifecycle actions; custom operations may be
// supported by individual cloud controllers.
// +kubebuilder:validation:Enum=MachineReboot;HostReboot;HostPowerOff;HostPowerOn
type OperationKind string

const (
	// OperationMachineReboot restarts the nspawn machine in place without
	// reprovisioning the rootfs. Services are stopped, the nspawn container
	// is restarted, and services are brought back up. Handled by the
	// in-VM agent.
	OperationMachineReboot OperationKind = "MachineReboot"

	// OperationHostReboot triggers a full hardware power cycle of the host
	// via BMC (e.g. Redfish). Handled by the machina controller or cloud
	// controller.
	OperationHostReboot OperationKind = "HostReboot"

	// OperationHostPowerOff powers off the host through an out-of-band provider.
	OperationHostPowerOff OperationKind = "HostPowerOff"

	// OperationHostPowerOn powers on the host through an out-of-band provider.
	OperationHostPowerOn OperationKind = "HostPowerOn"
)

// OperationPhase represents the current phase of a MachineOperation.
type OperationPhase string

const (
	OperationPhasePending    OperationPhase = "Pending"
	OperationPhaseInProgress OperationPhase = "InProgress"
	OperationPhaseComplete   OperationPhase = "Complete"
	OperationPhaseFailed     OperationPhase = "Failed"
)

// MachineOperationSpec defines the desired state of a MachineOperation.
type MachineOperationSpec struct {
	// MachineRef is the name of the Machine CR this operation targets.
	// Either machineRef or machineSelector must be set.
	// +optional
	MachineRef string `json:"machineRef,omitempty"`

	// MachineSelector selects machines by labels. When set, the
	// controller creates individual MachineOperation children for
	// each matched machine. Either machineRef or machineSelector
	// must be set.
	// +optional
	MachineSelector *metav1.LabelSelector `json:"machineSelector,omitempty"`

	// OperationKind is the operation to perform on the target machine.
	// +kubebuilder:validation:Required
	OperationKind OperationKind `json:"operationKind"`

	// Parameters is an optional set of key-value pairs passed to the
	// operation executor. For example, RestartService uses "service" to
	// specify the systemd unit name.
	// TODO: Revisit whether map[string]string is sufficient or if we
	// need a richer type (e.g. runtime.RawExtension for arbitrary
	// JSON, or a typed []OperationParameter struct).
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// TTLSecondsAfterFinished limits the lifetime of a completed or failed
	// MachineOperation. If set, the agent deletes the MachineOperation
	// this many seconds after it reaches a terminal phase. If unset, the
	// MachineOperation is kept indefinitely.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// MachineOperationStatus defines the observed state of a MachineOperation.
type MachineOperationStatus struct {
	// Phase is the current phase of the operation.
	Phase OperationPhase `json:"phase,omitempty"`

	// Message is a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the agent began executing the operation.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation reached a terminal state
	// (Complete or Failed).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedMachineGeneration is the Machine's metadata.generation
	// at the time the agent began executing the operation. Clients
	// can compare this to the current Machine generation to determine
	// whether the operation acted on the expected machine state.
	// +optional
	ObservedMachineGeneration int64 `json:"observedMachineGeneration,omitempty"`

	// Conditions represent the latest available observations of the
	// operation's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsTerminal returns true if the operation phase is Complete or Failed.
func (s *MachineOperationStatus) IsTerminal() bool {
	return s.Phase == OperationPhaseComplete || s.Phase == OperationPhaseFailed
}
