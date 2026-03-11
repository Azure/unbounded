package v1alpha2

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MachinePhase represents the current phase of a Machine.
type MachinePhase string

const (
	MachinePhasePending      MachinePhase = "Pending"
	MachinePhaseReady        MachinePhase = "Ready"
	MachinePhaseProvisioning MachinePhase = "Provisioning"
	MachinePhaseProvisioned  MachinePhase = "Provisioned"
	MachinePhaseJoined       MachinePhase = "Joined"
	MachinePhaseOrphaned     MachinePhase = "Orphaned"
	MachinePhaseFailed       MachinePhase = "Failed"
)

// LocalObjectReference contains enough information to locate the referenced resource.
type LocalObjectReference struct {
	// Name of the referenced resource.
	Name string `json:"name"`
}

// SecretKeySelector selects a key from a Secret.
type SecretKeySelector struct {
	// Name of the secret.
	Name string `json:"name"`

	// Key within the secret.
	// +kubebuilder:default=ssh-privatekey
	Key string `json:"key,omitempty"`
}

// NodeReference references a Node by name.
type NodeReference struct {
	// Name is the name of the Node.
	Name string `json:"name"`
}

// MachineSSHSpec defines the SSH connection details for a Machine.
type MachineSSHSpec struct {
	// Host is the hostname or IP address of the machine.
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port is the SSH port of the machine.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=22
	Port int32 `json:"port,omitempty"`
}

// MachineSpec defines the desired state of a Machine.
type MachineSpec struct {
	// SSH contains the SSH connection details for the machine.
	// +kubebuilder:validation:Required
	SSH MachineSSHSpec `json:"ssh"`

	// ModelRef is an optional reference to a MachineModel. When set the
	// controller will provision the machine using the referenced model
	// once the machine is reachable.
	// +optional
	ModelRef *LocalObjectReference `json:"modelRef,omitempty"`
}

// MachineStatus defines the observed state of a Machine.
type MachineStatus struct {
	// Phase is the current phase of the machine.
	Phase MachinePhase `json:"phase,omitempty"`

	// Message provides additional status information.
	Message string `json:"message,omitempty"`

	// LastProbeTime is the last time the machine was probed.
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`

	// ProvisionedModelGeneration is the generation of the MachineModel
	// that was used to provision this machine. Set when phase is
	// Provisioned or Joined.
	ProvisionedModelGeneration int64 `json:"provisionedModelGeneration,omitempty"`

	// NodeRef references the Node that corresponds to this Machine.
	// Set when the machine transitions to Joined.
	// +optional
	NodeRef *NodeReference `json:"nodeRef,omitempty"`

	// Conditions represent the latest available observations of the machine's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".spec.ssh.host"
// +kubebuilder:printcolumn:name="Port",type="integer",JSONPath=".spec.ssh.port"
// +kubebuilder:printcolumn:name="Model",type="string",JSONPath=".spec.modelRef.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".status.nodeRef.name"
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

func init() {
	SchemeBuilder.Register(&Machine{}, &MachineList{})
}
