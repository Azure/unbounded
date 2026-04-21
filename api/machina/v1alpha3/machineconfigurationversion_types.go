// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&MachineConfigurationVersion{}, &MachineConfigurationVersionList{})
}

// Label keys set on MachineConfigurationVersion objects.
const (
	// MCVConfigurationLabelKey identifies the parent MachineConfiguration.
	MCVConfigurationLabelKey = "unbounded-kube.io/machine-configuration"

	// MCVVersionLabelKey stores the version number as a label for
	// efficient selection.
	MCVVersionLabelKey = "unbounded-kube.io/machine-configuration-version"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mcv
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Configuration",type="string",JSONPath=".metadata.labels.unbounded-kube\\.io/machine-configuration"
// +kubebuilder:printcolumn:name="Version",type="integer",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="Deployed",type="boolean",JSONPath=".status.deployed"
// +kubebuilder:printcolumn:name="Machines",type="integer",JSONPath=".status.deployedMachines"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineConfigurationVersion is an immutable-once-deployed snapshot of a
// MachineConfiguration's spec at a specific point in time. It acts like a
// ReplicaSet to a Deployment: the MachineConfiguration controller
// automatically creates and manages these objects. Versions remain
// editable until they are deployed (referenced by a Machine). Once
// deployed, the spec fields become immutable.
type MachineConfigurationVersion struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineConfigurationVersionSpec   `json:"spec,omitempty"`
	Status MachineConfigurationVersionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineConfigurationVersionList contains a list of
// MachineConfigurationVersion.
type MachineConfigurationVersionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineConfigurationVersion `json:"items"`
}

// MachineConfigurationVersionSpec defines the versioned configuration
// snapshot.
type MachineConfigurationVersionSpec struct {
	// Version is the monotonically increasing version number within
	// the parent MachineConfiguration.
	// +kubebuilder:validation:Minimum=1
	Version int32 `json:"version"`

	// Template is the configuration snapshot copied from the parent
	// MachineConfiguration at the time this version was created or
	// last updated (while still editable).
	Template MachineConfigurationTemplate `json:"template"`
}

// MachineConfigurationVersionStatus defines the observed state of a
// MachineConfigurationVersion.
type MachineConfigurationVersionStatus struct {
	// Deployed indicates whether any Machine has been provisioned
	// using this version. Once true, the spec fields are immutable.
	Deployed bool `json:"deployed,omitempty"`

	// DeployedMachines is the count of Machines currently referencing
	// this version.
	DeployedMachines int32 `json:"deployedMachines,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
