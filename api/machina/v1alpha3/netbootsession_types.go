// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &NetbootSession{}, &NetbootSessionList{})
		metav1.AddToGroupVersion(s, GroupVersion)

		return nil
	})
}

// NetbootSessionPhase is the durable lifecycle phase of a provisioning session.
// +kubebuilder:validation:Enum=Pending;Preparing;Ready;Active;Complete;Failed;Expired
type NetbootSessionPhase string

const (
	NetbootSessionPhasePending   NetbootSessionPhase = "Pending"
	NetbootSessionPhasePreparing NetbootSessionPhase = "Preparing"
	NetbootSessionPhaseReady     NetbootSessionPhase = "Ready"
	NetbootSessionPhaseActive    NetbootSessionPhase = "Active"
	NetbootSessionPhaseComplete  NetbootSessionPhase = "Complete"
	NetbootSessionPhaseFailed    NetbootSessionPhase = "Failed"
	NetbootSessionPhaseExpired   NetbootSessionPhase = "Expired"
)

// Condition types for NetbootSession.
const (
	NetbootSessionConditionPrepared             = "Prepared"
	NetbootSessionConditionEndpointReady        = "EndpointReady"
	NetbootSessionConditionBootLoaderDownloaded = "BootLoaderDownloaded"
	NetbootSessionConditionBootImageWritten     = "BootImageWritten"
	NetbootSessionConditionCloudInitDone        = "CloudInitDone"
	NetbootSessionConditionAttested             = "Attested"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=nbs
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Machine",type="string",JSONPath=".spec.machine.name"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.endpoint.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Expires",type="date",JSONPath=".spec.expiresAt"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NetbootSession is an immutable provisioning contract for one
// MachineOperation target. Runtime progress is recorded only in status.
type NetbootSession struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetbootSessionSpec   `json:"spec,omitempty"`
	Status NetbootSessionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetbootSessionList contains a list of NetbootSession resources.
type NetbootSessionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetbootSession `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="netboot session spec is immutable"

// NetbootSessionSpec snapshots all input needed to provision one target.
type NetbootSessionSpec struct {
	// Machine identifies the exact Machine revision being provisioned.
	Machine NetbootSessionObjectSnapshot `json:"machine"`

	// Operation identifies the owning MachineOperation.
	Operation NetbootSessionObjectSnapshot `json:"operation"`

	// Endpoint snapshots the selected endpoint and advertised URL.
	Endpoint NetbootSessionEndpointSnapshot `json:"endpoint"`

	// Boot snapshots firmware and provisioning network configuration.
	Boot NetbootSessionBoot `json:"boot"`

	// Provisioning snapshots inputs used to render installer and first-boot
	// configuration.
	Provisioning NetbootSessionProvisioning `json:"provisioning"`

	// Artifacts identifies immutable OCI sources and files for this session.
	Artifacts NetbootSessionArtifacts `json:"artifacts"`

	// ExpiresAt is the last time new requests may use this session.
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// NetbootSessionObjectSnapshot identifies an exact Kubernetes object revision.
type NetbootSessionObjectSnapshot struct {
	// Name is the cluster-scoped object name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID detects deletion and recreation of the object.
	UID types.UID `json:"uid"`

	// Generation records the observed desired-state generation.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// NetbootSessionEndpointSnapshot identifies the endpoint selected for a session.
type NetbootSessionEndpointSnapshot struct {
	// Name is the NetbootEndpoint name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID detects deletion and recreation of the endpoint.
	UID types.UID `json:"uid"`

	// ExternalURL is the immutable base URL advertised for this session.
	// +kubebuilder:validation:Pattern=`^https?://`
	ExternalURL string `json:"externalURL"`
}

// NetbootSessionBoot snapshots firmware and provisioning network settings.
type NetbootSessionBoot struct {
	Transport           NetbootTransport           `json:"transport"`
	ConfigurationSource NetbootConfigurationSource `json:"configurationSource"`
	NetworkMode         NetbootNetworkMode         `json:"networkMode"`

	// FirmwareArtifact is the named immutable artifact advertised to firmware.
	// +kubebuilder:validation:MinLength=1
	FirmwareArtifact string `json:"firmwareArtifact"`

	// Architecture selects the boot artifact platform.
	// +kubebuilder:validation:Enum=amd64;arm64
	Architecture string `json:"architecture"`

	// DHCPLeases snapshots the target's provisioning network settings.
	// +optional
	DHCPLeases []DHCPLease `json:"dhcpLeases,omitempty"`

	// TargetDisk is the block device written by the installer.
	// +optional
	TargetDisk string `json:"targetDisk,omitempty"`
}

// NetbootSessionProvisioning snapshots installer and first-boot inputs.
type NetbootSessionProvisioning struct {
	Cluster NetbootSessionCluster `json:"cluster"`

	// Kubernetes contains the target's immutable kubelet configuration.
	// +optional
	Kubernetes *KubernetesSpec `json:"kubernetes,omitempty"`

	// Agent contains the immutable agent installation configuration.
	// +optional
	Agent *AgentSpec `json:"agent,omitempty"`

	// ProviderLabels are merged into the rendered kubelet labels.
	// +optional
	ProviderLabels map[string]string `json:"providerLabels,omitempty"`

	// UserData is the resolved cloud-init user-data content.
	UserData string `json:"userData"`
}

// NetbootSessionCluster snapshots cluster connection inputs used by the agent.
type NetbootSessionCluster struct {
	APIServerURL string `json:"apiServerURL"`
	CACertBase64 string `json:"caCertBase64"`
	DNS          string `json:"dns"`

	// KubernetesVersion is the cluster version used when the Machine does not
	// specify one.
	KubernetesVersion string `json:"kubernetesVersion"`
}

// NetbootSessionArtifacts snapshots immutable OCI sources and artifact paths.
type NetbootSessionArtifacts struct {
	MachineImage NetbootSessionImage `json:"machineImage"`
	NetbootImage NetbootSessionImage `json:"netbootImage"`

	// Files lists the named files an edge may request for this session.
	// +listType=map
	// +listMapKey=name
	Files []NetbootSessionArtifact `json:"files"`
}

// NetbootSessionImage identifies an OCI image resolved to an immutable digest.
type NetbootSessionImage struct {
	// Reference is the source repository reference used to resolve the image.
	// +kubebuilder:validation:MinLength=1
	Reference string `json:"reference"`

	// Digest is the immutable OCI manifest digest.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// PullSecretRef references registry credentials without copying them into
	// the session.
	// +optional
	PullSecretRef *NamespacedSecretReference `json:"pullSecretRef,omitempty"`
}

// NetbootSessionArtifact maps a public artifact name to an immutable image path.
type NetbootSessionArtifact struct {
	// Name is the stable route name used by edges and rendered templates.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Source selects the OCI image containing this file, or Session for
	// content snapshotted directly into the session.
	// +kubebuilder:validation:Enum=MachineImage;NetbootImage;Session
	Source string `json:"source"`

	// Path is the absolute path within the unpacked OCI image.
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`

	// Size is the expected file size when known.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Size *int64 `json:"size,omitempty"`
}

// NetbootSessionStatus reports preparation and target-scoped milestones.
type NetbootSessionStatus struct {
	// Phase is the current durable lifecycle phase.
	// +optional
	Phase NetbootSessionPhase `json:"phase,omitempty"`

	// CapabilityID identifies the signing key and capability generation without
	// persisting a bearer capability.
	// +optional
	CapabilityID string `json:"capabilityID,omitempty"`

	// Conditions contain preparation, endpoint readiness, and target-scoped
	// provisioning milestones.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NetbootSessionReference identifies the immutable session assigned to an
// operation target.
type NetbootSessionReference struct {
	// Name is the NetbootSession name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID detects deletion and recreation of the session.
	UID types.UID `json:"uid"`
}
