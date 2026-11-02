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

// MachineOperation asks for a one-time action on one or more Machines, such as
// rebooting a node or powering off a host.
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

// OperationKind is the action to take. The enum lists every kind; there are no
// custom kinds.
// +kubebuilder:validation:Enum=NodeReboot;AgentUpgrade;AgentReset;HostHardReboot;HostSoftReboot;HostPowerOff;HostPowerOn;HostReplace
type OperationKind string

const (
	// OperationNodeReboot restarts the node and its containers, not the host.
	OperationNodeReboot OperationKind = "NodeReboot"

	// OperationAgentUpgrade upgrades the agent on the host.
	OperationAgentUpgrade OperationKind = "AgentUpgrade"

	// OperationAgentReset removes the agent and everything it set up.
	OperationAgentReset OperationKind = "AgentReset"

	// OperationHostHardReboot restarts the host through its BMC or cloud provider.
	OperationHostHardReboot OperationKind = "HostHardReboot"

	// OperationHostSoftReboot reboots the host's operating system through the agent.
	OperationHostSoftReboot OperationKind = "HostSoftReboot"

	// OperationHostPowerOff powers off the host through its BMC or cloud provider.
	OperationHostPowerOff OperationKind = "HostPowerOff"

	// OperationHostPowerOn powers on the host through its BMC or cloud provider.
	OperationHostPowerOn OperationKind = "HostPowerOn"

	// OperationHostReplace reinstalls the host so the node can rejoin the cluster.
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
	OperationStageRepaveRequested  OperationStage = "RepaveRequested" // Not set yet.
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
	// MachineRef is the name of the Machine to act on. Set either machineRef
	// or machineSelector; if both are set, machineRef is used. Setting neither
	// is not checked yet.
	// +optional
	MachineRef string `json:"machineRef,omitempty"`

	// TODO(#1040): revisit once agent-run selector operations report
	// per-Machine status and match Machine labels.

	// MachineSelector picks the Machines to act on by label. They all share
	// this one MachineOperation. For kinds the agent runs, the selector is
	// matched against Node labels, and the operation completes or fails when
	// the first Machine finishes; Machines that have not started by then skip
	// it. Not supported for hosts managed through a cloud provider.
	// +optional
	MachineSelector *metav1.LabelSelector `json:"machineSelector,omitempty"`

	// OperationKind is the action to take. NodeReboot restarts the node,
	// including kubelet, containerd and the containers running on it, without
	// rebooting the host; the node is not drained first. AgentUpgrade upgrades
	// the agent; AgentReset removes it and everything it set up.
	// HostSoftReboot reboots the host's operating system through the agent and
	// completes once the host is back; if the host does not come back, the
	// operation stays InProgress, because it does not time out yet.
	// HostHardReboot, HostPowerOff and HostPowerOn restart, stop or start the
	// host through its BMC or cloud provider. HostReplace reinstalls the host
	// so the node can rejoin.
	// +kubebuilder:validation:Required
	OperationKind OperationKind `json:"operationKind"`

	// TODO: Revisit whether map[string]string is sufficient or if we
	// need a richer type (e.g. runtime.RawExtension for arbitrary
	// JSON, or a typed []OperationParameter struct).

	// Parameters are extra settings for some kinds. AgentUpgrade needs
	// "downloadURL" and accepts "sha256". HostReplace on OCI accepts
	// "sshAuthorizedKeys", and "imageID", which is used only when neither the
	// Machine nor its configuration sets a host image. Other kinds ignore
	// them.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// TODO(#1043): drop the "Not enforced" note once every installation
	// deletes expired operations.

	// TTLSecondsAfterFinished deletes the MachineOperation this many seconds
	// after it completes or fails. If unset, it is kept.
	// Not enforced without machine-ops-controller or metalman yet.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// MachineOperationStatus defines the observed state of a MachineOperation.
type MachineOperationStatus struct {
	// Phase is Pending, InProgress, Complete or Failed.
	Phase OperationPhase `json:"phase,omitempty"`

	// Message is a human-readable description of the current state.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when work on the operation began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the operation reached a terminal state
	// (Complete or Failed).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// ObservedMachineGeneration is the Machine's metadata.generation that the
	// operation acted on. Compare it with the current value to see whether the
	// Machine has changed since. It may be empty for a failed operation, and
	// is not set for bare-metal host operations yet.
	// +optional
	ObservedMachineGeneration int64 `json:"observedMachineGeneration,omitempty"`

	// Targets records progress for each Machine, by name. For actions run
	// through a BMC or cloud provider, the list is fixed when the operation
	// starts, even if labels change later. Not every kind records targets.
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
	// BootMedia is metalman's internal record of the boot media it used for
	// this Machine. Do not rely on its fields.
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

// MachineOperationBootMediaStatus is metalman's internal record of the boot
// media it made for one Machine.
type MachineOperationBootMediaStatus struct {
	// ID identifies the boot media. It changes if the media is replaced.
	ID string `json:"id"`
	// SourceServer is the server the BMC loads the media from.
	SourceServer string `json:"sourceServer,omitempty"`
	// SourceName is the media's name on SourceServer.
	SourceName string `json:"sourceName,omitempty"`
	// Phase is metalman's current step.
	Phase string `json:"phase"`
	// MachineUID is the UID of the Machine the media was made for.
	MachineUID types.UID `json:"machineUID"`
	// Netboot is a copy of the Machine's netboot settings when the media was
	// made.
	Netboot *PXESpec `json:"netboot"`
	// CertFingerprint is the fingerprint of the BMC's TLS certificate.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	// ImageWritten is true once the installer has written the image to disk.
	ImageWritten bool `json:"imageWritten,omitempty"`
	// Cleaned is true once the BMC is set to boot from disk and the media is
	// unmounted.
	Cleaned bool `json:"cleaned,omitempty"`
	// StagedDeleted is true once the staged media file has been deleted.
	StagedDeleted bool `json:"stagedDeleted,omitempty"`
	// CleanedAt is when Cleaned became true.
	CleanedAt *metav1.Time `json:"cleanedAt,omitempty"`
	// RebootRequestedAt is when the installer reported it was ready to reboot.
	RebootRequestedAt *metav1.Time `json:"rebootRequestedAt,omitempty"`
	// Released is true once metalman has deleted its own copy of the media.
	Released bool `json:"released,omitempty"`
	// Site is the site of the metalman that owns the media.
	Site string `json:"site,omitempty"`
	// NodeName is the name of the Node the host is expected to join as.
	NodeName string `json:"nodeName,omitempty"`
	// PhaseStartedAt is when the current Phase started.
	PhaseStartedAt *metav1.Time `json:"phaseStartedAt,omitempty"`
	// ReadinessStartedAt is when the BMC first reported that it was not ready
	// during the current Phase.
	ReadinessStartedAt *metav1.Time `json:"readinessStartedAt,omitempty"`
	// LoaderStartedAt is when the host, booted from the media, first fetched
	// its install config.
	LoaderStartedAt *metav1.Time `json:"loaderStartedAt,omitempty"`
	// BootAttempts counts power-on attempts used to boot the media.
	BootAttempts int32 `json:"bootAttempts,omitempty"`
	// Abandoned is true once metalman stops waiting for the installer and
	// gives up the media.
	Abandoned bool `json:"abandoned,omitempty"`
}

// IsTerminal returns true if the operation phase is Complete or Failed.
func (s *MachineOperationStatus) IsTerminal() bool {
	return s.Phase == OperationPhaseComplete || s.Phase == OperationPhaseFailed
}
