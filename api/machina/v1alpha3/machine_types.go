// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &Machine{}, &MachineList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

const (
	// MachineSiteLabelKey is the canonical site-membership label. It scopes a
	// Machine to a site and is also the canonical label the net controller
	// applies to Nodes, superseding the deprecated net.unbounded-cloud.io/site.
	MachineSiteLabelKey = "unbounded-cloud.io/site"
)

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

	// MachineConditionAgentBootstrapped indicates whether the unbounded
	// agent completed initial node bootstrap. Status is False with Reason
	// "Running" while the agent is preparing the nspawn node, False with
	// a failure reason when nspawn startup or kubelet TLS bootstrap fails,
	// and True with Reason "Succeeded" once the kubelet has bootstrapped.
	MachineConditionAgentBootstrapped = "AgentBootstrapped"

	// MachineConditionNodeUpdated indicates the result of a node
	// update performed by the agent daemon. Status is True with
	// Reason "Succeeded" after a successful update, and False with
	// Reason "Failed" when the update fails. While the update is in
	// progress the status is False with Reason "InProgress".
	MachineConditionNodeUpdated = "NodeUpdated"

	// MachineConditionConfigurationPending indicates that no
	// MachineConfiguration has been assigned to this Machine, either
	// directly via configurationRef or through a machineSelector
	// match. The Machine remains in a waiting state until a
	// configuration is assigned.
	MachineConditionConfigurationPending = "ConfigurationPending"

	// MachineConditionRepavePending indicates that the Machine's desired
	// configuration version has not yet been applied to the running Node.
	// For OnDelete updates this remains True until the operator deletes
	// the Node and the agent repaves with the desired version.
	MachineConditionRepavePending = "RepavePending"
)

// Annotation keys.
const (
	// AnnotationProvider associates a Machine with a provider's
	// controller for reboot/repave operations.
	AnnotationProvider = "unbounded-cloud.io/provider"

	// AnnotationConfigurationVersion is set on the Kubernetes Node
	// object to record which MachineConfigurationVersion was used to
	// bootstrap the machine.
	AnnotationConfigurationVersion = "unbounded-cloud.io/machine-configuration-version"
)

// MachineSpec defines the desired state of a Machine.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.providerRef) || (has(self.providerRef) && self.providerRef == oldSelf.providerRef)",message="providerRef is immutable once set"
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

	// Provider identifies the external control provider for this machine.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Provider string `json:"provider,omitempty"`

	// ProviderRef identifies the provider-owned Machine resource containing
	// provider-specific state for this Machine. Once set, the reference is
	// immutable. Provider controllers own the referenced resource's schema.
	// +optional
	ProviderRef *ProviderMachineReference `json:"providerRef,omitempty"`

	// ProviderID identifies the underlying infrastructure resource for this
	// machine, using a Kubernetes-style provider ID such as
	// azure:///subscriptions/.../virtualMachines/name or oci://ocid1.instance...
	//
	// ProviderID is deprecated in favor of ProviderRef and remains available
	// during the provider resource migration window.
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// Host contains provider-neutral desired host settings. Provider
	// controllers interpret these values for their infrastructure.
	// +optional
	Host *HostSpec `json:"host,omitempty"`

	// ConfigurationRef references a MachineConfiguration (and
	// optionally a specific version) that defines the configuration
	// profile for this machine. If a specific version is set, that
	// version is used at provisioning time; otherwise the latest
	// locked version is used and the version field is set by the
	// controller.
	// +optional
	ConfigurationRef *MachineConfigurationRef `json:"configurationRef,omitempty"`
}

// ProviderMachineReference identifies a cluster-scoped provider-owned Machine
// resource. API version is intentionally omitted so providers can evolve the
// served versions of their CRDs without rewriting every Machine reference.
type ProviderMachineReference struct {
	// APIGroup is the API group of the provider-owned Machine resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	APIGroup string `json:"apiGroup"`

	// Kind is the kind of the provider-owned Machine resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the name of the cluster-scoped provider-owned Machine resource.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// HostSpec contains provider-neutral desired host settings.
type HostSpec struct {
	// Image is an opaque provider-interpreted image identifier. If omitted,
	// HostReplace preserves the host's current image.
	// +optional
	Image string `json:"image,omitempty"`
}

// External provider names.
const (
	ExternalProviderAzureVM     = "AzureVM"
	ExternalProviderOCIInstance = "OCIInstance"
)

// MachineConfigurationRef references a MachineConfiguration and
// optionally a specific MachineConfigurationVersion.
type MachineConfigurationRef struct {
	// Name is the name of the MachineConfiguration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Version is the specific MachineConfigurationVersion number to
	// use. If omitted, the controller selects the latest locked
	// (deployed) version and populates this field.
	// +optional
	Version *int32 `json:"version,omitempty"`
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

// DHCPLease defines static IPv4 provisioning network settings.
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
	// Image is an OCI image reference containing the machine disk image.
	// The image must contain /disk/disk.img.gz.
	// Example: "ghcr.io/azure/host-ubuntu2404:v1"
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// PullSecretRef references a Docker registry credential Secret used to pull
	// Image. The Secret must be type kubernetes.io/dockerconfigjson or
	// kubernetes.io/dockercfg.
	// +optional
	PullSecretRef *NamespacedSecretReference `json:"pullSecretRef,omitempty"`

	// Architecture is the target CPU architecture for PXE boot artifacts and
	// machine images.
	// +kubebuilder:validation:Enum=amd64;arm64
	// +kubebuilder:default=amd64
	// +optional
	Architecture string `json:"architecture,omitempty"`

	// NetbootImage is an OCI image reference containing the PXE boot
	// artifacts used to install Image. When omitted, metalman uses the
	// default netboot image that corresponds to its release.
	// +optional
	NetbootImage string `json:"netbootImage,omitempty"`

	// NetbootPullSecretRef references a Docker registry credential Secret used to
	// pull NetbootImage. It is ignored when NetbootImage is omitted; metalman's
	// configured default netboot pull secret is used for the default netboot image.
	// The Secret must be type kubernetes.io/dockerconfigjson or
	// kubernetes.io/dockercfg.
	// +optional
	NetbootPullSecretRef *NamespacedSecretReference `json:"netbootPullSecretRef,omitempty"`

	// BootProtocol selects how metalman should trigger network boot for
	// repaves. PXE uses DHCP/TFTP bootfile options. HTTP uses Redfish UEFI
	// HTTP boot with a URL derived from the netboot image metadata.
	// +kubebuilder:validation:Enum=PXE;HTTP
	// +kubebuilder:default=PXE
	// +optional
	BootProtocol string `json:"bootProtocol,omitempty"`

	// DHCPLeases defines static IPv4 provisioning network settings. PXE boot
	// uses them as DHCP leases. HTTP boot uses the first entry to configure the
	// Redfish UEFI HTTP boot client.
	// +optional
	DHCPLeases []DHCPLease `json:"dhcpLeases,omitempty"`

	// TargetDisk is the block device the installer writes the machine image to.
	// Examples: /dev/nvme0n1, /dev/sda, /dev/disk/by-id/...
	// When omitted, the installer chooses a target disk automatically.
	// +optional
	TargetDisk string `json:"targetDisk,omitempty"`

	// Redfish configures optional Redfish BMC access.
	// +optional
	Redfish *RedfishSpec `json:"redfish,omitempty"`

	// CloudInit contains optional cloud-init customization for PXE-booted
	// machines.
	// +optional
	CloudInit *CloudInitSpec `json:"cloudInit,omitempty"`
}

const (
	// PXEBootProtocolPXE uses DHCP/TFTP PXE boot.
	PXEBootProtocolPXE = "PXE"
	// PXEBootProtocolHTTP uses Redfish UEFI HTTP boot.
	PXEBootProtocolHTTP = "HTTP"
	// PXEArchitectureAMD64 is the x86_64 target architecture for PXE boot.
	PXEArchitectureAMD64 = "amd64"
	// PXEArchitectureARM64 is the aarch64 target architecture for PXE boot.
	PXEArchitectureARM64 = "arm64"
	// DefaultPXEArchitecture is used when spec.pxe.architecture is omitted.
	DefaultPXEArchitecture = PXEArchitectureAMD64
	// DefaultPXEBootProtocol is used when spec.pxe.bootProtocol is omitted.
	DefaultPXEBootProtocol = PXEBootProtocolPXE
)

// TargetArchitecture returns the effective PXE target architecture.
func (p *PXESpec) TargetArchitecture() string {
	if p == nil || p.Architecture == "" {
		return DefaultPXEArchitecture
	}

	return p.Architecture
}

// TargetBootProtocol returns the effective network boot protocol.
func (p *PXESpec) TargetBootProtocol() string {
	if p == nil || p.BootProtocol == "" {
		return DefaultPXEBootProtocol
	}

	return p.BootProtocol
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
	// +optional
	BootstrapTokenRef *LocalObjectReference `json:"bootstrapTokenRef,omitempty"`
}

// AgentSpec defines settings for the unbounded node agent.
type AgentSpec struct {
	// Image is the OCI image reference used for provisioning the
	// nspawn machine (e.g. "ghcr.io/org/repo:tag"). When empty the
	// agent falls back to its built-in default image.
	// +optional
	Image string `json:"image,omitempty"`

	// Version pins the unbounded-agent release tag that is downloaded
	// onto the host (e.g. "v0.0.10"). When empty the install script
	// tracks the latest published release.
	// +optional
	Version string `json:"version,omitempty"`

	// BaseURL overrides the base URL used to construct the
	// unbounded-agent download URL. Defaults to the upstream GitHub
	// releases URL. The layout under BaseURL must match the GitHub
	// releases layout (<base>/latest/download/<asset> and
	// <base>/download/<tag>/<asset>).
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// URL is a fully qualified download URL for the unbounded-agent
	// tarball. When set it overrides Version and BaseURL entirely.
	// +optional
	URL string `json:"url,omitempty"`

	// Downloads overrides the download sources for the binaries the
	// agent installs into the nspawn rootfs (kubelet, containerd, runc,
	// CNI plugins, crictl). When unset the agent downloads each
	// artifact from its upstream default host.
	// +optional
	Downloads *AgentDownloadsSpec `json:"downloads,omitempty"`
}

// AgentDownloadsSpec overrides the download sources for the artifacts the
// agent installs into the nspawn rootfs. Each entry is optional; unset
// entries fall back to the upstream defaults.
type AgentDownloadsSpec struct {
	// Kubernetes overrides the download source for kubelet/kubectl/kube-proxy
	// (upstream default: https://dl.k8s.io).
	// +optional
	Kubernetes *DownloadSource `json:"kubernetes,omitempty"`

	// Containerd overrides the download source for containerd
	// (upstream default: https://github.com/containerd/containerd).
	// +optional
	Containerd *DownloadSource `json:"containerd,omitempty"`

	// Runc overrides the download source for runc
	// (upstream default: https://github.com/opencontainers/runc).
	// +optional
	Runc *DownloadSource `json:"runc,omitempty"`

	// CNI overrides the download source for CNI plugins
	// (upstream default: https://github.com/containernetworking/plugins).
	// +optional
	CNI *DownloadSource `json:"cni,omitempty"`

	// Crictl overrides the download source for crictl
	// (upstream default: https://github.com/kubernetes-sigs/cri-tools).
	// +optional
	Crictl *DownloadSource `json:"crictl,omitempty"`
}

// DownloadSource configures an override for a binary download source.
// Exactly one of URL or BaseURL should typically be set; when both are
// set URL wins. Version overrides the version that would otherwise be
// derived from the cluster Kubernetes version or agent defaults.
type DownloadSource struct {
	// BaseURL replaces the upstream host + path prefix used to
	// construct the download URL. Version and arch substitution are
	// preserved so mirrors need to publish assets under the same
	// layout as the upstream project.
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// URL is a fully qualified download URL template. Version/arch
	// substitution via fmt directives is preserved. When set it
	// overrides BaseURL entirely.
	// +optional
	URL string `json:"url,omitempty"`

	// Version overrides the version of the artifact that would
	// otherwise be derived from the cluster Kubernetes version or the
	// agent's compiled-in defaults.
	// +optional
	Version string `json:"version,omitempty"`
}

// LocalObjectReference contains enough information to locate the referenced resource.
type LocalObjectReference struct {
	// Name of the referenced resource.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// NamespacedSecretReference contains enough information to locate a Secret.
type NamespacedSecretReference struct {
	// Name of the secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the secret.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
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

	// Configuration records the MachineConfigurationVersion that was
	// applied to this machine during the most recent provisioning.
	// +optional
	Configuration *MachineConfigurationRefStatus `json:"configuration,omitempty"`

	// Conditions represent the latest available observations of the
	// machine's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MachineConfigurationRefStatus records which configuration version
// was applied to a Machine.
type MachineConfigurationRefStatus struct {
	// Name is the MachineConfiguration name.
	Name string `json:"name,omitempty"`

	// Version is the MachineConfigurationVersion number that was applied.
	Version int32 `json:"version,omitempty"`

	// VersionName is the full MachineConfigurationVersion object name
	// (e.g. "myconfig-v3").
	VersionName string `json:"versionName,omitempty"`
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
