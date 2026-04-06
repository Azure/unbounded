package provision

// AgentConfig is the configuration document uploaded to the remote machine
// before running the agent install script. The script reads it via the
// UNBOUNDED_AGENT_CONFIG_FILE environment variable.
type AgentConfig struct {
	MachineName string             `json:"MachineName"`
	Cluster     AgentClusterConfig `json:"Cluster"`
	Kubelet     AgentKubeletConfig `json:"Kubelet"`

	// OCIImage is the fully-qualified OCI image reference (e.g.
	// "ghcr.io/org/repo:tag") used to bootstrap the machine rootfs.
	// When empty the agent falls back to debootstrap.
	OCIImage string `json:"OCIImage,omitempty"`

	// Attest configures TPM-based attestation for obtaining a bootstrap
	// token from a metalman serve-pxe instance. When set, the agent
	// performs TPM attestation on the host instead of requiring a static
	// BootstrapToken in Kubelet config.
	Attest *AgentAttestConfig `json:"Attest,omitempty"`
}

// AgentClusterConfig holds the cluster-level values the agent needs to
// join the Kubernetes control plane.
type AgentClusterConfig struct {
	CaCertBase64 string `json:"CaCertBase64"`
	ClusterDNS   string `json:"ClusterDNS"`
	Version      string `json:"Version"`
}

// AgentKubeletConfig holds kubelet-specific overrides.
type AgentKubeletConfig struct {
	ApiServer          string            `json:"ApiServer"`
	BootstrapToken     string            `json:"BootstrapToken,omitempty"`
	Labels             map[string]string `json:"Labels"`
	RegisterWithTaints []string          `json:"RegisterWithTaints"`
}

// AgentAttestConfig holds configuration for TPM-based attestation against
// a metalman serve-pxe instance.
type AgentAttestConfig struct {
	// URL is the base URL of the metalman serve-pxe instance (e.g.
	// "http://10.0.0.1:8880"). The agent appends "/attest" to this URL
	// when performing TPM attestation.
	URL string `json:"URL"`
}
