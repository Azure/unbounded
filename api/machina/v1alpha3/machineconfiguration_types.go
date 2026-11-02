// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &MachineConfiguration{}, &MachineConfigurationList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
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

	// MachineSelector picks Machines that get this configuration
	// automatically. A matching Machine without a configurationRef gets the
	// newest version of the highest-priority matching configuration. The
	// choice is not revisited once configurationRef is set.
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

	// TODO(#1038): describe cleanup once new versions are made, and keep
	// versions a Machine still uses.

	// RevisionHistoryLimit is how many old MachineConfigurationVersions to
	// keep. Not used yet.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`
}

// MachineConfigurationTemplate holds the versioned configuration
// fields. These fields are copied into each MachineConfigurationVersion.
type MachineConfigurationTemplate struct {
	// Host contains provider-neutral desired host settings.
	// +optional
	Host *MachineConfigurationHostSpec `json:"host,omitempty"`

	// Kubernetes contains Kubernetes-specific configuration such as
	// the target version, node labels, and taints.
	// +optional
	Kubernetes *MachineConfigurationKubernetes `json:"kubernetes,omitempty"`

	// Agent contains settings for the unbounded node agent (e.g. the
	// OCI image reference for the nspawn machine).
	// +optional
	Agent *MachineConfigurationAgent `json:"agent,omitempty"`
}

// MachineConfigurationHostSpec contains versioned, provider-neutral host
// settings. Per-machine host ownership belongs on Machine.spec.host.
type MachineConfigurationHostSpec struct {
	// ProvisioningFormat declares the first-boot payload understood by Image.
	// +optional
	ProvisioningFormat ProvisioningFormat `json:"provisioningFormat,omitempty"`
	// Image is an opaque provider-interpreted image identifier.
	// +optional
	Image string `json:"image,omitempty"`
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
	// --register-with-taints flag.
	// +optional
	RegisterWithTaints []corev1.Taint `json:"registerWithTaints,omitempty"`
}

// MachineConfigurationAgent holds agent-specific fields that are part
// of the versioned configuration.
type MachineConfigurationAgent struct {
	// Image is the OCI image reference used for provisioning the
	// nspawn machine (e.g. "ghcr.io/org/repo:tag").
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// LocalDNS configures the optional CoreDNS cache for this version.
	// +optional
	LocalDNS *LocalDNSSpec `json:"localDNS,omitempty"`
}

// MachineConfigurationUpdateStrategyType defines how configuration
// changes are applied to machines.
// +kubebuilder:validation:Enum=OnDelete;RollingUpdate
type MachineConfigurationUpdateStrategyType string

const (
	// OnDeleteUpdateStrategy applies configurationRef when the Node object is deleted.
	OnDeleteUpdateStrategy MachineConfigurationUpdateStrategyType = "OnDelete"

	// RollingUpdateStrategy is meant to repave nodes a few at a time. Not used yet.
	RollingUpdateStrategy MachineConfigurationUpdateStrategyType = "RollingUpdate"
)

// MachineConfigurationUpdateStrategy defines the strategy for applying
// configuration updates.
type MachineConfigurationUpdateStrategy struct {
	// Type is OnDelete (the default) or RollingUpdate. With OnDelete, deleting
	// a Machine's Node object applies the version in its configurationRef;
	// Machines do not move to newer versions on their own. RollingUpdate is
	// not used yet and behaves like OnDelete.
	// +kubebuilder:default=OnDelete
	Type MachineConfigurationUpdateStrategyType `json:"type,omitempty"`

	// MaxUnavailable is how many machines a RollingUpdate may take down at
	// once. Not used yet.
	// +optional
	MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`
}

// MachineConfigurationStatus defines the observed state of a
// MachineConfiguration.
type MachineConfigurationStatus struct {
	// LatestVersion is the newest version number made for this
	// MachineConfiguration.
	LatestVersion int32 `json:"latestVersion,omitempty"`

	// TODO(#1038): drop the "Not set yet" note once versions are locked when
	// in use.

	// CurrentVersion is meant to hold the newest version that can still be
	// edited. Not set yet.
	CurrentVersion int32 `json:"currentVersion,omitempty"`

	// Conditions represent the latest available observations of the
	// MachineConfiguration's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
