// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
// like NodeReboot and HostSoftReboot, while cloud or PXE controllers handle
// operations like HostHardReboot, HostPowerOff, and HostPowerOn.
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
// +kubebuilder:validation:Enum=NodeReboot;AgentUpgrade;AgentReset;HostHardReboot;HostSoftReboot;HostPowerOff;HostPowerOn;HostReplace
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

	// OperationHostHardReboot triggers a full hardware power cycle of the host
	// via BMC (e.g. Redfish). Handled by the machina controller or cloud
	// controller.
	OperationHostHardReboot OperationKind = "HostHardReboot"

	// OperationHostSoftReboot reboots the host operating system from inside
	// the host (systemctl reboot). Handled by the in-VM agent, which reports
	// completion after it starts again on a new boot.
	OperationHostSoftReboot OperationKind = "HostSoftReboot"

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

	// MachineOperationConditionProviderOperationStalled reports that an
	// accepted external provider operation has exceeded its expected duration.
	MachineOperationConditionProviderOperationStalled = "ProviderOperationStalled"
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
	OperationStageWaitingProvider  OperationStage = "WaitingProviderOperation"
)

// ProviderOperationStatus identifies a resumable operation owned by an
// external provider.
type ProviderOperationStatus struct {
	// Provider is the external provider that accepted the operation.
	// +kubebuilder:validation:Required
	Provider string `json:"provider"`

	// OperationID is the provider-assigned operation identifier.
	// +kubebuilder:validation:Required
	OperationID string `json:"operationID"`

	// ResumeToken is opaque provider data needed to resume polling. Providers
	// must not store credentials or other secrets in this field.
	// +optional
	ResumeToken string `json:"resumeToken,omitempty"`
}

// MachineOperationTargetInput is the immutable, controller-resolved input for
// one MachineOperation target. It is persisted before provider execution so
// retries do not silently adopt later desired-state changes.
type MachineOperationTargetInput struct {
	// ProviderRef identifies the exact provider-owned resource referenced by
	// host.external.machineRef when the operation target was initialized.
	// +optional
	ProviderRef *ProviderMachineSnapshot `json:"providerRef,omitempty"`

	// HostImage is the resolved provider-interpreted host image for HostReplace.
	// An empty value instructs the provider to preserve the current image.
	// +optional
	HostImage string `json:"hostImage,omitempty"`

	// ProvisioningFormat is resolved with HostImage from the same configuration
	// version at HostReplace initialization. Retries use this frozen pair rather
	// than reselecting intent from a Machine edited during execution.
	// +optional
	ProvisioningFormat ProvisioningFormat `json:"provisioningFormat,omitempty"`
}

// ProviderMachineSnapshot identifies the exact provider-owned resource
// observed when a MachineOperation target was initialized.
type ProviderMachineSnapshot struct {
	// APIGroup is the API group of the provider-owned Machine resource.
	// +kubebuilder:validation:Required
	APIGroup string `json:"apiGroup"`

	// Kind is the kind of the provider-owned Machine resource.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`

	// Name is the name of the provider-owned Machine resource.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// UID detects deletion and recreation of the provider-owned resource.
	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// Generation records the provider-owned resource generation whose inputs
	// were observed.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

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
	// BootMedia records immutable media ownership and cleanup progress.
	// +optional
	BootMedia *MachineOperationBootMediaStatus `json:"bootMedia,omitempty"`

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

	// BootID is the host kernel boot ID the agent observed before it issued
	// a HostSoftReboot. The agent that starts after the reboot compares it with
	// the current boot ID to decide whether the reboot happened.
	// +optional
	BootID string `json:"bootID,omitempty"`

	// Input is the immutable provider input resolved before execution begins.
	// +optional
	Input *MachineOperationTargetInput `json:"input,omitempty"`

	// Attempts is the number of external action attempts made for this target.
	// Polling expected state changes does not increment this field.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// LastAttemptAt records when the latest external action attempt occurred.
	// +optional
	LastAttemptAt *metav1.Time `json:"lastAttemptAt,omitempty"`

	// ProviderOperation identifies the resumable external operation for this
	// target, when the provider returned one.
	// +optional
	ProviderOperation *ProviderOperationStatus `json:"providerOperation,omitempty"`

	// Conditions represent target-scoped observations for this operation. These
	// conditions are not persisted on the Machine, so BMC capability fallbacks
	// apply only to this MachineOperation target.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachineOperationBootMediaStatus persists media ownership before BMC changes.
type MachineOperationBootMediaStatus struct {
	ID              string    `json:"id"`
	SourceServer    string    `json:"sourceServer,omitempty"`
	SourceName      string    `json:"sourceName,omitempty"`
	Phase           string    `json:"phase"`
	MachineUID      types.UID `json:"machineUID"`
	Netboot         *PXESpec  `json:"netboot"`
	CertFingerprint string    `json:"certFingerprint,omitempty"`
	ImageWritten    bool      `json:"imageWritten,omitempty"`
	Cleaned         bool      `json:"cleaned,omitempty"`
	// StagedDeleted confirms rack media deletion after safe boot is restored.
	StagedDeleted bool `json:"stagedDeleted,omitempty"`
	// CleanedAt starts the bounded wait for installer reboot handoff.
	CleanedAt *metav1.Time `json:"cleanedAt,omitempty"`
	// RebootRequestedAt is set by the successful installer ready callback.
	RebootRequestedAt *metav1.Time `json:"rebootRequestedAt,omitempty"`
	// Released confirms local media removal after Cleaned revokes serving.
	Released bool `json:"released,omitempty"`
	// Site preserves controller ownership after the Machine is deleted.
	Site string `json:"site,omitempty"`
	// NodeName preserves the expected Node across Machine edits.
	NodeName string `json:"nodeName,omitempty"`
	// PhaseStartedAt bounds active progress without spending retry attempts.
	PhaseStartedAt *metav1.Time `json:"phaseStartedAt,omitempty"`
	// ReadinessStartedAt gives AC recovery one fixed grace window per phase.
	ReadinessStartedAt *metav1.Time `json:"readinessStartedAt,omitempty"`
	// LoaderStartedAt is set when the bootloader on the media first fetches
	// its install config. It proves the host booted the media.
	LoaderStartedAt *metav1.Time `json:"loaderStartedAt,omitempty"`
	// BootAttempts counts power-on attempts used to boot the media.
	BootAttempts int32 `json:"bootAttempts,omitempty"`
	// Abandoned ends ownership after the installer never finished its
	// handoff within the allowed time.
	Abandoned bool `json:"abandoned,omitempty"`
}

// IsTerminal returns true if the operation phase is Complete or Failed.
func (s *MachineOperationStatus) IsTerminal() bool {
	return s.Phase == OperationPhaseComplete || s.Phase == OperationPhaseFailed
}
