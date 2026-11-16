// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nodeclass projects the common MachineClass into Karpenter's readiness interface.
package nodeclass

import (
	"maps"
	"slices"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

var GroupVersion = schema.GroupVersion{
	Group:   "unbounded-cloud.io",
	Version: "v1alpha3",
}

// MachineClass is a read-only wire projection, not an additional custom resource.
// The capacity API owns its schema and all status writes.
type MachineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   MachineClassSpec   `json:"spec,omitzero"`
	Status MachineClassStatus `json:"status,omitzero"`
}

// MachineClassSpec is the part of the class spec the plugin reads.
type MachineClassSpec struct {
	SiteRef SiteReference `json:"siteRef,omitzero"`
}

// SiteReference names the Site machines of the class join.
type SiteReference struct {
	Name string `json:"name,omitempty"`
}

type MachineClassStatus struct {
	ObservedGeneration int64               `json:"observedGeneration,omitempty"`
	ResolvedRevision   string              `json:"resolvedRevision,omitempty"`
	Offerings          []capacity.Offering `json:"offerings,omitempty"`
	Conditions         []status.Condition  `json:"conditions,omitempty"`
}

type MachineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []MachineClass `json:"items"`
}

func AddToScheme(scheme *runtime.Scheme) {
	scheme.AddKnownTypes(GroupVersion, &MachineClass{}, &MachineClassList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
}

func (in *MachineClass) GetConditions() []status.Condition {
	conditions := slices.Clone(in.Status.Conditions)
	for index := range conditions {
		condition := &conditions[index]
		if condition.Type != status.ConditionReady {
			continue
		}

		if condition.ObservedGeneration != in.Generation ||
			in.Status.ObservedGeneration != in.Generation {
			condition.Status = metav1.ConditionUnknown
			condition.Reason = "UnobservedGeneration"
			condition.Message = "MachineClass inputs have not been observed for the current generation"
		}
	}

	return conditions
}

func (in *MachineClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

func (in *MachineClass) StatusConditions(options ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions().For(in, options...)
}

func (in *MachineClass) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}

	return in.DeepCopy()
}

func (in *MachineClass) DeepCopy() *MachineClass {
	if in == nil {
		return nil
	}

	out := new(MachineClass)
	*out = *in
	in.DeepCopyInto(&out.ObjectMeta)
	out.Status.Conditions = append([]status.Condition(nil), in.Status.Conditions...)
	out.Status.Offerings = make([]capacity.Offering, len(in.Status.Offerings))

	for index, offering := range in.Status.Offerings {
		out.Status.Offerings[index] = copyOffering(offering)
	}

	return out
}

func copyOffering(in capacity.Offering) capacity.Offering {
	out := in
	out.Labels = make(map[string]string, len(in.Labels))

	maps.Copy(out.Labels, in.Labels)

	out.Capacity = in.Capacity.DeepCopy()
	out.Overhead.KubeReserved = in.Overhead.KubeReserved.DeepCopy()
	out.Overhead.SystemReserved = in.Overhead.SystemReserved.DeepCopy()
	out.Overhead.EvictionThreshold = in.Overhead.EvictionThreshold.DeepCopy()
	out.ObservedAt = in.ObservedAt.DeepCopy()
	out.ValidUntil = in.ValidUntil.DeepCopy()

	if in.Price != nil {
		price := *in.Price
		price.ObservedAt = in.Price.ObservedAt.DeepCopy()
		out.Price = &price
	}

	return out
}

func (in *MachineClassList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}

	out := new(MachineClassList)
	*out = *in
	in.DeepCopyInto(&out.ListMeta)

	if in.Items != nil {
		out.Items = make([]MachineClass, len(in.Items))
		for index := range in.Items {
			out.Items[index] = *in.Items[index].DeepCopy()
		}
	}

	return out
}
