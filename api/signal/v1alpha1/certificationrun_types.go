// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Inconclusive;Canceled
type RunPhase string

const (
	ReservationNodeUIDLabel = "signal.unbounded-cloud.io/reserved-node-uid"
	RunConditionCordoned    = "Cordoned"
	RunConditionCleanup     = "WorkloadCleanupComplete"
	RunConditionReleased    = "Released"

	RunPhasePending      RunPhase = "Pending"
	RunPhaseRunning      RunPhase = "Running"
	RunPhaseSucceeded    RunPhase = "Succeeded"
	RunPhaseFailed       RunPhase = "Failed"
	RunPhaseInconclusive RunPhase = "Inconclusive"
	RunPhaseCancelled    RunPhase = "Canceled"
)

func (p RunPhase) Terminal() bool {
	switch p {
	case RunPhaseSucceeded, RunPhaseFailed, RunPhaseInconclusive, RunPhaseCancelled:
		return true
	default:
		return false
	}
}

// +kubebuilder:validation:XValidation:rule="size(self.nodeUID) > 0",message="nodeUID is required"
type Participant struct {
	// Index is derived from the participant's position and is never serialized.
	Index int32 `json:"-"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	NodeName string    `json:"nodeName"`
	NodeUID  types.UID `json:"nodeUID"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type BackendResourceReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=0
	// +kubebuilder:validation:items:Maximum=7
	// +listType=set
	ParticipantIndexes []int32 `json:"participantIndexes"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type PinnedProfile struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type PinnedSuite struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type ScriptBundleReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// Digest is the aggregate algorithm-qualified SHA-256 digest derived from
	// the selected script bytes in check order.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

type ScriptSnapshot struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
	// Digest is the algorithm-qualified SHA-256 digest of the selected script bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="self.backend != 'MPIJob' || (has(self.gpuCount) && self.gpuCount > 0)",message="MPIJob checks require a positive gpuCount"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || duration(self.minDuration) > duration('0s')",message="minDuration must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || duration(self.minDuration) <= duration(self.backendActiveDeadline)",message="minDuration must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.backendActiveDeadline)",message="maxSampleGap must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || !has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.minDuration)",message="maxSampleGap must not exceed minDuration"
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.coverageWindow) || duration(self.execution.coverageWindow) <= duration(self.backendActiveDeadline)",message="coverageWindow must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || self.execution.mode == 'RunOnce' || duration(self.backendActiveDeadline).getMilliseconds() % 1000 == 0",message="repeated execution requires a whole-second backendActiveDeadline"
type CertificationCheckSnapshot struct {
	// ID and Order are derived from the check's position and are never serialized.
	ID     CheckID        `json:"-"`
	Order  int32          `json:"-"`
	Script ScriptSnapshot `json:"script"`
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	RunnerImage    string           `json:"runnerImage"`
	Backend        ExecutionBackend `json:"backend"`
	WorkloadPolicy WorkloadPolicy   `json:"workloadPolicy,omitempty"`
	// +kubebuilder:validation:Minimum=0
	GPUCount              int32                 `json:"gpuCount,omitempty"`
	BackendActiveDeadline metav1.Duration       `json:"backendActiveDeadline"`
	MinDuration           *metav1.Duration      `json:"minDuration,omitempty"`
	Continuity            *ContinuityPolicy     `json:"continuity,omitempty"`
	Execution             *CheckExecutionPolicy `json:"execution,omitempty"`
}

// +kubebuilder:validation:Enum=Image;Model;Tokenizer;Artifact;WorkflowTemplate;ResolverOutput
type ResolvedInputKind string

const (
	ResolvedInputKindImage            ResolvedInputKind = "Image"
	ResolvedInputKindModel            ResolvedInputKind = "Model"
	ResolvedInputKindTokenizer        ResolvedInputKind = "Tokenizer"
	ResolvedInputKindArtifact         ResolvedInputKind = "Artifact"
	ResolvedInputKindWorkflowTemplate ResolvedInputKind = "WorkflowTemplate"
	ResolvedInputKindResolverOutput   ResolvedInputKind = "ResolverOutput"
)

// +kubebuilder:validation:Enum=Complete;Incomplete
type ResolvedInputState string

const (
	ResolvedInputStateComplete   ResolvedInputState = "Complete"
	ResolvedInputStateIncomplete ResolvedInputState = "Incomplete"
)

// +kubebuilder:validation:XValidation:rule="self.state != 'Complete' || (has(self.resolved) && size(self.resolved.trim()) > 0 && has(self.digest))",message="complete inputs require an immutable resolved identity and digest"
// +kubebuilder:validation:XValidation:rule="self.state != 'Complete' || !has(self.incompleteReason)",message="complete inputs must not declare incompleteReason"
// +kubebuilder:validation:XValidation:rule="self.state != 'Incomplete' || (has(self.incompleteReason) && size(self.incompleteReason.trim()) > 0 && !has(self.digest))",message="incomplete inputs require incompleteReason and must not claim a digest"
type ResolvedInputIdentity struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string            `json:"name"`
	Kind ResolvedInputKind `json:"kind"`
	// Requested preserves the authoring input, including a mutable channel such
	// as latest. It never authorizes execution by itself.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Requested string             `json:"requested"`
	State     ResolvedInputState `json:"state"`
	// Resolved is the immutable revision or identity selected before Run start.
	// +kubebuilder:validation:MaxLength=512
	Resolved string `json:"resolved,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	IncompleteReason string `json:"incompleteReason,omitempty"`
}

type ResolvedSecurityRequirements struct {
	Privileged  bool `json:"privileged,omitempty"`
	HostNetwork bool `json:"hostNetwork,omitempty"`
	IPCLock     bool `json:"ipcLock,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[A-Z][A-Z0-9_]*$`
	Capabilities []string `json:"capabilities,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=mountPath
	HostPaths []WorkloadHostPath `json:"hostPaths,omitempty"`
}

type ResolvedCertificationTest struct {
	Identity TestIdentitySnapshot `json:"identity"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ParameterSchemaDigest string `json:"parameterSchemaDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	MetricSchemaDigest string `json:"metricSchemaDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SecurityRequirementsDigest string `json:"securityRequirementsDigest"`
	// Metrics are retained so the immutable Run remains self-contained when the
	// source CertificationTest changes or is deleted.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Metrics  []TestMetricDefinition       `json:"metrics"`
	Topology TestTopology                 `json:"topology"`
	Driver   CertificationTestDriver      `json:"driver"`
	Security ResolvedSecurityRequirements `json:"security,omitempty"`
}

type ResolvedStageParticipantRoleBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role"`
	// ParticipantIndexes freezes the ordered Run participants assigned to this role.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Minimum=0
	// +kubebuilder:validation:items:Maximum=7
	// +listType=set
	ParticipantIndexes []int32 `json:"participantIndexes"`
}

// +kubebuilder:validation:XValidation:rule="!self.qualificationEligible || (self.test.identity.trust == 'Approved' && (!has(self.diagnosticOnly) || !self.diagnosticOnly))",message="qualification-eligible stages require an approved test and must not be diagnostic-only"
// +kubebuilder:validation:XValidation:rule="!self.qualificationEligible || !has(self.resolvedInputs) || self.resolvedInputs.all(i, i.state == 'Complete')",message="qualification-eligible stages require complete resolved inputs"
// +kubebuilder:validation:XValidation:rule="!has(self.resolvedInputs) || self.resolvedInputs.all(i, i.state != 'Incomplete') || (has(self.diagnosticOnly) && self.diagnosticOnly && !self.qualificationEligible)",message="stages with incomplete inputs must be diagnostic-only"
// +kubebuilder:validation:XValidation:rule="self.test.identity.trust == 'Approved' || (has(self.diagnosticOnly) && self.diagnosticOnly && !self.qualificationEligible)",message="unapproved tests are diagnostic-only and cannot grant qualification credit"
// +kubebuilder:validation:XValidation:rule="self.test.identity.trust != 'Rejected' && self.test.identity.trust != 'Revoked'",message="rejected or revoked tests cannot be resolved into new Runs"
// +kubebuilder:validation:XValidation:rule="((!has(self.test.topology.roles) || size(self.test.topology.roles) == 0) && (!has(self.participantRoles) || size(self.participantRoles) == 0)) || (has(self.test.topology.roles) && size(self.test.topology.roles) > 0 && has(self.participantRoles) && size(self.participantRoles) > 0)",message="role-aware tests require resolved participant role assignments"
// +kubebuilder:validation:XValidation:rule="!has(self.participantRoles) || (has(self.test.topology.roles) && self.participantRoles.all(b, self.test.topology.roles.exists(r, r.name == b.role)))",message="resolved participant roles must exist in the test topology"
// +kubebuilder:validation:XValidation:rule="!has(self.test.topology.roles) || !has(self.participantRoles) || self.test.topology.roles.all(r, (self.participantRoles.filter(b, b.role == r.name).size() == 0 && (!has(r.minCount) || r.minCount == 0)) || (self.participantRoles.filter(b, b.role == r.name).size() == 1 && self.participantRoles.filter(b, b.role == r.name)[0].participantIndexes.size() >= (has(r.minCount) ? r.minCount : 0) && self.participantRoles.filter(b, b.role == r.name)[0].participantIndexes.size() <= r.maxCount))",message="resolved participant role counts must satisfy the test topology"
// +kubebuilder:validation:XValidation:rule="!has(self.participantRoles) || self.participantRoles.map(r, r.participantIndexes.size()).sum() >= self.test.topology.minParticipants && self.participantRoles.map(r, r.participantIndexes.size()).sum() <= self.test.topology.maxParticipants",message="resolved participant role assignments must satisfy participant topology bounds"
// +kubebuilder:validation:XValidation:rule="!has(self.participantRoles) || self.participantRoles.all(r, r.participantIndexes.all(i, self.participantRoles.filter(o, i in o.participantIndexes).size() == 1))",message="a participant may fill only one role in a stage"
// +kubebuilder:validation:XValidation:rule="!has(self.perDeviceCoverageInterval) || self.schedule.mode != 'RunOnce'",message="perDeviceCoverageInterval requires Continuous or Periodic scheduling"
// +kubebuilder:validation:XValidation:rule="!has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval) > duration('0s')",message="perDeviceCoverageInterval must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval).getMilliseconds() % 1000 == 0",message="perDeviceCoverageInterval must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.schedule.coverageWindow) || !has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval) <= duration(self.schedule.coverageWindow)",message="perDeviceCoverageInterval must not exceed coverageWindow"
type ResolvedStagePlan struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	DisplayName string                    `json:"displayName,omitempty"`
	Test        ResolvedCertificationTest `json:"test"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Parameters []StageParameterBinding `json:"parameters,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=role
	ParticipantRoles []ResolvedStageParticipantRoleBinding `json:"participantRoles,omitempty"`
	Schedule         StageSchedule                         `json:"schedule"`
	Retry            *StageRetryPolicy                     `json:"retry,omitempty"`
	Optional         bool                                  `json:"optional,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Assertions                []TestAssertion  `json:"assertions,omitempty"`
	PerDeviceCoverageInterval *metav1.Duration `json:"perDeviceCoverageInterval,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	ResolvedInputs        []ResolvedInputIdentity `json:"resolvedInputs,omitempty"`
	QualificationEligible bool                    `json:"qualificationEligible"`
	DiagnosticOnly        bool                    `json:"diagnosticOnly,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.role == 'Validation' || self.stages.all(x, x.schedule.mode == 'RunOnce')",message="Startup and Terminal phases may contain only RunOnce stages"
type ResolvedPhasePlan struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	DisplayName string         `json:"displayName,omitempty"`
	Role        CheckStageRole `json:"role"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Stages []ResolvedStagePlan `json:"stages"`
}

// +kubebuilder:validation:XValidation:rule="self.phases.map(x, x.stages.size()).sum() <= 32",message="total stage count must not exceed 32"
type ResolvedCertificationPlan struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Phases []ResolvedPhasePlan `json:"phases"`
}

type ExecutionPlanProvenance struct {
	// ControllerImage is the immutable controller image that resolved the plan.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	ControllerImage string `json:"controllerImage,omitempty"`
	// AdapterImage is the immutable adapter image included in the execution plan.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	AdapterImage string `json:"adapterImage"`
	// WorkflowTemplateDigest identifies the rendered workflow/template semantics.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	WorkflowTemplateDigest string `json:"workflowTemplateDigest"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	ResolverImages []string `json:"resolverImages,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.suite == oldSelf.suite",message="suite is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants == oldSelf.participants",message="participants are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.scriptBundle) == has(oldSelf.scriptBundle) && (!has(self.scriptBundle) || self.scriptBundle == oldSelf.scriptBundle)",message="scriptBundle is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.checks) == has(oldSelf.checks) && (!has(self.checks) || self.checks == oldSelf.checks)",message="checks are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.environment) == has(oldSelf.environment) && (!has(self.environment) || self.environment == oldSelf.environment)",message="environment is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.resolvedPlan) == has(oldSelf.resolvedPlan) && (!has(self.resolvedPlan) || self.resolvedPlan == oldSelf.resolvedPlan)",message="resolvedPlan is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.planDigest) == has(oldSelf.planDigest) && (!has(self.planDigest) || self.planDigest == oldSelf.planDigest)",message="planDigest is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.planProvenance) == has(oldSelf.planProvenance) && (!has(self.planProvenance) || self.planProvenance == oldSelf.planProvenance)",message="planProvenance is immutable"
// +kubebuilder:validation:XValidation:rule="self.participants.all(x, self.participants.filter(y, y.nodeName == x.nodeName).size() == 1)",message="participants must use unique nodeName values"
// +kubebuilder:validation:XValidation:rule="(has(self.resolvedPlan) && size(self.resolvedPlan.phases) > 0) != (has(self.checks) && size(self.checks) > 0)",message="exactly one of resolvedPlan or legacy checks must be configured"
// +kubebuilder:validation:XValidation:rule="!has(self.checks) || size(self.checks) == 0 || has(self.scriptBundle)",message="legacy checks require scriptBundle"
// +kubebuilder:validation:XValidation:rule="!has(self.resolvedPlan) || !has(self.scriptBundle)",message="resolvedPlan Runs must not use legacy scriptBundle"
// +kubebuilder:validation:XValidation:rule="!has(self.resolvedPlan) || (has(self.planDigest) && has(self.planProvenance) && has(self.planProvenance.controllerImage))",message="resolvedPlan Runs require planDigest and complete planProvenance"
// +kubebuilder:validation:XValidation:rule="!has(self.resolvedPlan) || self.resolvedPlan.phases.all(p, p.stages.all(s, !has(s.participantRoles) || s.participantRoles.all(r, r.participantIndexes.all(i, i < size(self.participants)))))",message="resolved stage participant indexes must reference Run participants"
// +kubebuilder:validation:XValidation:rule="!has(self.resolvedPlan) || self.resolvedPlan.phases.all(p, p.stages.all(s, (has(s.participantRoles) && size(s.participantRoles) > 0) || (size(self.participants) >= s.test.topology.minParticipants && size(self.participants) <= s.test.topology.maxParticipants)))",message="resolved stages without explicit roles use all Run participants and must satisfy the test topology"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancel) || !oldSelf.cancel || (has(self.cancel) && self.cancel)",message="cancel cannot be cleared"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.release) || !oldSelf.release || (has(self.release) && self.release)",message="release cannot be cleared"
type CertificationRunSpec struct {
	Profile PinnedProfile `json:"profile"`
	Suite   PinnedSuite   `json:"suite"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Participants []Participant `json:"participants"`
	// ScriptBundle is retained only for backward-compatible legacy check Runs.
	ScriptBundle *ScriptBundleReference `json:"scriptBundle,omitempty"`
	// Checks is the immutable ordered snapshot of the Suite. A check's position
	// defines its stable ID. New Runs use resolvedPlan.
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks []CertificationCheckSnapshot `json:"checks,omitempty"`
	// Environment is the immutable Profile environment binding snapshot.
	Environment *CertificationEnvironmentBindings `json:"environment,omitempty"`
	// ResolvedPlan freezes every test, driver, parameter, assertion, schedule,
	// mutable authoring input, and trust decision before execution.
	ResolvedPlan *ResolvedCertificationPlan `json:"resolvedPlan,omitempty"`
	// PlanDigest is the canonical digest of all execution-affecting inputs.
	// It is optional only for backward decoding of Runs created before this contract.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest,omitempty"`
	// PlanProvenance pins shared execution inputs not carried by individual checks.
	PlanProvenance *ExecutionPlanProvenance `json:"planProvenance,omitempty"`
	Cancel         bool                     `json:"cancel,omitempty"`
	Release        bool                     `json:"release,omitempty"`
}

type WorkflowStatus struct {
	Name  string `json:"name,omitempty"`
	UID   string `json:"uid,omitempty"`
	Phase string `json:"phase,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.uri) || oldSelf.uri.size() == 0 || (has(self.uri) && self.uri == oldSelf.uri)",message="evidence URI is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.digest) || oldSelf.digest.size() == 0 || (has(self.digest) && self.digest == oldSelf.digest)",message="evidence digest is write-once"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.identity) || (has(self.identity) && self.identity == oldSelf.identity)",message="evidence identity is write-once"
type EvidenceStatus struct {
	// +kubebuilder:validation:MaxLength=2048
	URI string `json:"uri,omitempty"`
	// Digest is the algorithm-qualified SHA-256 digest of the evidence bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
	// Identity contains bounded qualification inputs. Large measurements remain
	// in the durable evidence document addressed by URI and Digest.
	Identity *QualificationEvidenceIdentity `json:"identity,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Inconclusive;Skipped
type CheckPhase string

const (
	CheckPhasePending      CheckPhase = "Pending"
	CheckPhaseRunning      CheckPhase = "Running"
	CheckPhaseSucceeded    CheckPhase = "Succeeded"
	CheckPhaseFailed       CheckPhase = "Failed"
	CheckPhaseInconclusive CheckPhase = "Inconclusive"
	CheckPhaseSkipped      CheckPhase = "Skipped"
)

type BackendEvidenceStatus struct {
	Complete bool `json:"complete,omitempty"`
	// Digest identifies the complete backend-evidence v1 document retained in
	// the durable evidence artifact.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest,omitempty"`
	// Content is transient input for the durable evidence artifact.
	Content string `json:"-"`
}

type CheckCleanupStatus struct {
	Complete bool   `json:"-"`
	Reason   string `json:"-"`
}

type CheckStatus struct {
	// Internal fields are derived or transient and are never persisted in CR status.
	ID               CheckID    `json:"-"`
	Order            int32      `json:"-"`
	WorkflowNodeName string     `json:"-"`
	Phase            CheckPhase `json:"phase"`
	Verdict          string     `json:"-"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// Measurements, backend identities, backend evidence, and timestamps are
	// transient inputs to the durable evidence document, not CR status.
	Measurements     string                     `json:"-"`
	BackendResources []BackendResourceReference `json:"-"`
	BackendEvidence  BackendEvidenceStatus      `json:"-"`
	StartedAt        *metav1.Time               `json:"-"`
	FinishedAt       *metav1.Time               `json:"-"`
	Cleanup          CheckCleanupStatus         `json:"-"`
}

type CertificationRunStatus struct {
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	Phase              RunPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason   string         `json:"reason,omitempty"`
	Workflow WorkflowStatus `json:"workflow,omitempty"`
	Evidence EvidenceStatus `json:"evidence,omitempty"`
	// Checks remains positionally aligned with spec.checks.
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks []CheckStatus `json:"checks,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.evidence) || !has(self.status.evidence.identity) || (has(self.spec.planDigest) && self.status.evidence.identity.planDigest == self.spec.planDigest)",message="evidence identity requires and must match spec.planDigest"
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=certrun
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Workflow",type=string,JSONPath=`.status.workflow.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CertificationRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertificationRunSpec   `json:"spec"`
	Status CertificationRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CertificationRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationRun `json:"items"`
}
