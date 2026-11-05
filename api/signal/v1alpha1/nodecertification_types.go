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

// +kubebuilder:validation:XValidation:rule="self.policy == oldSelf.policy",message="policy is immutable"
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

type NodeCertificationStatus struct {
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	Phase              NodeCertificationPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	PolicyDigest string `json:"policyDigest,omitempty"`
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^sha256:[0-9a-f]{64}$`
	PlanDigests []string         `json:"planDigests,omitempty"`
	Fingerprint *NodeFingerprint `json:"fingerprint,omitempty"`
	QualifiedAt *metav1.Time     `json:"qualifiedAt,omitempty"`
	ExpiresAt   *metav1.Time     `json:"expiresAt,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	ContributingRuns []ContributingRunReference `json:"contributingRuns,omitempty"`
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NodeCertification is the qualification ledger for one Policy UID and Node UID.
// Qualification is fail-closed: deleting this object removes its gate projection.
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
