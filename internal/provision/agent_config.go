// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"maps"
	"strings"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/cloudprovider"
)

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

// ClusterEndpoint holds the cluster-level connection parameters needed to
// build agent configuration. These values are typically resolved once at
// controller startup and reused across reconcile loops.
type ClusterEndpoint struct {
	// APIServer is the Kubernetes API server endpoint (e.g.
	// "my-cluster-dns.hcp.eastus.azmk8s.io:443").
	APIServer string

	// CACertBase64 is the base64-encoded cluster CA certificate.
	CACertBase64 string

	// ClusterDNS is the ClusterIP of the kube-dns Service.
	ClusterDNS string

	// KubeVersion is the cluster's Kubernetes version (e.g. "v1.34.0"),
	// used as a fallback when the Machine's Spec.Kubernetes.Version is
	// empty.
	KubeVersion string
}

// BuildAgentConfigParams holds the inputs for BuildAgentConfig.
// Cluster-level values are resolved once at controller startup and reused
// across reconcile loops. Machine-level values come from the Machine object.
type BuildAgentConfigParams struct {
	// Machine is the Machine object to build the config for.
	Machine *v1alpha3.Machine

	// Cluster holds the cluster-level connection parameters.
	Cluster ClusterEndpoint

	// ProviderLabels are cloud-provider-injected labels that override
	// all other labels. These are typically resolved from
	// cloudprovider.Provider.DefaultLabels().
	ProviderLabels map[string]string

	// BootstrapToken is the kubelet bootstrap token (e.g. "abc123.xyz789").
	// When empty, the agent is expected to obtain a token via attestation.
	BootstrapToken string

	// AttestURL is the base URL of a metalman serve-pxe instance for
	// TPM-based attestation (e.g. "http://10.0.0.1:8880"). When non-empty
	// an Attest section is included in the config.
	AttestURL string
}

// BuildAgentConfig constructs an AgentConfig from a Machine and cluster-level
// parameters. This is the canonical function used by all codepaths that
// produce agent configuration (machina SSH provisioner, metalman PXE
// templates, and kubectl-unbounded manual bootstrap).
//
// Label priority (lowest to highest):
//  1. User-defined labels from Machine.Spec.Kubernetes.NodeLabels.
//  2. Common labels applied unconditionally (e.g. cloud provider exclusion).
//  3. Provider-injected labels from params.ProviderLabels.
func BuildAgentConfig(params BuildAgentConfigParams) AgentConfig {
	machine := params.Machine

	// Resolve Kubernetes version: Machine spec overrides cluster default.
	k8sVersion := params.Cluster.KubeVersion
	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.Version != "" {
		k8sVersion = machine.Spec.Kubernetes.Version
	}

	if k8sVersion != "" && !strings.HasPrefix(k8sVersion, "v") {
		k8sVersion = "v" + k8sVersion
	}

	// Build labels with the documented priority.
	labels := map[string]string{}

	if machine.Spec.Kubernetes != nil {
		maps.Copy(labels, machine.Spec.Kubernetes.NodeLabels)
	}

	// Common labels are applied unconditionally to every node provisioned
	// by unbounded, regardless of the detected cloud provider.
	maps.Copy(labels, cloudprovider.CommonDefaultLabels())

	maps.Copy(labels, params.ProviderLabels)

	// Collect taints.
	var taints []string
	if machine.Spec.Kubernetes != nil {
		taints = machine.Spec.Kubernetes.RegisterWithTaints
	}

	// Resolve OCI image.
	var ociImage string
	if machine.Spec.Agent != nil {
		ociImage = machine.Spec.Agent.Image
	}

	cfg := AgentConfig{
		MachineName: machine.Name,
		Cluster: AgentClusterConfig{
			CaCertBase64: params.Cluster.CACertBase64,
			ClusterDNS:   params.Cluster.ClusterDNS,
			Version:      k8sVersion,
		},
		Kubelet: AgentKubeletConfig{
			ApiServer:          params.Cluster.APIServer,
			BootstrapToken:     params.BootstrapToken,
			Labels:             labels,
			RegisterWithTaints: taints,
		},
		OCIImage: ociImage,
	}

	if params.AttestURL != "" {
		cfg.Attest = &AgentAttestConfig{URL: params.AttestURL}
	}

	return cfg
}
