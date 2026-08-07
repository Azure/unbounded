// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "github.com/Azure/unbounded/pkg/agent/config"

// Kubelet defines the goal state for the kubelet configuration.
type Kubelet struct {
	// KubeletBinPath is the absolute path to the kubelet binary inside the
	// machine container (e.g. /usr/local/bin/kubelet).
	KubeletBinPath string

	// KubeletAuthInfo holds the authentication configuration for the
	// kubelet. Exactly one of BootstrapToken or ExecCredential must be set.
	config.KubeletAuthInfo

	// APIServer is the HTTPS endpoint of the Kubernetes API server
	// (e.g. "https://my-cluster.hcp.eastus.azmk8s.io:443").
	APIServer string

	// CACertData is the PEM-encoded CA certificate of the API server.
	CACertData []byte

	// ClusterDNS is the ClusterIP of the kube-dns service.
	ClusterDNS string

	// NodeIP overrides kubelet --node-ip. Supports a single IP or a
	// comma-separated dual-stack pair.
	NodeIP string

	// NodeLabels are key=value labels applied to the node at registration.
	NodeLabels map[string]string

	// RegisterWithTaints are taints applied to the node at registration.
	// Each entry uses the kubelet format: "key=value:effect"
	// (e.g. "dedicated=gpu:NoSchedule").
	RegisterWithTaints []string

	// Configuration is merged over the agent's baseline and rendered as a
	// kubelet.config.k8s.io/v1beta1 KubeletConfiguration.
	Configuration map[string]any

	// ImageCredentialProvider holds optional kubelet exec image credential
	// provider paths inside the nspawn machine.
	ImageCredentialProvider *ImageCredentialProvider
}

// ImageCredentialProvider holds paths inside the nspawn machine for kubelet's
// exec image credential provider configuration and binaries.
type ImageCredentialProvider struct {
	ConfigPath string
	BinDir     string
}
