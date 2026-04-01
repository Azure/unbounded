package provision

// AgentConfig is the configuration document uploaded to the remote machine
// before running the agent install script. The script reads it via the
// UNBOUNDED_AGENT_CONFIG_FILE environment variable.
type AgentConfig struct {
	MachineName string             `json:"MachineName"`
	Cluster     AgentClusterConfig `json:"Cluster"`
	Kubelet     AgentKubeletConfig `json:"Kubelet"`
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
	BootstrapToken     string            `json:"BootstrapToken"`
	Labels             map[string]string `json:"Labels"`
	RegisterWithTaints []string          `json:"RegisterWithTaints"`
}
