// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&MachineOperationCredential{}, &MachineOperationCredentialList{})
}

// MachineOperationAuthType identifies how a MachineOperation controller should
// authenticate to a cloud provider.
// +kubebuilder:validation:Enum=DefaultAzureCredential;ServicePrincipalSecret;APIKey
type MachineOperationAuthType string

const (
	// MachineOperationAuthDefaultAzureCredential uses the Azure SDK default
	// credential chain configured in the controller environment.
	MachineOperationAuthDefaultAzureCredential MachineOperationAuthType = "DefaultAzureCredential"

	// MachineOperationAuthServicePrincipalSecret uses an Azure service principal
	// stored in a referenced Secret.
	MachineOperationAuthServicePrincipalSecret MachineOperationAuthType = "ServicePrincipalSecret"

	// MachineOperationAuthAPIKey uses provider-specific API key fields stored in
	// a referenced Secret.
	MachineOperationAuthAPIKey MachineOperationAuthType = "APIKey"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mocred
// +kubebuilder:printcolumn:name="Site",type="string",JSONPath=".spec.siteName"
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
// +kubebuilder:printcolumn:name="Auth Type",type="string",JSONPath=".spec.authType"
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

	// AuthType identifies how the provider should authenticate.
	// +kubebuilder:validation:Required
	AuthType MachineOperationAuthType `json:"authType"`

	// SecretRef references provider-specific credential material. It is not
	// required for auth types that use ambient controller credentials.
	// +optional
	SecretRef *NamespacedSecretReference `json:"secretRef,omitempty"`
}
