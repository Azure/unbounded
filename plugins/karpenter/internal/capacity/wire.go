// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package capacity contains the provider-neutral wire projection of the shared capacity API.
package capacity

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	ConsumerUIDLabel  = "unbounded-cloud.io/consumer-uid"
	AllocationLabel   = "unbounded-cloud.io/allocation-uid"
	CapacityTypeLabel = "unbounded-cloud.io/capacity-type"
	ProviderIDPrefix  = "unbounded:///machineallocations/"
	Active            = "Active"
	Released          = "Released"

	// ConsumerGroup and ConsumerKind identify the allocations this plugin owns.
	ConsumerGroup = "karpenter.sh"
	ConsumerKind  = "NodeClaim"

	// Condition types and reasons the plugin reads from allocation status.
	ConditionAllocated  = "Allocated"
	ConditionFenced     = "Fenced"
	ConditionReleased   = "Released"
	ConditionRegistered = "Registered"

	FailureInsufficientCapacity = "InsufficientCapacity"
	FailureQuotaExceeded        = "QuotaExceeded"
)

// AllocationName is the name of the allocation for the NodeClaim with uid.
// It derives from the UID alone, so every retry addresses the same object.
func AllocationName(uid types.UID) string {
	return "nodeclaim-" + string(uid)
}

var AllocationGVK = schema.GroupVersionKind{
	Group:   "unbounded-cloud.io",
	Version: "v1alpha3",
	Kind:    "MachineAllocation",
}

type Offering struct {
	Name         string              `json:"name"`
	InstanceType string              `json:"instanceType"`
	Zone         string              `json:"zone,omitempty"`
	CapacityType string              `json:"capacityType"`
	Labels       map[string]string   `json:"labels,omitempty"`
	Capacity     corev1.ResourceList `json:"capacity,omitempty"`
	Overhead     Overhead            `json:"overhead,omitzero"`
	Availability string              `json:"availability"`
	Price        *Price              `json:"price,omitempty"`
	ObservedAt   *metav1.Time        `json:"observedAt,omitempty"`
	ValidUntil   *metav1.Time        `json:"validUntil,omitempty"`
}

type Overhead struct {
	KubeReserved      corev1.ResourceList `json:"kubeReserved,omitempty"`
	SystemReserved    corev1.ResourceList `json:"systemReserved,omitempty"`
	EvictionThreshold corev1.ResourceList `json:"evictionThreshold,omitempty"`
}

type Price struct {
	Currency   string       `json:"currency"`
	PerHour    string       `json:"perHour"`
	Source     string       `json:"source,omitempty"`
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

type Reference struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}

type Consumer struct {
	APIGroup string    `json:"apiGroup"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	UID      types.UID `json:"uid"`
}

type Requirement struct {
	Key       string                      `json:"key"`
	Operator  corev1.NodeSelectorOperator `json:"operator"`
	Values    []string                    `json:"values,omitempty"`
	MinValues *int32                      `json:"minValues,omitempty"`
}

type NodeTemplate struct {
	Labels        map[string]string `json:"labels,omitempty"`
	Taints        []corev1.Taint    `json:"taints,omitempty"`
	StartupTaints []corev1.Taint    `json:"startupTaints,omitempty"`
}

type AllocationSpec struct {
	ConsumerRef  Consumer            `json:"consumerRef"`
	ClassRef     Reference           `json:"classRef"`
	Resources    corev1.ResourceList `json:"resources,omitempty"`
	Requirements []Requirement       `json:"requirements,omitempty"`
	NodeTemplate NodeTemplate        `json:"nodeTemplate,omitzero"`
	Lifecycle    string              `json:"lifecycle,omitempty"`
}

// Snapshot is the part of the frozen allocation input the plugin reads.
type Snapshot struct {
	ClassUID      types.UID `json:"classUID"`
	ClassRevision string    `json:"classRevision"`
	SiteName      string    `json:"siteName"`
	Offering      Offering  `json:"offering"`
}

type AllocationStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	Snapshot           *Snapshot           `json:"snapshot,omitempty"`
	NodeRef            *Reference          `json:"nodeRef,omitempty"`
	ProviderID         string              `json:"providerID,omitempty"`
	ProviderResourceID string              `json:"providerResourceID,omitempty"`
	InstanceType       string              `json:"instanceType,omitempty"`
	Zone               string              `json:"zone,omitempty"`
	CapacityType       string              `json:"capacityType,omitempty"`
	Image              string              `json:"image,omitempty"`
	Capacity           corev1.ResourceList `json:"capacity,omitempty"`
	Allocatable        corev1.ResourceList `json:"allocatable,omitempty"`
	Labels             map[string]string   `json:"labels,omitempty"`
	FailureReason      string              `json:"failureReason,omitempty"`
	FailureMessage     string              `json:"failureMessage,omitempty"`
	Conditions         []metav1.Condition  `json:"conditions,omitempty"`
}

type Allocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   AllocationSpec   `json:"spec,omitzero"`
	Status AllocationStatus `json:"status,omitzero"`
}
