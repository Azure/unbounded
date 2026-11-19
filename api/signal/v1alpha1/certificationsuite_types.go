// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type CheckID string

const (
	// MinCertificationChecks keeps certification suites non-empty.
	MinCertificationChecks = 1
	// MaxCertificationChecks bounds CEL immutability cost and persisted Run
	// status size without making the suite length part of the domain model.
	MaxCertificationChecks = 256
	// MaxCertificationStages bounds nested resolved-plan and evidence payloads
	// below normal Kubernetes object-storage limits.
	MaxCertificationStages = 32
	// MaxWorkloadPathLength bounds CEL path-validation cost across the maximum
	// Suite check and host-path counts.
	MaxWorkloadPathLength = 1024
)

// +kubebuilder:validation:XValidation:rule="duration(self.backendActiveDeadline) > duration('0s')",message="backendActiveDeadline must be positive"
// +kubebuilder:validation:XValidation:rule="self.backend != 'MPIJob' || (has(self.gpuCount) && self.gpuCount > 0)",message="MPIJob checks require a positive gpuCount"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || duration(self.minDuration) > duration('0s')",message="minDuration must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || duration(self.minDuration) <= duration(self.backendActiveDeadline)",message="minDuration must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.backendActiveDeadline)",message="maxSampleGap must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.minDuration) || !has(self.continuity) || !has(self.continuity.maxSampleGap) || duration(self.continuity.maxSampleGap) <= duration(self.minDuration)",message="maxSampleGap must not exceed minDuration"
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.coverageWindow) || duration(self.execution.coverageWindow) <= duration(self.backendActiveDeadline)",message="coverageWindow must not exceed backendActiveDeadline"
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || self.execution.mode == 'RunOnce' || duration(self.backendActiveDeadline).getMilliseconds() % 1000 == 0",message="repeated execution requires a whole-second backendActiveDeadline"
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
	// MinDuration is the legacy minimum observed backend lifetime. It does not
	// establish sustained workload activity without an explicit Continuous or
	// Periodic execution policy.
	MinDuration *metav1.Duration `json:"minDuration,omitempty"`
	// Continuity declares the evidence required to prove uninterrupted execution.
	Continuity *ContinuityPolicy `json:"continuity,omitempty"`
	// Execution declares runner-owned stage scheduling. When omitted, the check
	// has legacy RunOnce semantics and is ineligible for sustained-duration
	// qualification.
	Execution *CheckExecutionPolicy `json:"execution,omitempty"`
}

// +kubebuilder:validation:Enum=Pod;MPIJob
type ExecutionBackend string

const (
	ExecutionBackendPod    ExecutionBackend = "Pod"
	ExecutionBackendMPIJob ExecutionBackend = "MPIJob"
)

// +kubebuilder:validation:Enum=Startup;Validation;Terminal
type CheckStageRole string

const (
	CheckStageRoleStartup    CheckStageRole = "Startup"
	CheckStageRoleValidation CheckStageRole = "Validation"
	CheckStageRoleTerminal   CheckStageRole = "Terminal"
)

// +kubebuilder:validation:Enum=RunOnce;Continuous;Periodic
type CheckExecutionMode string

const (
	CheckExecutionModeRunOnce    CheckExecutionMode = "RunOnce"
	CheckExecutionModeContinuous CheckExecutionMode = "Continuous"
	CheckExecutionModePeriodic   CheckExecutionMode = "Periodic"
)

// +kubebuilder:validation:XValidation:rule="self.role == 'Validation' || self.mode == 'RunOnce'",message="Startup and Terminal stages must use RunOnce mode"
// +kubebuilder:validation:XValidation:rule="self.mode != 'RunOnce' || (!has(self.coverageWindow) && !has(self.iterationTimeout) && !has(self.interval) && !has(self.minExecutions) && !has(self.maxGap))",message="RunOnce mode must not declare repeated-execution fields"
// +kubebuilder:validation:XValidation:rule="self.mode == 'RunOnce' || (has(self.coverageWindow) && has(self.iterationTimeout) && has(self.minExecutions) && has(self.maxGap))",message="Continuous and Periodic modes require coverageWindow, iterationTimeout, minExecutions, and maxGap"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Periodic' || has(self.interval)",message="Periodic mode requires interval"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Periodic' || !has(self.interval)",message="only Periodic mode may declare interval"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || duration(self.coverageWindow) > duration('0s')",message="coverageWindow must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || duration(self.coverageWindow).getMilliseconds() % 1000 == 0",message="coverageWindow must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.iterationTimeout) || duration(self.iterationTimeout) > duration('0s')",message="iterationTimeout must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.iterationTimeout) || duration(self.iterationTimeout).getMilliseconds() % 1000 == 0",message="iterationTimeout must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || !has(self.iterationTimeout) || duration(self.iterationTimeout) <= duration(self.coverageWindow)",message="iterationTimeout must not exceed coverageWindow"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || duration(self.interval) > duration('0s')",message="interval must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || duration(self.interval).getMilliseconds() % 1000 == 0",message="interval must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.iterationTimeout) || duration(self.iterationTimeout) <= duration(self.interval)",message="Periodic iterationTimeout must not exceed interval"
// +kubebuilder:validation:XValidation:rule="!has(self.maxGap) || duration(self.maxGap) > duration('0s')",message="maxGap must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.maxGap) || duration(self.maxGap).getMilliseconds() % 1000 == 0",message="maxGap must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || !has(self.maxGap) || duration(self.maxGap) <= duration(self.coverageWindow)",message="maxGap must not exceed coverageWindow"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.maxGap) || duration(self.interval) <= duration(self.maxGap)",message="Periodic maxGap must not be less than interval"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.coverageWindow) || !has(self.iterationTimeout) || !has(self.minExecutions) || duration(self.iterationTimeout).getMilliseconds() + duration(self.interval).getMilliseconds() * (self.minExecutions - 1) <= duration(self.coverageWindow).getMilliseconds()",message="minExecutions must fit within coverageWindow"
type CheckExecutionPolicy struct {
	Role CheckStageRole     `json:"role"`
	Mode CheckExecutionMode `json:"mode"`
	// CoverageWindow is the sustained activity window owned by the runner.
	CoverageWindow *metav1.Duration `json:"coverageWindow,omitempty"`
	// IterationTimeout bounds each runner-owned script invocation.
	IterationTimeout *metav1.Duration `json:"iterationTimeout,omitempty"`
	// Interval is the target separation between Periodic invocation starts.
	Interval *metav1.Duration `json:"interval,omitempty"`
	// MinExecutions is the minimum completed invocation count in the window.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=1000000
	MinExecutions *int32 `json:"minExecutions,omitempty"`
	// MaxGap bounds the observed gap between consecutive invocation starts.
	MaxGap *metav1.Duration `json:"maxGap,omitempty"`
}

type ContinuityPolicy struct {
	// MaxSampleGap is the maximum permitted gap between consecutive evidence samples.
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="maxSampleGap must be positive"
	MaxSampleGap *metav1.Duration `json:"maxSampleGap,omitempty"`
	// RequireSameBootID rejects evidence spanning a participant reboot.
	RequireSameBootID bool `json:"requireSameBootID,omitempty"`
	// RequirePerDeviceCoverage requires evidence for every device in the starting inventory.
	RequirePerDeviceCoverage bool `json:"requirePerDeviceCoverage,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.mountPath.startsWith('/') && !self.mountPath.contains('//') && !self.mountPath.contains('/./') && !self.mountPath.contains('/../') && !self.mountPath.endsWith('/.') && !self.mountPath.endsWith('/..')",message="mountPath must be a clean absolute path"
// +kubebuilder:validation:XValidation:rule="self.mountPath != '/scripts' && !self.mountPath.startsWith('/scripts/') && self.mountPath != '/results' && !self.mountPath.startsWith('/results/')",message="mountPath must not replace or shadow a reserved Project Signal mount"
// +kubebuilder:validation:XValidation:rule="(self.path == '/proc/cpuinfo' || self.path == '/proc/meminfo' || self.path == '/proc/version' || self.path == '/proc/sys/kernel/random/boot_id' || self.path == '/sys' || self.path.startsWith('/sys/') || self.path == '/var/lib/project-signal' || self.path.startsWith('/var/lib/project-signal/') || self.path == '/var/log' || self.path == '/run/log' || self.path == '/etc/machine-id') && !self.path.contains('//') && !self.path.contains('/./') && !self.path.contains('/../') && !self.path.endsWith('/.') && !self.path.endsWith('/..')",message="path must be an approved proc leaf, journal path, or a clean path under /sys or /var/lib/project-signal"
// +kubebuilder:validation:XValidation:rule="!(self.path.startsWith('/proc/') || self.path == '/sys' || self.path.startsWith('/sys/') || self.path == '/var/log' || self.path == '/run/log' || self.path == '/etc/machine-id') || self.readOnly",message="proc, sys, and journal host paths must be read-only"
type WorkloadHostPath struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	MountPath string `json:"mountPath"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
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

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type CertificationTestReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// ContentDigest is the accepted canonical CertificationTest definition.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
}

type StageParameterBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name  string     `json:"name"`
	Value TypedValue `json:"value"`
}

type StageParticipantRoleBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Role string `json:"role"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	Count int32 `json:"count"`
}

// +kubebuilder:validation:Enum=InfrastructureFailure
type RetryableFailureClass string

const RetryableFailureClassInfrastructure RetryableFailureClass = "InfrastructureFailure"

type StageRetryPolicy struct {
	// MaxAttempts includes the initial attempt.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxAttempts int32 `json:"maxAttempts"`
	// +kubebuilder:validation:MaxItems=1
	// +listType=set
	RetryableFailures []RetryableFailureClass `json:"retryableFailures,omitempty"`
}

// +kubebuilder:validation:Enum=Succeeded;Completed
type StageDependencyCondition string

const (
	StageDependencyConditionSucceeded StageDependencyCondition = "Succeeded"
	StageDependencyConditionCompleted StageDependencyCondition = "Completed"
)

type StageDependency struct {
	// Stage is the stable name of another stage in this Suite.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Stage string `json:"stage"`
	// Condition defaults to Succeeded. Completed allows terminal cleanup or
	// evidence stages to run after any terminal dependency outcome.
	Condition StageDependencyCondition `json:"condition,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.mode != 'RunOnce' || (!has(self.coverageWindow) && !has(self.interval) && !has(self.minExecutions) && !has(self.maxGap))",message="RunOnce schedules must not declare repeated-execution fields"
// +kubebuilder:validation:XValidation:rule="self.mode == 'RunOnce' || (has(self.coverageWindow) && has(self.minExecutions) && has(self.maxGap))",message="Continuous and Periodic schedules require coverageWindow, minExecutions, and maxGap"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Periodic' || has(self.interval)",message="Periodic schedules require interval"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Periodic' || !has(self.interval)",message="only Periodic schedules may declare interval"
// +kubebuilder:validation:XValidation:rule="duration(self.invocationTimeout) > duration('0s')",message="invocationTimeout must be positive"
// +kubebuilder:validation:XValidation:rule="duration(self.invocationTimeout).getMilliseconds() % 1000 == 0",message="invocationTimeout must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || duration(self.coverageWindow) > duration('0s')",message="coverageWindow must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || duration(self.coverageWindow).getMilliseconds() % 1000 == 0",message="coverageWindow must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || duration(self.invocationTimeout) <= duration(self.coverageWindow)",message="invocationTimeout must not exceed coverageWindow"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || duration(self.interval) > duration('0s')",message="interval must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || duration(self.interval).getMilliseconds() % 1000 == 0",message="interval must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || duration(self.invocationTimeout) <= duration(self.interval)",message="Periodic invocationTimeout must not exceed interval"
// +kubebuilder:validation:XValidation:rule="!has(self.maxGap) || duration(self.maxGap) > duration('0s')",message="maxGap must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.maxGap) || duration(self.maxGap).getMilliseconds() % 1000 == 0",message="maxGap must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.coverageWindow) || !has(self.maxGap) || duration(self.maxGap) <= duration(self.coverageWindow)",message="maxGap must not exceed coverageWindow"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.maxGap) || duration(self.interval) <= duration(self.maxGap)",message="Periodic maxGap must not be less than interval"
// +kubebuilder:validation:XValidation:rule="!has(self.interval) || !has(self.coverageWindow) || !has(self.minExecutions) || duration(self.invocationTimeout).getMilliseconds() + duration(self.interval).getMilliseconds() * (self.minExecutions - 1) <= duration(self.coverageWindow).getMilliseconds()",message="minExecutions must fit within coverageWindow"
type StageSchedule struct {
	Mode CheckExecutionMode `json:"mode"`
	// InvocationTimeout bounds every invocation, including RunOnce.
	InvocationTimeout metav1.Duration  `json:"invocationTimeout"`
	CoverageWindow    *metav1.Duration `json:"coverageWindow,omitempty"`
	Interval          *metav1.Duration `json:"interval,omitempty"`
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=1000000
	MinExecutions *int32           `json:"minExecutions,omitempty"`
	MaxGap        *metav1.Duration `json:"maxGap,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.perDeviceCoverageInterval) || self.schedule.mode != 'RunOnce'",message="perDeviceCoverageInterval requires Continuous or Periodic scheduling"
// +kubebuilder:validation:XValidation:rule="!has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval) > duration('0s')",message="perDeviceCoverageInterval must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval).getMilliseconds() % 1000 == 0",message="perDeviceCoverageInterval must use whole-second precision"
// +kubebuilder:validation:XValidation:rule="!has(self.schedule.coverageWindow) || !has(self.perDeviceCoverageInterval) || duration(self.perDeviceCoverageInterval) <= duration(self.schedule.coverageWindow)",message="perDeviceCoverageInterval must not exceed coverageWindow"
type CertificationStage struct {
	// Name is the stable operator-defined stage identity within the Suite.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	DisplayName string                     `json:"displayName,omitempty"`
	Test        CertificationTestReference `json:"test"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Parameters []StageParameterBinding `json:"parameters,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=role
	ParticipantRoles []StageParticipantRoleBinding `json:"participantRoles,omitempty"`
	Schedule         StageSchedule                 `json:"schedule"`
	Retry            *StageRetryPolicy             `json:"retry,omitempty"`
	// DependsOn overrides the default previous-phase barrier for this stage.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=stage
	DependsOn []StageDependency `json:"dependsOn,omitempty"`
	// Optional stages are reported but do not determine phase or Run success.
	Optional bool `json:"optional,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Assertions []TestAssertion `json:"assertions,omitempty"`
	// PerDeviceCoverageInterval requires every starting device to be covered in
	// each interval represented by durable stage evidence.
	PerDeviceCoverageInterval *metav1.Duration `json:"perDeviceCoverageInterval,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.stages.all(x, self.stages.filter(y, y.name == x.name).size() == 1)",message="stage names must be unique within a phase"
// +kubebuilder:validation:XValidation:rule="self.role == 'Validation' || self.stages.all(x, x.schedule.mode == 'RunOnce')",message="Startup and Terminal phases may contain only RunOnce stages"
type CertificationPhase struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=128
	DisplayName string         `json:"displayName,omitempty"`
	Role        CheckStageRole `json:"role"`
	// Stages execute concurrently. The phase completes after all required
	// stages reach terminal verdicts.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Stages []CertificationStage `json:"stages"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.phases) && size(self.phases) > 0) != (has(self.checks) && size(self.checks) > 0)",message="exactly one of phases or legacy checks must be configured"
// +kubebuilder:validation:XValidation:rule="!has(self.checks) || size(self.checks) == 0 || has(self.scriptBundle)",message="legacy checks require scriptBundle"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || size(self.phases) == 0 || !has(self.scriptBundle)",message="phase-based Suites must not use legacy scriptBundle"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || self.phases.all(x, self.phases.filter(y, y.name == x.name).size() == 1)",message="phase names must be unique"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || self.phases.map(x, x.stages.size()).sum() <= 32",message="total stage count must not exceed 32"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || self.phases.filter(x, x.role == 'Startup').size() <= 1",message="at most one Startup phase is allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || self.phases.filter(x, x.role == 'Terminal').size() <= 1",message="at most one Terminal phase is allowed"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || self.phases.filter(x, x.role == 'Startup').size() == 0 || self.phases[0].role == 'Startup'",message="Startup phase must be first"
// +kubebuilder:validation:XValidation:rule="!has(self.phases) || self.phases.filter(x, x.role == 'Terminal').size() == 0 || self.phases[self.phases.size() - 1].role == 'Terminal'",message="Terminal phase must be last"
type CertificationSuiteSpec struct {
	// ScriptBundle is retained only for backward-compatible legacy checks.
	ScriptBundle *ScriptBundleSource `json:"scriptBundle,omitempty"`
	// Checks is the ordered executable test plan. A check's position defines
	// its stable ID. New Suites should use phases and named stages.
	// +kubebuilder:validation:MaxItems=256
	// +listType=atomic
	Checks []CertificationCheck `json:"checks,omitempty"`
	// Phases execute in order. Stages within a phase execute concurrently.
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Phases []CertificationPhase `json:"phases,omitempty"`
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
