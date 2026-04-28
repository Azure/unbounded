// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package provision

import (
	"maps"
	"strings"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/cloudprovider"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

type (
	AgentConfig        = config.AgentConfig
	AgentClusterConfig = config.AgentClusterConfig
	AgentKubeletConfig = config.AgentKubeletConfig
	KubeletAuthInfo    = config.KubeletAuthInfo
	CRIConfig          = config.CRIConfig
	ContainerdConfig   = config.ContainerdConfig
	RuncConfig         = config.RuncConfig
	CNIConfig          = config.CNIConfig
)

// UnboundedAgentConfig extends the shared AgentConfig with unbounded-specific
// fields that are not part of the public agent IR. Controllers and the
// agent CLI use this type; the shared agent library uses only AgentConfig.
type UnboundedAgentConfig struct {
	config.AgentConfig

	// Attest configures TPM-based attestation for obtaining a bootstrap
	// token from a metalman serve-pxe instance. When set, the agent
	// performs TPM attestation on the host instead of requiring a static
	// BootstrapToken in the Kubelet.Auth config.
	Attest *AgentAttestConfig `json:"Attest,omitempty"`

	// Downloads optionally overrides the download sources for binaries
	// the agent installs into the nspawn rootfs (kubelet, containerd,
	// runc, CNI plugins, crictl). When unset the agent downloads each
	// artifact from its upstream default host.
	Downloads *AgentDownloads `json:"Downloads,omitempty"`
}

// AgentAttestConfig holds configuration for TPM-based attestation against
// a metalman serve-pxe instance.
type AgentAttestConfig struct {
	// URL is the base URL of the metalman serve-pxe instance (e.g.
	// "http://10.0.0.1:8880"). The agent appends "/attest" to this URL
	// when performing TPM attestation.
	URL string `json:"URL"`
}

// AgentDownloads optionally overrides the download sources for the
// binaries the agent installs into the nspawn rootfs. Each entry is
// optional; unset entries fall back to the upstream defaults compiled
// into the agent.
type AgentDownloads struct {
	Kubernetes *AgentDownloadSource `json:"Kubernetes,omitempty"`
	Containerd *AgentDownloadSource `json:"Containerd,omitempty"`
	Runc       *AgentDownloadSource `json:"Runc,omitempty"`
	CNI        *AgentDownloadSource `json:"CNI,omitempty"`
	Crictl     *AgentDownloadSource `json:"Crictl,omitempty"`
}

// AgentDownloadSource configures an override for a single binary download
// source. BaseURL replaces the upstream host + path prefix; URL replaces
// the entire URL template. Version overrides the version that would
// otherwise be derived from the cluster Kubernetes version or the agent's
// compiled-in defaults.
type AgentDownloadSource struct {
	BaseURL string `json:"BaseURL,omitempty"`
	URL     string `json:"URL,omitempty"`
	Version string `json:"Version,omitempty"`
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
func BuildAgentConfig(params BuildAgentConfigParams) UnboundedAgentConfig {
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

	// Resolve download overrides from the Machine spec.
	var downloads *AgentDownloads
	if machine.Spec.Agent != nil && machine.Spec.Agent.Downloads != nil {
		downloads = agentDownloadsFromSpec(machine.Spec.Agent.Downloads)
	}

	cfg := UnboundedAgentConfig{
		AgentConfig: config.AgentConfig{
			MachineName: machine.Name,
			Cluster: AgentClusterConfig{
				CaCertBase64: params.Cluster.CACertBase64,
				ClusterDNS:   params.Cluster.ClusterDNS,
				Version:      k8sVersion,
			},
			Kubelet: AgentKubeletConfig{
				ApiServer: params.Cluster.APIServer,
				Auth: KubeletAuthInfo{
					BootstrapToken: params.BootstrapToken,
				},
				Labels:             labels,
				RegisterWithTaints: taints,
			},
			OCIImage: ociImage,
		},
		Downloads: downloads,
	}

	if params.AttestURL != "" {
		cfg.Attest = &AgentAttestConfig{URL: params.AttestURL}
	}

	return cfg
}

// agentDownloadsFromSpec converts the Machine API AgentDownloadsSpec into
// the agent-facing AgentDownloads config. Returns nil if every entry is
// unset, so the resulting JSON config remains minimal.
func agentDownloadsFromSpec(spec *v1alpha3.AgentDownloadsSpec) *AgentDownloads {
	if spec == nil {
		return nil
	}

	out := &AgentDownloads{
		Kubernetes: downloadSourceFromSpec(spec.Kubernetes),
		Containerd: downloadSourceFromSpec(spec.Containerd),
		Runc:       downloadSourceFromSpec(spec.Runc),
		CNI:        downloadSourceFromSpec(spec.CNI),
		Crictl:     downloadSourceFromSpec(spec.Crictl),
	}

	if out.Kubernetes == nil && out.Containerd == nil && out.Runc == nil && out.CNI == nil && out.Crictl == nil {
		return nil
	}

	return out
}

func downloadSourceFromSpec(s *v1alpha3.DownloadSource) *AgentDownloadSource {
	if s == nil {
		return nil
	}

	if s.BaseURL == "" && s.URL == "" && s.Version == "" {
		return nil
	}

	return &AgentDownloadSource{
		BaseURL: s.BaseURL,
		URL:     s.URL,
		Version: s.Version,
	}
}

// ResolveDownloadOverrides converts the provision AgentDownloads (from the
// agent config JSON) into the goalstates.DownloadOverrides shape that
// rootfs phase tasks consume. Returns nil when no overrides are set.
func ResolveDownloadOverrides(d *AgentDownloads) *goalstates.DownloadOverrides {
	if d == nil {
		return nil
	}

	convert := func(s *AgentDownloadSource) *goalstates.DownloadSource {
		if s == nil {
			return nil
		}
		if s.BaseURL == "" && s.URL == "" && s.Version == "" {
			return nil
		}
		return &goalstates.DownloadSource{
			BaseURL: s.BaseURL,
			URL:     s.URL,
			Version: s.Version,
		}
	}

	out := &goalstates.DownloadOverrides{
		Kubernetes: convert(d.Kubernetes),
		Containerd: convert(d.Containerd),
		Runc:       convert(d.Runc),
		CNI:        convert(d.CNI),
		Crictl:     convert(d.Crictl),
	}

	if out.Kubernetes == nil && out.Containerd == nil && out.Runc == nil && out.CNI == nil && out.Crictl == nil {
		return nil
	}

	return out
}
