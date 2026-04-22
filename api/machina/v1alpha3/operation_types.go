// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// MachineOperationMachineLabelKey is the label key set on every
	// MachineOperation to identify the target Machine. Agents use a
	// label selector on this key to scope their informer to only
	// operations targeting their own machine.
	MachineOperationMachineLabelKey = "unbounded-kube.io/machine"
)

func init() {
	SchemeBuilder.Register(&MachineOperation{}, &MachineOperationList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mop
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineRef"
// +kubebuilder:printcolumn:name="Operation",type="string",JSONPath=".spec.operationName"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineOperation represents a discrete operation to be performed on a
// Machine. MachineOperations are created by CLI commands or controllers and
// processed by the appropriate agent - the in-VM agent handles operations
// like Reboot, while cloud or PXE controllers handle operations like
// HardReboot, PowerOff, and PowerOn.
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

// OperationName identifies the kind of operation to perform. Predefined
// operations cover common lifecycle actions; custom operations may be
// supported by individual cloud controllers.
// +kubebuilder:validation:Enum=Reboot;HardReboot;Shutdown;PowerOff;PowerOn;RestartService
type OperationName string

const (
	// OperationReboot restarts the nspawn machine in place without
	// reprovisioning the rootfs. Services are stopped, the nspawn container
	// is restarted, and services are brought back up. Handled by the
	// in-VM agent.
	OperationReboot OperationName = "Reboot"

	// OperationHardReboot triggers a full hardware power cycle of the host
	// via BMC (e.g. Redfish). Handled by the machina controller or cloud
	// controller.
	OperationHardReboot OperationName = "HardReboot"

	// OperationShutdown gracefully shuts down the nspawn machine. Handled
	// by the in-VM agent.
	OperationShutdown OperationName = "Shutdown"

	// OperationPowerOff powers off the host via BMC. Handled by the
	// machina controller or cloud controller.
	OperationPowerOff OperationName = "PowerOff"

	// OperationPowerOn powers on the host via BMC. Handled by the
	// machina controller or cloud controller.
	OperationPowerOn OperationName = "PowerOn"

	// OperationRestartService restarts a named service inside the nspawn
	// machine. The service name should be specified in Parameters.
	// Handled by the in-VM agent.
	OperationRestartService OperationName = "RestartService"
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
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	MachineRef string `json:"machineRef"`

	// OperationName is the operation to perform on the target machine.
	// +kubebuilder:validation:Required
	OperationName OperationName `json:"operationName"`

	// Parameters is an optional set of key-value pairs passed to the
	// operation executor. For example, RestartService uses "service" to
	// specify the systemd unit name.
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

	// Conditions represent the latest available observations of the
	// operation's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsTerminal returns true if the operation phase is Complete or Failed.
func (s *MachineOperationStatus) IsTerminal() bool {
	return s.Phase == OperationPhaseComplete || s.Phase == OperationPhaseFailed
}
