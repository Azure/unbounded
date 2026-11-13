// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	MaxTestParameters     = 16
	MaxTestMetrics        = 16
	MaxTestRoles          = 8
	MaxTestArtifactBytes  = 95 * 1024
	MaxTestArtifactsBytes = 320 * 1024
	MaxTypedStringLength  = 65536
)

// +kubebuilder:validation:Enum=String;Integer;Decimal;Boolean;Duration;Quantity
type TypedValueType string

const (
	TypedValueTypeString   TypedValueType = "String"
	TypedValueTypeInteger  TypedValueType = "Integer"
	TypedValueTypeDecimal  TypedValueType = "Decimal"
	TypedValueTypeBoolean  TypedValueType = "Boolean"
	TypedValueTypeDuration TypedValueType = "Duration"
	TypedValueTypeQuantity TypedValueType = "Quantity"
)

// +kubebuilder:validation:XValidation:rule="(self.type == 'String' && has(self.stringValue) && !has(self.integerValue) && !has(self.decimalValue) && !has(self.booleanValue) && !has(self.durationValue) && !has(self.quantityValue)) || (self.type == 'Integer' && !has(self.stringValue) && has(self.integerValue) && !has(self.decimalValue) && !has(self.booleanValue) && !has(self.durationValue) && !has(self.quantityValue)) || (self.type == 'Decimal' && !has(self.stringValue) && !has(self.integerValue) && has(self.decimalValue) && !has(self.booleanValue) && !has(self.durationValue) && !has(self.quantityValue)) || (self.type == 'Boolean' && !has(self.stringValue) && !has(self.integerValue) && !has(self.decimalValue) && has(self.booleanValue) && !has(self.durationValue) && !has(self.quantityValue)) || (self.type == 'Duration' && !has(self.stringValue) && !has(self.integerValue) && !has(self.decimalValue) && !has(self.booleanValue) && has(self.durationValue) && !has(self.quantityValue)) || (self.type == 'Quantity' && !has(self.stringValue) && !has(self.integerValue) && !has(self.decimalValue) && !has(self.booleanValue) && !has(self.durationValue) && has(self.quantityValue))",message="exactly the value field matching type must be set"
type TypedValue struct {
	Type TypedValueType `json:"type"`
	// +kubebuilder:validation:MaxLength=65536
	StringValue  *string `json:"stringValue,omitempty"`
	IntegerValue *int64  `json:"integerValue,omitempty"`
	// DecimalValue is a base-10 JSON-independent decimal representation.
	// +kubebuilder:validation:Pattern=`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`
	// +kubebuilder:validation:MaxLength=128
	DecimalValue  *string          `json:"decimalValue,omitempty"`
	BooleanValue  *bool            `json:"booleanValue,omitempty"`
	DurationValue *metav1.Duration `json:"durationValue,omitempty"`
	// QuantityValue uses Kubernetes quantity syntax.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	QuantityValue *string `json:"quantityValue,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.default) || self.default.type == self.type",message="default type must match parameter type"
// +kubebuilder:validation:XValidation:rule="!has(self.minimum) || self.minimum.type == self.type",message="minimum type must match parameter type"
// +kubebuilder:validation:XValidation:rule="!has(self.maximum) || self.maximum.type == self.type",message="maximum type must match parameter type"
// +kubebuilder:validation:XValidation:rule="!has(self.allowedValues) || self.allowedValues.all(x, x.type == self.type)",message="allowed value types must match parameter type"
type TestParameterDefinition struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*([A-Z][a-z0-9]*)*$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string         `json:"displayName"`
	Type        TypedValueType `json:"type"`
	Required    bool           `json:"required,omitempty"`
	Default     *TypedValue    `json:"default,omitempty"`
	Minimum     *TypedValue    `json:"minimum,omitempty"`
	Maximum     *TypedValue    `json:"maximum,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	Pattern string `json:"pattern,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	AllowedValues []TypedValue `json:"allowedValues,omitempty"`
}

// +kubebuilder:validation:Enum=Boolean;Status;Count;Gauge;Duration;Ratio;Throughput;IOPS;Bandwidth;Latency
type MetricValueType string

const (
	MetricValueTypeBoolean    MetricValueType = "Boolean"
	MetricValueTypeStatus     MetricValueType = "Status"
	MetricValueTypeCount      MetricValueType = "Count"
	MetricValueTypeGauge      MetricValueType = "Gauge"
	MetricValueTypeDuration   MetricValueType = "Duration"
	MetricValueTypeRatio      MetricValueType = "Ratio"
	MetricValueTypeThroughput MetricValueType = "Throughput"
	MetricValueTypeIOPS       MetricValueType = "IOPS"
	MetricValueTypeBandwidth  MetricValueType = "Bandwidth"
	MetricValueTypeLatency    MetricValueType = "Latency"
)

type TestMetricDefinition struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*([A-Z][a-z0-9]*)*$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string          `json:"displayName"`
	Type        MetricValueType `json:"type"`
	// +kubebuilder:validation:MaxLength=32
	Unit     string `json:"unit,omitempty"`
	Required bool   `json:"required,omitempty"`
}

type TestParticipantRole struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=8
	MinCount int32 `json:"minCount,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxCount int32 `json:"maxCount"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.roles) || self.roles.all(x, !has(x.minCount) || x.minCount <= x.maxCount)",message="role minCount must not exceed maxCount"
// +kubebuilder:validation:XValidation:rule="!has(self.roles) || size(self.roles) == 0 || self.roles.map(x, has(x.minCount) ? x.minCount : 0).sum() <= self.maxParticipants",message="role minimum counts must fit maxParticipants"
// +kubebuilder:validation:XValidation:rule="!has(self.roles) || size(self.roles) == 0 || self.roles.map(x, x.maxCount).sum() >= self.minParticipants",message="role maximum counts must cover minParticipants"
type TestTopology struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MinParticipants int32 `json:"minParticipants"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxParticipants int32 `json:"maxParticipants"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	Roles []TestParticipantRole `json:"roles,omitempty"`
}

type TestArtifact struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Name string `json:"name"`
	// Content is materialized into the execution workload only after the
	// CertificationTest identity and trust decision match the admitted Run.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=97280
	Content string `json:"content"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// +kubebuilder:validation:Enum=Shell;Python
type ScriptLanguage string

const (
	ScriptLanguageShell  ScriptLanguage = "Shell"
	ScriptLanguagePython ScriptLanguage = "Python"
)

// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.limits) || size(self.resources.limits) <= 16",message="script resource limits must not exceed 16 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.requests) || size(self.resources.requests) <= 16",message="script resource requests must not exceed 16 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.claims) || size(self.resources.claims) <= 16",message="script resource claims must not exceed 16 entries"
type ScriptTestDriver struct {
	Language ScriptLanguage `json:"language"`
	// Artifact names an entry in CertificationTest.spec.artifacts.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Artifact string `json:"artifact"`
	// RunnerImage is the approved digest-pinned runtime for the payload.
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	RunnerImage string                      `json:"runnerImage"`
	Resources   corev1.ResourceRequirements `json:"resources,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.limits) || size(self.resources.limits) <= 16",message="container resource limits must not exceed 16 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.requests) || size(self.resources.requests) <= 16",message="container resource requests must not exceed 16 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.claims) || size(self.resources.claims) <= 16",message="container resource claims must not exceed 16 entries"
type ContainerTestDriver struct {
	// +kubebuilder:validation:Pattern=`^[^@\s]+@sha256:[0-9a-f]{64}$`
	Image string `json:"image"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	Command []string `json:"command,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MaxLength=256
	Args      []string                    `json:"args,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type WorkflowTemplateTestDriver struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
	// Artifact optionally names an entry in CertificationTest.spec.artifacts
	// passed to the WorkflowTemplate as the standard script parameters.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[A-Za-z0-9._-]+$`
	Artifact string `json:"artifact,omitempty"`
}

// +kubebuilder:validation:Enum=Script;Container;WorkflowTemplate
type TestDriverType string

const (
	TestDriverTypeScript           TestDriverType = "Script"
	TestDriverTypeContainer        TestDriverType = "Container"
	TestDriverTypeWorkflowTemplate TestDriverType = "WorkflowTemplate"
)

// +kubebuilder:validation:XValidation:rule="self.type != 'Script' || has(self.script)",message="Script drivers require script"
// +kubebuilder:validation:XValidation:rule="self.type == 'Script' || !has(self.script)",message="script is valid only for Script drivers"
// +kubebuilder:validation:XValidation:rule="self.type != 'Container' || has(self.container)",message="Container drivers require container"
// +kubebuilder:validation:XValidation:rule="self.type == 'Container' || !has(self.container)",message="container is valid only for Container drivers"
// +kubebuilder:validation:XValidation:rule="self.type != 'WorkflowTemplate' || has(self.workflowTemplate)",message="WorkflowTemplate drivers require workflowTemplate"
// +kubebuilder:validation:XValidation:rule="self.type == 'WorkflowTemplate' || !has(self.workflowTemplate)",message="workflowTemplate is valid only for WorkflowTemplate drivers"
type CertificationTestDriver struct {
	Type             TestDriverType              `json:"type"`
	Script           *ScriptTestDriver           `json:"script,omitempty"`
	Container        *ContainerTestDriver        `json:"container,omitempty"`
	WorkflowTemplate *WorkflowTemplateTestDriver `json:"workflowTemplate,omitempty"`
}

type TestSecurityRequirements struct {
	Privileged  bool `json:"privileged,omitempty"`
	AllowRoot   bool `json:"allowRoot,omitempty"`
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

// +kubebuilder:validation:XValidation:rule="!has(self.topology) || self.topology.minParticipants <= self.topology.maxParticipants",message="minParticipants must not exceed maxParticipants"
// +kubebuilder:validation:XValidation:rule="self.driver.type != 'Script' || (has(self.artifacts) && self.artifacts.exists(a, a.name == self.driver.script.artifact))",message="Script driver artifact must exist in spec.artifacts"
// +kubebuilder:validation:XValidation:rule="self.driver.type != 'WorkflowTemplate' || !has(self.driver.workflowTemplate.artifact) || (has(self.artifacts) && self.artifacts.exists(a, a.name == self.driver.workflowTemplate.artifact))",message="WorkflowTemplate driver artifact must exist in spec.artifacts"
// +kubebuilder:validation:XValidation:rule="!has(self.artifacts) || self.artifacts.map(a, a.content.size()).sum() <= 327680",message="total artifact content must not exceed 327680 characters"
type CertificationTestSpec struct {
	// Metadata name is the stable machine name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName"`
	// +kubebuilder:validation:MaxLength=2048
	Description string `json:"description,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Owner string `json:"owner"`
	// Version is an operator-facing immutable source version, not a mutable channel.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=set
	// +kubebuilder:validation:items:MaxLength=63
	Tags []string `json:"tags,omitempty"`
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Parameters []TestParameterDefinition `json:"parameters,omitempty"`
	// Artifacts are inline, digest-verified execution inputs owned by this Test.
	// They are not copied into CertificationRun resolved plans.
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Artifacts []TestArtifact `json:"artifacts,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Metrics  []TestMetricDefinition   `json:"metrics"`
	Topology TestTopology             `json:"topology"`
	Driver   CertificationTestDriver  `json:"driver"`
	Security TestSecurityRequirements `json:"security,omitempty"`
}

// +kubebuilder:validation:Enum=Approved;Diagnostic;Rejected;Revoked
type CertificationTestTrustState string

const (
	CertificationTestTrustApproved   CertificationTestTrustState = "Approved"
	CertificationTestTrustDiagnostic CertificationTestTrustState = "Diagnostic"
	CertificationTestTrustRejected   CertificationTestTrustState = "Rejected"
	CertificationTestTrustRevoked    CertificationTestTrustState = "Revoked"
)

type CertificationTestTrustStatus struct {
	State CertificationTestTrustState `json:"state"`
	// ContentDigest identifies the canonical approved test definition.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ContentDigest string `json:"contentDigest"`
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason,omitempty"`
}

type CertificationTestStatus struct {
	ObservedGeneration int64                         `json:"observedGeneration,omitempty"`
	Trust              *CertificationTestTrustStatus `json:"trust,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certtest
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver.type`
// +kubebuilder:printcolumn:name="Trust",type=string,JSONPath=`.status.trust.state`
type CertificationTest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertificationTestSpec   `json:"spec"`
	Status CertificationTestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CertificationTestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationTest `json:"items"`
}
