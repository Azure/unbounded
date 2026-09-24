// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	ConditionAccepted = "Accepted"
	ConditionReady    = "Ready"
)

// ClusterCache names a node-local HTTP cache backed by a node-local origin.
// Each selected nonterminating Site has an independent cache universe.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS label of at most 63 characters"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=".status.participants.desired"
// +kubebuilder:printcolumn:name="Participants",type=integer,JSONPath=".status.participants.ready"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type ClusterCache struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// +kubebuilder:default={cacheGeneration:1,maxCandidateAttempts:3}
	Spec   ClusterCacheSpec   `json:"spec,omitempty"`
	Status ClusterCacheStatus `json:"status,omitempty"`
}

type ClusterCacheSpec struct {
	// SiteSelector matches Site labels. An omitted or empty selector matches all
	// nonterminating Sites.
	// +optional
	SiteSelector metav1.LabelSelector `json:"siteSelector,omitempty"`
	// CacheGeneration invalidates cached data without changing the origin socket.
	// It cannot decrease. Resource recreation also changes cache identity.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="cacheGeneration cannot decrease"
	CacheGeneration int64 `json:"cacheGeneration"`
	// MaxCandidateAttempts bounds peer candidates before local origin fallback.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxCandidateAttempts int32 `json:"maxCandidateAttempts"`
}

type ClusterCacheStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ClientSocket is the configured node-local client path, not a readiness signal.
	// +optional
	ClientSocket string `json:"clientSocket,omitempty"`
	// OriginSocket is the configured node-local origin path, not a readiness signal.
	// +optional
	OriginSocket string `json:"originSocket,omitempty"`
	// Conditions describe acceptance and activation, not origin availability.
	// +listType=map
	// +listMapKey=type
	Conditions   []metav1.Condition       `json:"conditions,omitempty"`
	Participants ClusterCacheParticipants `json:"participants,omitempty"`
}

type ClusterCacheParticipants struct {
	// Desired includes eligible Nodes even while their dataplane is starting.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Desired int32 `json:"desired"`
	// Ready counts fresh activation acknowledgments with healthy workers.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	Ready int32 `json:"ready"`
}

// +kubebuilder:object:root=true
type ClusterCacheList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterCache `json:"items"`
}
