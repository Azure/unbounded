package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubernetesProfile defines Kubernetes-specific configuration.
type KubernetesProfile struct {
	// Version is the Kubernetes version to install (e.g., "1.34.0").
	// When omitted the controller falls back to the cluster's Kubernetes version.
	// +optional
	Version string `json:"version,omitempty"`

	// BootstrapTokenRef references a bootstrap token Secret in kube-system.
	// The secret must be of type bootstrap.kubernetes.io/token with the
	// well-known keys "token-id" and "token-secret". The controller
	// combines them as "<token-id>.<token-secret>".
	// +kubebuilder:validation:Required
	BootstrapTokenRef LocalObjectReference `json:"bootstrapTokenRef"`
}

// JumpboxConfig configures an SSH jump host (bastion) for proxy connections.
type JumpboxConfig struct {
	// Host is the hostname or IP address of the jumpbox
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port is the SSH port of the jumpbox
	// +kubebuilder:default=22
	Port int32 `json:"port,omitempty"`

	// SSHUsername is the SSH username for the jumpbox
	// +kubebuilder:default=azureuser
	SSHUsername string `json:"sshUsername,omitempty"`

	// SSHPrivateKeyRef references a secret containing the SSH private key for the jumpbox.
	// If not specified, uses the same key as the model's SSHPrivateKeyRef.
	// +optional
	SSHPrivateKeyRef *SecretKeySelector `json:"sshPrivateKeyRef,omitempty"`
}

// MachineModelSpec defines the desired state of a MachineModel.
type MachineModelSpec struct {
	// SSHUsername is the SSH username for machines using this model
	// +kubebuilder:default=azureuser
	SSHUsername string `json:"sshUsername,omitempty"`

	// SSHPrivateKeyRef references a secret containing the SSH private key
	// +kubebuilder:validation:Required
	SSHPrivateKeyRef SecretKeySelector `json:"sshPrivateKeyRef"`

	// Jumpbox configures an optional jump host (bastion) for SSH proxy connections.
	// The controller will open a connection to the jumpbox and then jump to the
	// target machine.
	// +optional
	Jumpbox *JumpboxConfig `json:"jumpbox,omitempty"`

	// AgentInstallScript is the script that will be run on the target machine
	// to install and configure the agent.
	// +kubebuilder:validation:Required
	AgentInstallScript string `json:"agentInstallScript"`

	// KubernetesProfile contains Kubernetes-specific configuration
	// +optional
	KubernetesProfile *KubernetesProfile `json:"kubernetesProfile,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="K8s Version",type="string",JSONPath=".spec.kubernetesProfile.version"
// +kubebuilder:printcolumn:name="SSH User",type="string",JSONPath=".spec.sshUsername"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// MachineModel defines a model that machines can adhere to for provisioning.
type MachineModel struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MachineModelSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// MachineModelList contains a list of MachineModel.
type MachineModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineModel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MachineModel{}, &MachineModelList{})
}
