// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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
// +kubebuilder:selectablefield:JSONPath=".spec.machineRef"
// +kubebuilder:selectablefield:JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machineRef"
// +kubebuilder:printcolumn:name="Operation",type="string",JSONPath=".spec.operationKind"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Message",type="string",JSONPath=".status.message",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// The allocation transition and status rules sit on the root, not on spec or
// status, because a rule on a child node is skipped when an update drops that
// whole node.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || !has(oldSelf.spec.allocationRef) || (has(self.spec) && self.spec == oldSelf.spec)",message="spec is immutable for HostAllocate and HostRelease",fieldPath=".spec"
// +kubebuilder:validation:XValidation:rule="(has(oldSelf.spec) && has(oldSelf.spec.allocationRef)) || !has(self.spec) || !has(self.spec.allocationRef)",message="allocationRef cannot be added after creation",fieldPath=".spec.allocationRef"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.allocation) || (has(self.status) && has(self.status.allocation) && self.status.allocation == oldSelf.status.allocation)",message="allocation is immutable once set",fieldPath=".status.allocation"
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.allocation) || (has(self.spec) && self.spec.operationKind == 'HostAllocate')",message="allocation is allowed only for HostAllocate",fieldPath=".status.allocation"
// +kubebuilder:validation:XValidation:rule="(has(oldSelf.spec) && has(oldSelf.spec.machineSelector)) == (has(self.spec) && has(self.spec.machineSelector)) && (!has(oldSelf.spec) || !has(oldSelf.spec.machineSelector) || (has(self.spec) && has(self.spec.machineSelector) && self.spec.machineSelector == oldSelf.spec.machineSelector))",message="machineSelector cannot be changed, added or removed after creation",fieldPath=".spec.machineSelector"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || !has(oldSelf.spec.machineSelector) || (has(self.spec) && self.spec == oldSelf.spec)",message="spec is immutable for a selector operation",fieldPath=".spec"
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.suspend) || !self.spec.suspend || (has(oldSelf.spec) && has(oldSelf.spec.suspend) && oldSelf.spec.suspend)",message="suspend can be set only when the operation is created",fieldPath=".spec.suspend"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || !has(self.spec) || (has(oldSelf.spec.machineRef) ? oldSelf.spec.machineRef : '') == (has(self.spec.machineRef) ? self.spec.machineRef : '')",message="machineRef cannot be changed",fieldPath=".spec.machineRef"

// MachineOperation represents a discrete operation to be performed on a
// Machine. MachineOperations are created by CLI commands or controllers and
// processed by the appropriate agent. The in-VM agent handles operations
// like NodeReboot and HostSoftReboot, while cloud or PXE controllers handle
// operations like HostHardReboot, HostPowerOff, and HostPowerOn.
// HostAllocate and HostRelease act on a MachineAllocation, and neither the
// agent nor the cloud or PXE controllers process them.
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
// +kubebuilder:validation:Enum=NodeReboot;AgentUpgrade;AgentReset;HostHardReboot;HostSoftReboot;HostPowerOff;HostPowerOn;HostReplace;HostAllocate;HostRelease
type OperationKind string

const (
	// OperationNodeReboot restarts the node stack in place without
	// reprovisioning. Services are stopped, the node stack (nspawn machine,
	// container or host units, depending on the isolation mode) is
	// restarted, and services are brought back up. Handled by the agent.
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
	// completion after it starts again on a new boot. The agent does not start
	// it while an out-of-band host operation targeting the same Machine is
	// unfinished; out-of-band host operations do not wait for it.
	OperationHostSoftReboot OperationKind = "HostSoftReboot"

	// OperationHostPowerOff powers off the host through an out-of-band provider.
	OperationHostPowerOff OperationKind = "HostPowerOff"

	// OperationHostPowerOn powers on the host through an out-of-band provider.
	OperationHostPowerOn OperationKind = "HostPowerOn"

	// OperationHostReplace replaces the host VM through an out-of-band provider
	// and reinstalls the unbounded-agent so the node can rejoin the cluster.
	OperationHostReplace OperationKind = "HostReplace"

	// OperationHostAllocate launches the host for the MachineAllocation named
	// by spec.allocationRef. The shared machine-ops controller, metalman and
	// the agent do not process it.
	OperationHostAllocate OperationKind = "HostAllocate"

	// OperationHostRelease releases the host for the MachineAllocation named
	// by spec.allocationRef and the provider resources the allocation owns.
	// The shared machine-ops controller, metalman and the agent do not
	// process it.
	OperationHostRelease OperationKind = "HostRelease"
)

// IsAllocationOperation reports whether k acts on a MachineAllocation rather
// than on an existing host. It is true only for HostAllocate and HostRelease.
func (k OperationKind) IsAllocationOperation() bool {
	return k == OperationHostAllocate || k == OperationHostRelease
}

// MachineOperationParentUIDLabel is set on a child MachineOperation to the UID
// of the selector operation that created it. The UID is used because an
// operation name can be longer than a label value may be; the child's owner
// reference carries the parent's name.
const MachineOperationParentUIDLabel = "unbounded-cloud.io/parent-operation-uid"

// Reasons a selector operation or one of its targets reports.
const (
	// OperationReasonNoMachinesMatched means the selector matched no Machine
	// when a controller resolved it.
	OperationReasonNoMachinesMatched = "NoMachinesMatched"

	// OperationReasonNotPickedUp means no agent or provider controller
	// started a child operation within the pickup timeout, so the controller
	// that created the child failed it.
	OperationReasonNotPickedUp = "NotPickedUp"

	// OperationReasonRolloutStopped means the rollout started no further
	// targets because an earlier target failed.
	OperationReasonRolloutStopped = "RolloutStopped"

	// OperationReasonFailureLimitReached means the rollout started no
	// further targets because spec.rollout.maxFailures targets had failed.
	OperationReasonFailureLimitReached = "FailureLimitReached"

	// OperationReasonTargetFailed means every target finished and at least
	// one of them failed.
	OperationReasonTargetFailed = "TargetFailed"
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

	// MachineOperationConditionChildrenPruned is True on a finished
	// selector operation once the children it does not keep have been
	// removed. The children that remain failed, or carry a boot media
	// record. It is not set on an operation that retains all its children
	// or that matched no machine.
	MachineOperationConditionChildrenPruned = "ChildrenPruned"

	// MachineOperationConditionTargetsResolved is True on a selector
	// operation once a child operation exists for every Machine its selector
	// matched. From then on the set of machines does not change.
	MachineOperationConditionTargetsResolved = "TargetsResolved"
)

// MachineOperationMachineRefField is the field selector key for
// spec.machineRef. An agent watches and lists with it so that the API server
// sends it only the operations that name its machine.
const MachineOperationMachineRefField = "spec.machineRef"

// MachineOperationReleasedAtAnnotation is the RFC 3339 time at which the
// controller that created a child operation let it run: when it was created,
// for a child created released, or when its spec.suspend was cleared.
const MachineOperationReleasedAtAnnotation = "unbounded-cloud.io/released-at"

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
// +kubebuilder:validation:XValidation:rule="!(self.operationKind in ['HostAllocate', 'HostRelease']) || has(self.allocationRef)",message="allocationRef is required for HostAllocate and HostRelease"
// +kubebuilder:validation:XValidation:rule="!has(self.allocationRef) || self.operationKind in ['HostAllocate', 'HostRelease']",message="allocationRef is allowed only for HostAllocate and HostRelease"
// +kubebuilder:validation:XValidation:rule="!(self.operationKind in ['HostAllocate', 'HostRelease']) || (has(self.machineRef) && self.machineRef != \"\")",message="machineRef is required for HostAllocate and HostRelease"
// +kubebuilder:validation:XValidation:rule="!(self.operationKind in ['HostAllocate', 'HostRelease']) || !has(self.machineSelector)",message="machineSelector is not allowed for HostAllocate and HostRelease"
// +kubebuilder:validation:XValidation:rule="!(self.operationKind in ['HostAllocate', 'HostRelease']) || !has(self.ttlSecondsAfterFinished)",message="ttlSecondsAfterFinished is not allowed for HostAllocate and HostRelease"
// +kubebuilder:validation:XValidation:rule="(has(self.machineRef) && self.machineRef != \"\") != has(self.machineSelector)",message="exactly one of machineRef and machineSelector must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.machineSelector) || (has(self.machineSelector.matchLabels) && size(self.machineSelector.matchLabels) > 0) || (has(self.machineSelector.matchExpressions) && size(self.machineSelector.matchExpressions) > 0)",message="machineSelector must have at least one matchLabels or matchExpressions entry"
// +kubebuilder:validation:XValidation:rule="!has(self.rollout) || has(self.machineSelector)",message="rollout is allowed only with machineSelector"
// +kubebuilder:validation:XValidation:rule="!has(self.suspend) || !self.suspend || (!has(self.machineSelector) && !(self.operationKind in ['HostAllocate', 'HostRelease']))",message="suspend is allowed only on a machineRef operation that is not HostAllocate or HostRelease"
type MachineOperationSpec struct {
	// MachineRef is the name of the Machine CR this operation targets. Agents
	// and provider controllers execute only operations that have machineRef.
	// Exactly one of machineRef and machineSelector must be set.
	// +optional
	MachineRef string `json:"machineRef,omitempty"`

	// AllocationRef identifies the MachineAllocation incarnation that a
	// HostAllocate or HostRelease acts on. It is required for those kinds,
	// forbidden for all others, and immutable.
	// +optional
	AllocationRef *UIDReference `json:"allocationRef,omitempty"`

	// MachineSelector selects Machines by labels for a selector operation.
	// A controller resolves the selector against Machine labels once and
	// creates one child MachineOperation with machineRef for each matched
	// Machine. The children are the per-machine record; this operation's
	// status carries only counts, in status.summary. Agents and provider
	// controllers do not resolve the selector. Exactly one of machineRef and
	// machineSelector must be set.
	// +optional
	MachineSelector *metav1.LabelSelector `json:"machineSelector,omitempty"`

	// Rollout sets how a selector operation is paced. Allowed only with
	// machineSelector. The whole spec of a selector operation is immutable,
	// so that every machine gets the same operation.
	// +optional
	Rollout *MachineOperationRollout `json:"rollout,omitempty"`

	// Suspend holds a machineRef operation: while it is true, no agent or
	// provider controller starts the operation, and it does not make another
	// operation on the same machine wait. It can be set only when the
	// operation is created, and only cleared afterward. A selector operation
	// creates the children that may not run yet suspended, and clears the
	// field to release them at its rollout pace.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

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
	// MachineOperation. If set, the controller that owns the operation
	// deletes it this many seconds after it reaches a terminal phase: the
	// selector fan-out controller for a selector operation, and the provider
	// controller or metalman for the host operations they execute. It is not
	// enforced for an operation that an agent executes. If unset, the
	// MachineOperation is kept indefinitely.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// IsHeld reports whether o must not be started yet: a machineRef operation
// with spec.suspend set. No agent or provider controller starts a held
// operation, and it does not make another operation wait.
//
// The answer depends on the spec alone. spec.suspend can only be set when the
// operation is created and can only be cleared afterward, so a held operation
// has never started and a released one is never held again. Nothing in the
// status, which every agent may write, can release an operation.
func (o *MachineOperation) IsHeld() bool {
	return o.Spec.Suspend && !o.Spec.IsSelectorOperation()
}

// QueueTime is when o joined the queue of operations on its machine: the
// time it was released when it was created held, and its creation time
// otherwise. An operation that waited, held, for its turn in a rollout does
// not go ahead of one that started in the meantime.
func (o *MachineOperation) QueueTime() metav1.Time {
	if value := o.Annotations[MachineOperationReleasedAtAnnotation]; value != "" {
		if released, err := time.Parse(time.RFC3339, value); err == nil && released.After(o.CreationTimestamp.Time) {
			return metav1.NewTime(released)
		}
	}

	return o.CreationTimestamp
}

// IsSelectorOperation reports whether s is a selector operation. A selector
// operation has machineSelector and no machineRef.
func (s *MachineOperationSpec) IsSelectorOperation() bool {
	return s.MachineRef == "" && s.MachineSelector != nil
}

// MachineOperationRollout sets how a selector operation is paced. It is
// allowed only on an operation with machineSelector.
// +kubebuilder:validation:XValidation:rule="!has(self.maxFailures) || (has(self.onFailure) && self.onFailure == 'Continue')",message="maxFailures is allowed only with onFailure Continue"
type MachineOperationRollout struct {
	// MaxConcurrent is how many matched machines may be operated on at
	// the same time: a number (at least 1) or a percentage of the matched
	// machines from "1%" to "100%". Defaults to 1.
	// The number is bounded so that it fits the 32-bit integer the field
	// decodes into: a larger stored value would fail every typed read of
	// the object.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:XIntOrString
	// +kubebuilder:validation:XValidation:rule="(type(self) == int && self >= 1 && self <= 2147483647) || (type(self) == string && self.matches('^([1-9][0-9]?|100)%$'))",message="maxConcurrent must be an integer of at least 1 or a percentage from 1% to 100%"
	MaxConcurrent *intstr.IntOrString `json:"maxConcurrent,omitempty"`

	// OnFailure says what happens after a machine's operation fails.
	// Stop (the default) starts no further machines. Continue carries on
	// until maxFailures machines have failed.
	// +optional
	// +kubebuilder:default=Stop
	// +kubebuilder:validation:Enum=Stop;Continue
	OnFailure RolloutFailurePolicy `json:"onFailure,omitempty"`

	// MaxFailures is how many failed machines end a rollout whose onFailure
	// is Continue: once that many have failed, no further machines are
	// started and the operation fails with reason FailureLimitReached.
	// Machines already started still finish, so more than this many can
	// fail. Defaults to 1000, which is also the most allowed: a failed
	// machine keeps its child operation, and the limit bounds how many a
	// single operation can leave behind.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	MaxFailures *int32 `json:"maxFailures,omitempty"`

	// ChildRetention says which child operations are kept once the selector
	// operation has finished. Failed (the default) keeps the children that
	// failed, at most 1000 of them, and removes the rest: the ones that
	// completed, and the held ones a stopped rollout never started. A child
	// that carries a boot media record is kept as well. The counts in
	// status.summary remain. All keeps every child, for the per-machine
	// record of when each finished. Deleting the parent, or its
	// ttlSecondsAfterFinished, removes its children whatever this says.
	//
	// status.summary is the durable record of a selector operation. A child
	// may be removed at any time once it has completed, and a child need not
	// exist for a machine before its turn.
	// +optional
	// +kubebuilder:default=Failed
	// +kubebuilder:validation:Enum=Failed;All
	ChildRetention ChildRetentionPolicy `json:"childRetention,omitempty"`
}

// MaxRolloutFailures is the default and the largest value of
// spec.rollout.maxFailures, and the most failed children a finished selector
// operation keeps.
const MaxRolloutFailures = 1000

// ChildRetentionPolicy says which children of a finished selector operation
// are kept.
type ChildRetentionPolicy string

const (
	// ChildRetentionFailed keeps only the children that failed.
	ChildRetentionFailed ChildRetentionPolicy = "Failed"

	// ChildRetentionAll keeps every child.
	ChildRetentionAll ChildRetentionPolicy = "All"
)

// RolloutFailurePolicy says what a selector operation does after a machine's
// operation fails.
type RolloutFailurePolicy string

const (
	// RolloutFailurePolicyStop starts no further machines after a failure.
	RolloutFailurePolicyStop RolloutFailurePolicy = "Stop"

	// RolloutFailurePolicyContinue carries on after a failure.
	RolloutFailurePolicyContinue RolloutFailurePolicy = "Continue"
)

// MachineOperationSummary counts the children of a selector operation. Every
// child is in exactly one of held, active, complete and failed.
type MachineOperationSummary struct {
	// Total is the number of Machines the selector matched, each with one
	// child operation.
	// +kubebuilder:validation:Minimum=0
	Total int32 `json:"total"`

	// Held is the number of children not yet released to run.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Held int32 `json:"held,omitempty"`

	// Active is the number of children released and not finished.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Active int32 `json:"active,omitempty"`

	// Complete is the number of children that completed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Complete int32 `json:"complete,omitempty"`

	// Failed is the number of children that failed, including children that
	// were deleted before they finished.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Failed int32 `json:"failed,omitempty"`

	// FailedMachines names up to ten of the machines whose child failed, in
	// name order. List the children for all of them.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=10
	FailedMachines []string `json:"failedMachines,omitempty"`
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

	// Targets records the execution state of a machineRef operation's
	// machine, in one entry. A selector operation has no targets: its
	// children carry the per-machine state and Summary carries the counts.
	// +optional
	// +listType=map
	// +listMapKey=machineRef
	Targets []MachineOperationTargetStatus `json:"targets,omitempty"`

	// Summary counts the children of a selector operation by state. It is set
	// once every child exists and is the only per-rollout state kept here, so
	// the size of this object does not depend on how many machines matched.
	// +optional
	Summary *MachineOperationSummary `json:"summary,omitempty"`

	// Allocation is the provider's result for a HostAllocate. It is accepted
	// only on HostAllocate and cannot change or be removed once set.
	// +optional
	Allocation *MachineOperationAllocationResult `json:"allocation,omitempty"`

	// Conditions represent the latest available observations of the
	// operation's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachineOperationAllocationResult is the provider's result for a
// HostAllocate. Field names match MachineAllocationStatus.
type MachineOperationAllocationResult struct {
	// ProviderResourceID is the provider-native host identifier.
	// +optional
	ProviderResourceID string `json:"providerResourceID,omitempty"`

	// InstanceType is the launched instance type.
	// +optional
	InstanceType string `json:"instanceType,omitempty"`

	// Zone is the launched zone.
	// +optional
	Zone string `json:"zone,omitempty"`

	// Image is the launched host image.
	// +optional
	Image string `json:"image,omitempty"`

	// OwnedResources are the provider resources confirmed as created for the
	// allocation, with provider-native identity. A failed allocation may still
	// report them.
	// +optional
	// +listType=map
	// +listMapKey=id
	OwnedResources []ProviderOwnedResource `json:"ownedResources,omitempty"`
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

// HasOutstandingBootMedia reports whether virtual boot media for this target
// is still owned, or released before the host confirmed a boot from disk.
// The host must not be rebooted by anyone else while this holds.
func (t MachineOperationTargetStatus) HasOutstandingBootMedia() bool {
	return t.BootMedia != nil && (!t.BootMedia.Released || t.BootMediaBootUnconfirmed())
}

// BootMediaBootUnconfirmed reports whether the host may still be running the
// media installer because it has not reported booting the installed OS.
func (t MachineOperationTargetStatus) BootMediaBootUnconfirmed() bool {
	b := t.BootMedia
	if b == nil || b.Abandoned || t.CloudInitReported() {
		return false
	}

	// After cleanup the media config serves halt, so only a loader that
	// already started can still be running an installer.
	return b.ImageWritten || b.LoaderStartedAt != nil ||
		(!b.Cleaned && (b.Phase == "PoweringOn" || b.Phase == "Installing" || b.Phase == "Installed"))
}

// CloudInitReported reports whether the installed OS has reported a final
// cloud-init result. A failure also proves the host left the installer and
// booted from disk.
func (t MachineOperationTargetStatus) CloudInitReported() bool {
	cond := apimeta.FindStatusCondition(t.Conditions, MachineOperationConditionCloudInitDone)

	return cond != nil && (cond.Status == metav1.ConditionTrue ||
		(cond.Status == metav1.ConditionFalse && cond.Reason == "Failed"))
}
