// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:validation:Enum=OnNodeReady
type CampaignTrigger string

const (
	CampaignTriggerOnNodeReady CampaignTrigger = "OnNodeReady"
)

// +kubebuilder:validation:XValidation:rule="(has(self.nodeSelector.matchLabels) && size(self.nodeSelector.matchLabels) > 0) || (has(self.nodeSelector.matchExpressions) && size(self.nodeSelector.matchExpressions) > 0)",message="nodeSelector must contain at least one matchLabels or matchExpressions term"
// +kubebuilder:validation:XValidation:rule="duration(self.duration) > duration('0s')",message="duration must be positive"
// +kubebuilder:validation:XValidation:rule="duration(self.duration) <= duration('168h')",message="duration must not exceed 168 hours"
type CertificationCampaignPolicySpec struct {
	Profile      PolicyProfileReference `json:"profile"`
	NodeSelector metav1.LabelSelector   `json:"nodeSelector"`
	Trigger      CampaignTrigger        `json:"trigger"`
	Duration     metav1.Duration        `json:"duration"`
	// StopOnFailure ends the campaign after a failed Run finishes and releases.
	// The default is false, so campaigns continue admitting Runs until stopped
	// or their deadline is reached.
	StopOnFailure bool `json:"stopOnFailure,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=certcampaignpolicy
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile.name`
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.spec.duration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CertificationCampaignPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CertificationCampaignPolicySpec `json:"spec"`
}

// +kubebuilder:object:root=true
type CertificationCampaignPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationCampaignPolicy `json:"items"`
}

type PinnedCampaignPolicy struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

// +kubebuilder:validation:Enum=Pending;Running;Completed;Blocked
type CampaignPhase string

const (
	CampaignPhasePending   CampaignPhase = "Pending"
	CampaignPhaseRunning   CampaignPhase = "Running"
	CampaignPhaseCompleted CampaignPhase = "Completed"
	CampaignPhaseBlocked   CampaignPhase = "Blocked"
)

type CampaignRunReference struct {
	Name     string    `json:"name"`
	UID      types.UID `json:"uid"`
	Sequence int32     `json:"sequence"`
}

// +kubebuilder:validation:XValidation:rule="self.policy == oldSelf.policy",message="policy is immutable"
// +kubebuilder:validation:XValidation:rule="self.profile == oldSelf.profile",message="profile is immutable"
// +kubebuilder:validation:XValidation:rule="self.suite == oldSelf.suite",message="suite is immutable"
// +kubebuilder:validation:XValidation:rule="self.nodeName == oldSelf.nodeName",message="nodeName is immutable"
// +kubebuilder:validation:XValidation:rule="self.nodeUID == oldSelf.nodeUID",message="nodeUID is immutable"
// +kubebuilder:validation:XValidation:rule="self.startedAt == oldSelf.startedAt",message="startedAt is immutable"
// +kubebuilder:validation:XValidation:rule="self.deadline == oldSelf.deadline",message="deadline is immutable"
// +kubebuilder:validation:XValidation:rule="(has(self.stopOnFailure) && self.stopOnFailure) == (has(oldSelf.stopOnFailure) && oldSelf.stopOnFailure)",message="stopOnFailure is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.stop) || !oldSelf.stop || (has(self.stop) && self.stop)",message="stop cannot be cleared"
type CertificationCampaignSpec struct {
	Policy    PinnedCampaignPolicy `json:"policy"`
	Profile   PinnedProfile        `json:"profile"`
	Suite     PinnedSuite          `json:"suite"`
	NodeName  string               `json:"nodeName"`
	NodeUID   types.UID            `json:"nodeUID"`
	StartedAt metav1.Time          `json:"startedAt"`
	Deadline  metav1.Time          `json:"deadline"`
	// StopOnFailure ends the campaign after a failed Run finishes and releases.
	StopOnFailure bool `json:"stopOnFailure,omitempty"`
	// Stop prevents new Runs from being admitted. An active Run is allowed to
	// finish and release normally.
	Stop bool `json:"stop,omitempty"`
}

type CertificationCampaignStatus struct {
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	Phase              CampaignPhase         `json:"phase,omitempty"`
	CurrentRun         *CampaignRunReference `json:"currentRun,omitempty"`
	LastRun            *CampaignRunReference `json:"lastRun,omitempty"`
	StartedRuns        int32                 `json:"startedRuns,omitempty"`
	SucceededRuns      int32                 `json:"succeededRuns,omitempty"`
	FailedRuns         int32                 `json:"failedRuns,omitempty"`
	InconclusiveRuns   int32                 `json:"inconclusiveRuns,omitempty"`
	CanceledRuns       int32                 `json:"canceledRuns,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=certcampaign
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runs",type=integer,JSONPath=`.status.startedRuns`
// +kubebuilder:printcolumn:name="Deadline",type=date,JSONPath=`.spec.deadline`
type CertificationCampaign struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CertificationCampaignSpec   `json:"spec"`
	Status CertificationCampaignStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type CertificationCampaignList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CertificationCampaign `json:"items"`
}
