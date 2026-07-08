// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &MachineOperation{}, &MachineOperationList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
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
// like NodeReboot, while cloud or PXE controllers handle operations like
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
// +kubebuilder:validation:Enum=NodeReboot;AgentUpgrade;AgentReset;HostReboot;HostPowerOff;HostPowerOn;HostReplace
type OperationKind string

const (
	// OperationNodeReboot restarts the nspawn-backed node in place without
	// reprovisioning the rootfs. Services are stopped, the nspawn container
	// is restarted, and services are brought back up. Handled by the
	// in-VM agent.
	OperationNodeReboot OperationKind = "NodeReboot"

	// OperationAgentUpgrade upgrades the host-side unbounded-agent binary.
	// Handled by the in-VM agent.
	OperationAgentUpgrade OperationKind = "AgentUpgrade"

	// OperationAgentReset resets the host by removing the unbounded-agent and
	// all associated resources. Handled by the in-VM agent.
	OperationAgentReset OperationKind = "AgentReset"

	// OperationHostReboot triggers a full hardware power cycle of the host
	// via BMC (e.g. Redfish). Handled by the machina controller or cloud
	// controller.
	OperationHostReboot OperationKind = "HostReboot"

	// OperationHostPowerOff powers off the host through an out-of-band provider.
	OperationHostPowerOff OperationKind = "HostPowerOff"

	// OperationHostPowerOn powers on the host through an out-of-band provider.
	OperationHostPowerOn OperationKind = "HostPowerOn"

	// OperationHostReplace replaces the host VM through an out-of-band provider
	// and reinstalls the unbounded-agent so the node can rejoin the cluster.
	OperationHostReplace OperationKind = "HostReplace"
)

// OperationPhase represents the current phase of a MachineOperation.
type OperationPhase string

const (
	OperationPhasePending    OperationPhase = "Pending"
	OperationPhaseInProgress OperationPhase = "InProgress"
	OperationPhaseComplete   OperationPhase = "Complete"
	OperationPhaseFailed     OperationPhase = "Failed"
)

// Condition types for MachineOperation.
const (
	// MachineOperationConditionCompleted indicates whether the operation has
	// reached a terminal state.
	MachineOperationConditionCompleted = "Completed"

	// MachineOperationConditionBootLoaderDownloaded indicates that a metalman
	// target has downloaded the initial PXE boot loader for the operation.
	// Once set to True for an operation, it remains latched.
	MachineOperationConditionBootLoaderDownloaded = "BootLoaderDownloaded"

	// MachineOperationConditionBootImageWritten indicates that a metalman
	// target has booted the PXE installer and written the boot image to disk.
	MachineOperationConditionBootImageWritten = "BootImageWritten"

	// MachineOperationConditionCloudInitDone indicates that a metalman PXE
	// target has completed first-boot cloud-init after writing the boot image.
	MachineOperationConditionCloudInitDone = "CloudInitDone"
)

// Condition types for MachineOperation targets.
const (
	// MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported
	// indicates that metalman determined the target BMC does not support
	// disabling the Redfish boot override. When True, metalman falls back to
	// setting Hdd/Continuous for this target within this operation only.
	MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported = "RedfishDisableBootOverrideUnsupported"
)

// OperationStage represents the current stage of a target operation.
type OperationStage string

const (
	OperationStagePoweringOff      OperationStage = "PoweringOff"
	OperationStageWaitingOff       OperationStage = "WaitingOff"
	OperationStagePoweringOn       OperationStage = "PoweringOn"
	OperationStageWaitingOn        OperationStage = "WaitingOn"
	OperationStageRepaveRequested  OperationStage = "RepaveRequested"
	OperationStageWaitingRepave    OperationStage = "WaitingRepave"
	OperationStageWaitingCloudInit OperationStage = "WaitingCloudInit"
	OperationStageWaitingNode      OperationStage = "WaitingNode"
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

	// Targets records the per-machine execution state for this operation.
	// For single-machine operations this contains one entry. For selector
	// operations, targets are snapshotted when execution begins and remain
	// authoritative even if selector matches change later.
	// +optional
	// +listType=map
	// +listMapKey=machineRef
	Targets []MachineOperationTargetStatus `json:"targets,omitempty"`

	// Conditions represent the latest available observations of the
	// operation's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachineOperationTargetStatus records execution state for one target Machine.
type MachineOperationTargetStatus struct {
	// MachineRef is the name of the targeted Machine.
	// +kubebuilder:validation:Required
	MachineRef string `json:"machineRef"`

	// Phase is the current lifecycle phase for this target.
	// +optional
	Phase OperationPhase `json:"phase,omitempty"`

	// Stage is the current operation-specific stage for this target.
	// +optional
	Stage OperationStage `json:"stage,omitempty"`

	// Message is a human-readable description of target progress or failure.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when execution began for this target.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when this target reached a terminal phase.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedGeneration is the target Machine generation acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Attempts is the number of external action attempts made for this target.
	// Polling expected state changes does not increment this field.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// LastAttemptAt records when the latest external action attempt occurred.
	// +optional
	LastAttemptAt *metav1.Time `json:"lastAttemptAt,omitempty"`

	// Conditions represent target-scoped observations for this operation. These
	// conditions are not persisted on the Machine, so BMC capability fallbacks
	// apply only to this MachineOperation target.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// IsTerminal returns true if the operation phase is Complete or Failed.
func (s *MachineOperationStatus) IsTerminal() bool {
	return s.Phase == OperationPhaseComplete || s.Phase == OperationPhaseFailed
}
