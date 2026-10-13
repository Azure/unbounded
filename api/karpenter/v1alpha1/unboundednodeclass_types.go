// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	KindUnboundedNodeClass = "UnboundedNodeClass"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &UnboundedNodeClass{}, &UnboundedNodeClassList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=unboundednodeclasses,scope=Cluster,categories=karpenter,shortName={unc,uncs}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// UnboundedNodeClass selects existing Machine inventory and describes the
// instance capacity Karpenter may allocate from it.
type UnboundedNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UnboundedNodeClassSpec   `json:"spec"`
	Status UnboundedNodeClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type UnboundedNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UnboundedNodeClass `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="!self.imagePolicy.requireNVIDIA || size(self.imagePolicy.requiredImage) > 0",message="requireNVIDIA requires an exact requiredImage trust anchor"
type UnboundedNodeClassSpec struct {
	// MachineSelector selects the physical Machine inventory this class owns.
	// +kubebuilder:validation:Required
	MachineSelector metav1.LabelSelector `json:"machineSelector"`

	// Site restricts Machines to unbounded-cloud.io/site.
	// +optional
	Site string `json:"site,omitempty"`

	// Domain restricts Machines to unbounded-cloud.io/capacity-domain.
	// +optional
	Domain string `json:"domain,omitempty"`

	// ConfigurationRef selects the MachineConfiguration applied before
	// provisioning a reserved Machine.
	// +kubebuilder:validation:Required
	ConfigurationRef machinav1alpha3.MachineConfigurationRef `json:"configurationRef"`

	// ImagePolicy defines the exact configured and observed worker image.
	// +kubebuilder:validation:Required
	ImagePolicy ImagePolicy `json:"imagePolicy"`

	// InstanceTypes lists every capacity shape exposed to Karpenter.
	// +kubebuilder:validation:MinItems=1
	InstanceTypes []InstanceType `json:"instanceTypes"`

	// Labels are applied to hydrated NodeClaims and joining Nodes.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Requirements are additional fixed provider requirements.
	// +optional
	Requirements []corev1.NodeSelectorRequirement `json:"requirements,omitempty"`
}

type ImagePolicy struct {
	// RequiredImage is an exact OCI image reference.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	RequiredImage string `json:"requiredImage"`

	// RequireNVIDIA requires NVIDIA worker support. Dedicated GB200 capacity
	// always requires this field.
	// +optional
	RequireNVIDIA bool `json:"requireNVIDIA,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.gpu) || self.gpu.model != 'GB200' || (self.gpu.vendor == 'NVIDIA' && self.gpu.count == 4 && self.gpu.requireFabric)",message="GB200 instance capacity requires NVIDIA, exactly 4 GPUs, and fabric health"
type InstanceType struct {
	// Name is the value of node.kubernetes.io/instance-type.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Capacity is the full resource capacity. CPU, memory, and pods are
	// required. GPU profiles must also declare nvidia.com/gpu.
	// +kubebuilder:validation:Required
	Capacity corev1.ResourceList `json:"capacity"`

	// Architecture is a Kubernetes architecture label value.
	// +kubebuilder:default=amd64
	Architecture string `json:"architecture,omitempty"`

	// OperatingSystem is a Kubernetes OS label value.
	// +kubebuilder:default=linux
	OperatingSystem string `json:"operatingSystem,omitempty"`

	// Zone is the topology zone exposed to Karpenter.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Zone string `json:"zone"`

	// CapacityType is the Karpenter capacity type.
	// Existing physical Machine inventory normally uses reserved.
	// +kubebuilder:default=reserved
	CapacityType string `json:"capacityType,omitempty"`

	// GPU defines exact accelerator and health requirements.
	// +optional
	GPU *GPUPolicy `json:"gpu,omitempty"`
}

type GPUPolicy struct {
	// +kubebuilder:validation:Enum=NVIDIA
	Vendor string `json:"vendor"`
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`
	// +kubebuilder:validation:Minimum=1
	Count         int32 `json:"count"`
	RequireFabric bool  `json:"requireFabric,omitempty"`
}

type UnboundedNodeClassStatus struct {
	// Conditions contains the standard operator Ready condition.
	// +listType=map
	// +listMapKey=type
	Conditions []status.Condition `json:"conditions,omitempty"`
}

func (in *UnboundedNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions().For(in, opts...)
}

func (in *UnboundedNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *UnboundedNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
