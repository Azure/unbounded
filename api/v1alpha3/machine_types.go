// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&Machine{}, &MachineList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mach
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".spec.ssh.host"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="K8s Version",type="string",JSONPath=".spec.kubernetes.version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Machine represents a machine that can be managed by machina.
type Machine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineSpec   `json:"spec,omitempty"`
	Status MachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineList contains a list of Machine.
type MachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Machine `json:"items"`
}

// Condition types for Machine.
const (
	// MachineConditionProvisioned indicates that the machine has been
	// successfully provisioned. The observedGeneration field on the
	// condition tracks which generation of the Machine spec was
	// provisioned.
	MachineConditionProvisioned = "Provisioned"

	// MachineConditionSSHReachable indicates whether the machine is
	// reachable via SSH. The lastTransitionTime and message fields are
	// updated on probe results.
	MachineConditionSSHReachable = "SSHReachable"

	// MachineConditionProvisioning indicates that the machine is
	// currently being provisioned. The lastTransitionTime records when
	// provisioning started, which is used to detect stale provisioning
	// attempts (e.g. after a controller restart).
	MachineConditionProvisioning = "Provisioning"

	// MachineConditionReimaged indicates the state of a reimage operation.
	// Status is set to False (with Reason "Pending") when a reimage begins,
	// and True (with Reason "Succeeded") when the reimage completes.
	// The lastTransitionTime records when the reimage started, which is
	// used to detect stale reimage attempts.
	MachineConditionReimaged = "Reimaged"

	// MachineConditionCloudInitDone indicates whether cloud-init has
	// finished on the machine. Status is True with Reason "Succeeded"
	// when cloud-init completes without errors, False with Reason
	// "Running" while cloud-init stages are still executing, and
	// False with Reason "Failed" when a cloud-init stage reports a
	// failure. On failure the message includes the stage name and the
	// error result so that operators can diagnose the problem without
	// logging into the machine.
	MachineConditionCloudInitDone = "CloudInitDone"
)

// Annotation keys.
const (
	// AnnotationProvider associates a Machine with a provider's
	// controller for reboot/reimage operations.
	AnnotationProvider = "unbounded-kube.io/provider"
)

// MachineSpec defines the desired state of a Machine.
type MachineSpec struct {
	// SSH contains the SSH connection and credential details for the
	// machine.
	// +optional
	SSH *SSHSpec `json:"ssh,omitempty"`

	// PXE contains PXE boot configuration for the machine.
	// +optional
	PXE *PXESpec `json:"pxe,omitempty"`

	// Kubernetes contains Kubernetes-specific configuration.
	// +optional
	Kubernetes *KubernetesSpec `json:"kubernetes,omitempty"`

	// Agent contains settings for the unbounded node agent.
	// +optional
	Agent *AgentSpec `json:"agent,omitempty"`

	// Operations contains counter-based operation triggers.
	// +optional
	Operations *OperationsSpec `json:"operations,omitempty"`
}

// SSHSpec defines SSH connection details. The same structure is reused
// for both the target machine and the optional bastion host.
type SSHSpec struct {
	// Host is the hostname or IP address of the machine, optionally
	// including the port (e.g. "1.2.3.4:2222"). When the port is
	// omitted, 22 is assumed.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Username is the SSH username.
	// +kubebuilder:default=azureuser
	Username string `json:"username,omitempty"`

	// PrivateKeyRef references a secret containing the SSH private key.
	// +kubebuilder:validation:Required
	PrivateKeyRef SecretKeySelector `json:"privateKeyRef"`

	// Bastion configures an optional SSH jump host (bastion) for proxy
	// connections. Its structure is identical to SSHSpec minus the
	// bastion field itself.
	// +optional
	Bastion *BastionSSHSpec `json:"bastion,omitempty"`
}

// BastionSSHSpec defines SSH connection details for a bastion host.
// It mirrors SSHSpec but omits the recursive Bastion field.
type BastionSSHSpec struct {
	// Host is the hostname or IP address of the bastion, optionally
	// including the port (e.g. "1.2.3.4:2222"). When the port is
	// omitted, 22 is assumed.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Username is the SSH username for the bastion.
	// +kubebuilder:default=azureuser
	Username string `json:"username,omitempty"`

	// PrivateKeyRef references a secret containing the SSH private key
	// for the bastion. If not specified, uses the same key as the
	// parent SSHSpec.
	// +optional
	PrivateKeyRef *SecretKeySelector `json:"privateKeyRef,omitempty"`
}

// DHCPLease defines a static DHCP lease for PXE booting.
type DHCPLease struct {
	// IPv4 is the IP address to assign.
	IPv4 string `json:"ipv4"`

	// MAC is the MAC address of the network interface.
	MAC string `json:"mac"`

	// SubnetMask is the subnet mask for the lease.
	SubnetMask string `json:"subnetMask"`

	// Gateway is the default gateway.
	Gateway string `json:"gateway"`

	// DNS is a list of DNS server addresses.
	// +optional
	DNS []string `json:"dns,omitempty"`
}

// RedfishSpec defines Redfish BMC connection details.
type RedfishSpec struct {
	// URL is the Redfish endpoint URL.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Username is the Redfish username.
	// +kubebuilder:validation:Required
	Username string `json:"username"`

	// DeviceID is the Redfish system device ID. Defaults to "1".
	// +kubebuilder:default="1"
	DeviceID string `json:"deviceID,omitempty"`

	// PasswordRef references a secret containing the Redfish password.
	// +kubebuilder:validation:Required
	PasswordRef SecretKeySelector `json:"passwordRef"`
}

// PXESpec defines PXE boot configuration for a Machine.
type PXESpec struct {
	// Image is an OCI image reference containing netboot artifacts.
	// Example: "ghcr.io/azure/images/host-ubuntu2404:v1"
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// DHCPLeases defines static DHCP leases for PXE booting.
	// +optional
	DHCPLeases []DHCPLease `json:"dhcpLeases,omitempty"`

	// Redfish configures optional Redfish BMC access.
	// +optional
	Redfish *RedfishSpec `json:"redfish,omitempty"`

	// CloudInit contains optional cloud-init customization for PXE-booted
	// machines.
	// +optional
	CloudInit *CloudInitSpec `json:"cloudInit,omitempty"`
}

// CloudInitSpec defines cloud-init customization for PXE-booted machines.
// Cloud-init merges vendor-data (managed by unbounded-kube) with user-data
// (managed by the cluster operator). This spec controls the user-data
// portion, allowing operators to configure SSH keys, install packages,
// and perform other host-level customization.
type CloudInitSpec struct {
	// UserDataConfigMapRef references a ConfigMap containing custom
	// cloud-init user-data. The referenced key (default "user-data")
	// must contain a valid cloud-init configuration (e.g. a
	// #cloud-config YAML document).
	// +optional
	UserDataConfigMapRef *ConfigMapKeySelector `json:"userDataConfigMapRef,omitempty"`
}

// ConfigMapKeySelector selects a key from a ConfigMap.
type ConfigMapKeySelector struct {
	// Name of the ConfigMap.
	Name string `json:"name"`

	// Namespace of the ConfigMap.
	Namespace string `json:"namespace"`

	// Key within the ConfigMap.
	// +kubebuilder:default=user-data
	Key string `json:"key,omitempty"`
}

// KubernetesSpec defines Kubernetes-specific configuration for a Machine.
type KubernetesSpec struct {
	// Version is the Kubernetes version to install (e.g., "v1.34.0").
	// When omitted the controller falls back to the cluster's
	// Kubernetes version.
	// +optional
	Version string `json:"version,omitempty"`

	// NodeRef references the Node that corresponds to this Machine.
	// +optional
	NodeRef *LocalObjectReference `json:"nodeRef,omitempty"`

	// NodeLabels are labels passed to kubelet's --node-labels flag.
	// +optional
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`

	// RegisterWithTaints are taints passed to kubelet's --register-with-taints flag.
	// Each entry uses the standard Kubernetes taint format: key=value:Effect.
	// +optional
	RegisterWithTaints []string `json:"registerWithTaints,omitempty"`

	// BootstrapTokenRef references a bootstrap token Secret in
	// kube-system. The secret must be of type
	// bootstrap.kubernetes.io/token with the well-known keys
	// "token-id" and "token-secret".
	// +kubebuilder:validation:Required
	BootstrapTokenRef LocalObjectReference `json:"bootstrapTokenRef"`
}

// AgentSpec defines settings for the unbounded node agent.
type AgentSpec struct {
	// Image is the OCI image reference used for provisioning the
	// nspawn machine (e.g. "ghcr.io/org/repo:tag").
	// +kubebuilder:validation:Required
	Image string `json:"image"`
}

// OperationsSpec defines counter-based operation triggers.
// Controllers compare spec counters against status counters to
// determine if an operation is needed.
type OperationsSpec struct {
	// RebootCounter triggers a reboot when it exceeds the status
	// reboot counter.
	// +optional
	RebootCounter int64 `json:"rebootCounter,omitempty"`

	// ReimageCounter triggers a reimage when it exceeds the status
	// reimage counter.
	// +optional
	ReimageCounter int64 `json:"reimageCounter,omitempty"`
}

// LocalObjectReference contains enough information to locate the referenced resource.
type LocalObjectReference struct {
	// Name of the referenced resource.
	Name string `json:"name"`
}

// SecretKeySelector selects a key from a Secret.
type SecretKeySelector struct {
	// Name of the secret.
	Name string `json:"name"`

	// Namespace of the secret.
	Namespace string `json:"namespace"`

	// Key within the secret.
	// +kubebuilder:default=ssh-privatekey
	Key string `json:"key,omitempty"`
}

// MachineStatus defines the observed state of a Machine.
type MachineStatus struct {
	// Phase is the current phase of the machine. Intended for human
	// consumption; follows the state machine rather than driving it.
	Phase MachinePhase `json:"phase,omitempty"`

	// Message provides additional status information.
	Message string `json:"message,omitempty"`

	// SSH holds observed SSH state.
	// +optional
	SSH *SSHStatus `json:"ssh,omitempty"`

	// Redfish holds observed Redfish state.
	// +optional
	Redfish *RedfishStatus `json:"redfish,omitempty"`

	// TPM holds observed TPM state.
	// +optional
	TPM *TPMStatus `json:"tpm,omitempty"`

	// Agent holds the applied agent settings.
	// +optional
	Agent *AgentStatus `json:"agent,omitempty"`

	// Operations holds the last-observed operation counters.
	// +optional
	Operations *OperationsStatus `json:"operations,omitempty"`

	// Conditions represent the latest available observations of the
	// machine's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachinePhase represents the current phase of a Machine.
type MachinePhase string

const (
	MachinePhasePending      MachinePhase = "Pending"
	MachinePhaseRebooting    MachinePhase = "Rebooting"
	MachinePhaseProvisioning MachinePhase = "Provisioning"
	MachinePhaseJoining      MachinePhase = "Joining"
	MachinePhaseReady        MachinePhase = "Ready"
	MachinePhaseFailed       MachinePhase = "Failed"
)

// SSHStatus holds observed SSH state.
type SSHStatus struct {
	// Fingerprint is the SSH host key fingerprint discovered on
	// first connection. Subsequent connections must match this value.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// RedfishStatus holds observed Redfish state.
type RedfishStatus struct {
	// CertFingerprint is the TLS certificate fingerprint for the
	// Redfish endpoint, pinned on first connection.
	CertFingerprint string `json:"certFingerprint,omitempty"`
}

// TPMStatus holds observed TPM state.
type TPMStatus struct {
	// EKPublicKey is the TPM endorsement key public key, written
	// when the PXE boot image requests a bootstrap token.
	EKPublicKey string `json:"ekPublicKey,omitempty"`
}

// AgentStatus holds the applied agent settings for the machine.
type AgentStatus struct {
	// Image is the OCI image reference that was applied to the
	// nspawn machine.
	Image string `json:"image,omitempty"`
}

// OperationsStatus holds the last-observed operation counters.
type OperationsStatus struct {
	// RebootCounter is the last reboot counter value that was acted on.
	RebootCounter int64 `json:"rebootCounter,omitempty"`

	// ReimageCounter is the last reimage counter value that was acted on.
	ReimageCounter int64 `json:"reimageCounter,omitempty"`
}
