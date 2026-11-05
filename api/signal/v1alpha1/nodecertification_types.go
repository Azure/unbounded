// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type PinnedPolicy struct {
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

const (
	NodeCertificationConditionGateProjected     = "GateProjected"
	NodeCertificationReasonProtectionUnverified = "NodeRestrictionUnverified"
)

// +kubebuilder:validation:XValidation:rule="self.policy.name == oldSelf.policy.name",message="policy name is immutable"
// +kubebuilder:validation:XValidation:rule="self.policy.uid == oldSelf.policy.uid",message="policy UID is immutable"
// +kubebuilder:validation:XValidation:rule="self.policy.generation >= oldSelf.policy.generation",message="policy generation cannot decrease"
// +kubebuilder:validation:XValidation:rule="self.nodeName == oldSelf.nodeName",message="nodeName is immutable"
// +kubebuilder:validation:XValidation:rule="self.nodeUID == oldSelf.nodeUID",message="nodeUID is immutable"
// +kubebuilder:validation:XValidation:rule="size(self.nodeUID) > 0",message="nodeUID is required"
type NodeCertificationSpec struct {
	Policy PinnedPolicy `json:"policy"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeName string    `json:"nodeName"`
	NodeUID  types.UID `json:"nodeUID"`
}

// +kubebuilder:validation:Enum=Pending;Qualifying;Qualified;Expired;Revoked;NodeGone
type NodeCertificationPhase string

const (
	NodeCertificationPhasePending    NodeCertificationPhase = "Pending"
	NodeCertificationPhaseQualifying NodeCertificationPhase = "Qualifying"
	NodeCertificationPhaseQualified  NodeCertificationPhase = "Qualified"
	NodeCertificationPhaseExpired    NodeCertificationPhase = "Expired"
	NodeCertificationPhaseRevoked    NodeCertificationPhase = "Revoked"
	NodeCertificationPhaseNodeGone   NodeCertificationPhase = "NodeGone"
)

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type ContributingRunReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigest string `json:"planDigest"`
	// +kubebuilder:validation:MaxLength=2048
	EvidenceURI string `json:"evidenceURI"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	EvidenceDigest string `json:"evidenceDigest"`
}

// +kubebuilder:validation:Enum=Failed;Inconclusive;Canceled
type NonPassClassification string

const (
	NonPassClassificationFailed       NonPassClassification = "Failed"
	NonPassClassificationInconclusive NonPassClassification = "Inconclusive"
	NonPassClassificationCanceled     NonPassClassification = "Canceled"
)

// +kubebuilder:validation:XValidation:rule="size(self.uid) > 0",message="uid is required"
type LatestNonPassWatermark struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	// FinishedAt is the terminal Run completion time used to order watermarks.
	FinishedAt     metav1.Time           `json:"finishedAt"`
	Classification NonPassClassification `json:"classification"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	EvidenceDigest string `json:"evidenceDigest"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.latestNonPass) || (has(self.latestNonPass) && (self.latestNonPass.finishedAt > oldSelf.latestNonPass.finishedAt || (self.latestNonPass.finishedAt == oldSelf.latestNonPass.finishedAt && (self.latestNonPass.uid > oldSelf.latestNonPass.uid || (self.latestNonPass.uid == oldSelf.latestNonPass.uid && self.latestNonPass == oldSelf.latestNonPass)))))",message="latestNonPass must advance by finishedAt and UID and is immutable for the same Run"
type NodeCertificationStatus struct {
	// ObservedGeneration is the Policy generation reflected by this status.
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	Phase              NodeCertificationPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PolicyDigest string `json:"policyDigest,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigests []string         `json:"planDigests,omitempty"`
	Fingerprint *NodeFingerprint `json:"fingerprint,omitempty"`
	QualifiedAt *metav1.Time     `json:"qualifiedAt,omitempty"`
	ExpiresAt   *metav1.Time     `json:"expiresAt,omitempty"`
	// LatestNonPass is the durable latest terminal non-pass that affects
	// Reset or Revoke semantics. It survives Run and evidence deletion so
	// passes completed before it cannot regain credit.
	LatestNonPass *LatestNonPassWatermark `json:"latestNonPass,omitempty"`
	// ContributingRuns contains credited passing Runs only.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	ContributingRuns []ContributingRunReference `json:"contributingRuns,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodeCertification is the qualification ledger for one Policy UID and Node UID.
// No ledger individually owns the shared qualification label. The controller
// combines every ProtectedLabel Policy fail-closed and removes the label unless
// all have a Qualified, unexpired ledger for the exact Node UID. While
// NodeRestriction protection is disabled or unverified, the controller removes
// the label from every Node and sets GateProjected=False with reason
// NodeRestrictionUnverified.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=nodecert
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policy.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`
type NodeCertification struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeCertificationSpec   `json:"spec"`
	Status NodeCertificationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NodeCertificationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeCertification `json:"items"`
}
