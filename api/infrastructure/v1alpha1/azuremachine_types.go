// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &AzureMachine{}, &AzureMachineList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

const (
	// AzureMachineKind is the Kubernetes kind used by the Azure VM provider.
	AzureMachineKind = "AzureMachine"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=azm
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Resource ID",type="string",JSONPath=".spec.resourceID"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AzureMachine contains the Azure-specific state for one Unbounded Machine.
type AzureMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AzureMachineSpec   `json:"spec"`
	Status AzureMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AzureMachineList contains a list of AzureMachine resources.
type AzureMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AzureMachine `json:"items"`
}

// AzureMachineSpec defines the Azure resource owned by an Unbounded Machine.
// +kubebuilder:validation:XValidation:rule="self.resourceID == oldSelf.resourceID",message="resourceID is immutable"
type AzureMachineSpec struct {
	// ResourceID is the full Azure Resource Manager ID of the virtual machine.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ResourceID string `json:"resourceID"`
}

// AzureMachineStatus describes the Azure provider's latest observations.
type AzureMachineStatus struct {
	// ObservedGeneration is the AzureMachine generation most recently observed
	// by the provider controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest Azure provider observations.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
