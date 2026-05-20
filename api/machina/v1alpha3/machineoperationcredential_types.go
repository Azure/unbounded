// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&MachineOperationCredential{}, &MachineOperationCredentialList{})
}

// MachineOperationCredentialAuthMode identifies the credential source used by a
// MachineOperation controller.
// +kubebuilder:validation:Enum=WorkloadIdentity;ExternalPlugin
type MachineOperationCredentialAuthMode string

const (
	// MachineOperationCredentialAuthWorkloadIdentity uses OIDC/workload identity
	// credentials available to the controller process.
	MachineOperationCredentialAuthWorkloadIdentity MachineOperationCredentialAuthMode = "WorkloadIdentity"

	// MachineOperationCredentialAuthExternalPlugin delegates credential handling
	// to provider-specific external plugin configuration.
	MachineOperationCredentialAuthExternalPlugin MachineOperationCredentialAuthMode = "ExternalPlugin"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mocred
// +kubebuilder:printcolumn:name="Site",type="string",JSONPath=".spec.siteName"
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
// +kubebuilder:printcolumn:name="Auth Mode",type="string",JSONPath=".spec.auth.mode"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineOperationCredential defines the credential source used by external
// MachineOperation controllers for machines in a site.
type MachineOperationCredential struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MachineOperationCredentialSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// MachineOperationCredentialList contains a list of MachineOperationCredential.
type MachineOperationCredentialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineOperationCredential `json:"items"`
}

// MachineOperationCredentialSpec defines a provider credential source for a
// site.
type MachineOperationCredentialSpec struct {
	// SiteName is matched against the Machine site label.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SiteName string `json:"siteName"`

	// Provider identifies the external control provider this credential applies
	// to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=AzureVM;OCIInstance
	Provider string `json:"provider"`

	// Auth identifies how the provider should authenticate.
	// +kubebuilder:validation:Required
	Auth MachineOperationCredentialAuth `json:"auth"`
}

// MachineOperationCredentialAuth defines the credential source configuration.
type MachineOperationCredentialAuth struct {
	// Mode identifies the credential source.
	// +kubebuilder:validation:Required
	Mode MachineOperationCredentialAuthMode `json:"mode"`

	// SecretRef references provider-specific external plugin configuration. It
	// is required for ExternalPlugin mode.
	// +optional
	SecretRef *NamespacedSecretReference `json:"secretRef,omitempty"`
}
