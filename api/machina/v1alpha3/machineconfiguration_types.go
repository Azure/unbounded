// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&MachineConfiguration{}, &MachineConfigurationList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Latest Version",type="integer",JSONPath=".status.latestVersion"
// +kubebuilder:printcolumn:name="Update Strategy",type="string",JSONPath=".spec.updateStrategy.type"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineConfiguration stores a configuration profile for a class of
// machines. It acts like a Deployment: edits to the spec automatically
// create or update a child MachineConfigurationVersion, similar to how
// Deployment edits create or update ReplicaSets.
type MachineConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineConfigurationSpec   `json:"spec,omitempty"`
	Status MachineConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineConfigurationList contains a list of MachineConfiguration.
type MachineConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineConfiguration `json:"items"`
}

// MachineConfigurationSpec defines the desired configuration profile for
// a class of machines.
type MachineConfigurationSpec struct {
	// Template contains the configuration fields that are versioned
	// into MachineConfigurationVersion objects.
	// +kubebuilder:validation:Required
	Template MachineConfigurationTemplate `json:"template"`

	// MachineSelector selects machines that should automatically
	// receive this configuration. If set, new machines matching the
	// selector will be assigned the latest locked version of the
	// highest priority configuration that matches.
	// +optional
	MachineSelector *metav1.LabelSelector `json:"machineSelector,omitempty"`

	// Priority determines which configuration is selected when multiple
	// configurations match a machine's labels. Higher values take
	// precedence. When two configurations have the same priority,
	// lexicographic ordering of names is used as a tiebreaker.
	// +optional
	// +kubebuilder:default=0
	Priority int32 `json:"priority,omitempty"`

	// UpdateStrategy controls how configuration changes are rolled out
	// to machines that reference this configuration.
	// +optional
	UpdateStrategy MachineConfigurationUpdateStrategy `json:"updateStrategy,omitempty"`

	// RevisionHistoryLimit is the number of old
	// MachineConfigurationVersions to retain for rollback. Versions
	// that are still referenced by a Machine are never deleted
	// regardless of this limit.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
}

// MachineConfigurationTemplate holds the versioned configuration
// fields. These fields are copied into each MachineConfigurationVersion.
type MachineConfigurationTemplate struct {
	// Kubernetes contains Kubernetes-specific configuration such as
	// the target version, node labels, and taints.
	// +optional
	Kubernetes *MachineConfigurationKubernetes `json:"kubernetes,omitempty"`

	// Agent contains settings for the unbounded node agent (e.g. the
	// OCI image reference for the nspawn machine).
	// +optional
	Agent *MachineConfigurationAgent `json:"agent,omitempty"`
}

// MachineConfigurationKubernetes holds the Kubernetes-specific fields
// that are part of the versioned configuration.
type MachineConfigurationKubernetes struct {
	// Version is the Kubernetes version to install (e.g. "v1.34.0").
	// +optional
	Version string `json:"version,omitempty"`

	// NodeLabels are labels passed to kubelet's --node-labels flag.
	// +optional
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`

	// RegisterWithTaints are taints passed to kubelet's
	// --register-with-taints flag. Each entry uses the standard
	// Kubernetes taint format: key=value:Effect.
	// +optional
	RegisterWithTaints []string `json:"registerWithTaints,omitempty"`
}

// MachineConfigurationAgent holds agent-specific fields that are part
// of the versioned configuration.
type MachineConfigurationAgent struct {
	// Image is the OCI image reference used for provisioning the
	// nspawn machine (e.g. "ghcr.io/org/repo:tag").
	// +kubebuilder:validation:Required
	Image string `json:"image"`
}

// MachineConfigurationUpdateStrategyType defines how configuration
// changes are applied to machines.
// +kubebuilder:validation:Enum=OnDelete;RollingUpdate
type MachineConfigurationUpdateStrategyType string

const (
	// OnDeleteUpdateStrategy requires the operator to manually
	// cordon, drain, and delete the Node object to trigger a repave
	// with the new configuration.
	OnDeleteUpdateStrategy MachineConfigurationUpdateStrategyType = "OnDelete"

	// RollingUpdateStrategy automates the cordon/drain/delete cycle
	// across machines, respecting maxUnavailable. Reserved for future
	// implementation.
	RollingUpdateStrategy MachineConfigurationUpdateStrategyType = "RollingUpdate"
)

// MachineConfigurationUpdateStrategy defines the strategy for applying
// configuration updates.
type MachineConfigurationUpdateStrategy struct {
	// Type is the update strategy type.
	// +kubebuilder:default=OnDelete
	Type MachineConfigurationUpdateStrategyType `json:"type,omitempty"`

	// MaxUnavailable is the maximum number of machines that can be
	// unavailable during a RollingUpdate. Only used when Type is
	// RollingUpdate. Can be an absolute number or a percentage.
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`
}

// MachineConfigurationStatus defines the observed state of a
// MachineConfiguration.
type MachineConfigurationStatus struct {
	// LatestVersion is the highest version number that has been
	// created for this MachineConfiguration. The controller uses this
	// to avoid reusing version numbers when versions are deleted.
	LatestVersion int32 `json:"latestVersion,omitempty"`

	// CurrentVersion is the version number of the latest non-deployed
	// (editable) MachineConfigurationVersion. If all versions are
	// deployed, this is 0.
	CurrentVersion int32 `json:"currentVersion,omitempty"`

	// Conditions represent the latest available observations of the
	// MachineConfiguration's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
