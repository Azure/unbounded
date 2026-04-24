// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/unbounded/internal/provision"
)

const configFileEnv = "UNBOUNDED_AGENT_CONFIG_FILE"

func requiredEnv(n string) (string, error) {
	v := strings.TrimSpace(os.Getenv(n))
	if v == "" {
		return "", fmt.Errorf("env var %q is required", n)
	}

	return v, nil
}

// loadConfig returns an AgentConfig populated from the JSON file pointed to by
// UNBOUNDED_AGENT_CONFIG_FILE.  When that variable is unset the function falls
// back to the legacy per-field environment variables so that existing
// deployments keep working without changes.
func loadConfig() (*provision.UnboundedAgentConfig, error) {
	var (
		cfg *provision.UnboundedAgentConfig
		err error
	)

	if path := strings.TrimSpace(os.Getenv(configFileEnv)); path != "" {
		cfg, err = loadConfigFromFile(path)
	} else {
		cfg, err = loadConfigFromEnv()
	}

	if err != nil {
		return nil, err
	}

	normalizeConfig(cfg)

	return cfg, nil
}

// normalizeConfig applies common fixups regardless of how the config was loaded.
func normalizeConfig(cfg *provision.UnboundedAgentConfig) {
	cfg.Cluster.Version = strings.TrimPrefix(cfg.Cluster.Version, "v")

	// FIXME: should we set the scheme in machina side?
	if !strings.HasPrefix(cfg.Kubelet.ApiServer, "https://") {
		cfg.Kubelet.ApiServer = "https://" + cfg.Kubelet.ApiServer
	}
}

// loadConfigFromFile reads and decodes the JSON config file at path.
func loadConfigFromFile(path string) (*provision.UnboundedAgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent config file %q: %w", path, err)
	}

	var cfg provision.UnboundedAgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode agent config file %q: %w", path, err)
	}

	return &cfg, nil
}

// loadConfigFromEnv builds an AgentConfig from the legacy ad-hoc environment
// variables.  This preserves backward compatibility for callers that have not
// yet switched to the JSON config file.
func loadConfigFromEnv() (*provision.UnboundedAgentConfig, error) {
	machineName, err := requiredEnv("MACHINA_MACHINE_NAME")
	if err != nil {
		return nil, err
	}

	kubeVersion, err := requiredEnv("KUBE_VERSION")
	if err != nil {
		return nil, err
	}

	apiServer, err := requiredEnv("API_SERVER")
	if err != nil {
		return nil, err
	}

	caCertB64, err := requiredEnv("CA_CERT_BASE64")
	if err != nil {
		return nil, err
	}

	clusterDNS, err := requiredEnv("CLUSTER_DNS")
	if err != nil {
		return nil, err
	}

	// AGENT_ATTEST_URL is optional; when set it enables TPM attestation and
	// BOOTSTRAP_TOKEN is not required.
	attestURL := strings.TrimSpace(os.Getenv("AGENT_ATTEST_URL"))

	var bootstrapToken string
	if attestURL == "" {
		bootstrapToken, err = requiredEnv("BOOTSTRAP_TOKEN")
		if err != nil {
			return nil, err
		}
	} else {
		bootstrapToken = strings.TrimSpace(os.Getenv("BOOTSTRAP_TOKEN"))
	}

	// NODE_LABELS is optional; parse "key1=val1,key2=val2" format.
	labels := make(map[string]string)

	if raw := strings.TrimSpace(os.Getenv("NODE_LABELS")); raw != "" {
		for _, pair := range strings.Split(raw, ",") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				return nil, fmt.Errorf("invalid NODE_LABELS entry %q", pair)
			}

			labels[k] = v
		}
	}

	// REGISTER_WITH_TAINTS is optional; parse "key=val:effect,..." format.
	var taints []string
	if raw := strings.TrimSpace(os.Getenv("REGISTER_WITH_TAINTS")); raw != "" {
		taints = strings.Split(raw, ",")
	}

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: machineName,
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: caCertB64,
				ClusterDNS:   clusterDNS,
				Version:      kubeVersion,
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer:          apiServer,
				BootstrapToken:     bootstrapToken,
				Labels:             labels,
				RegisterWithTaints: taints,
			},
		},
	}

	if attestURL != "" {
		cfg.Attest = &provision.AgentAttestConfig{
			URL: attestURL,
		}
	}

	return cfg, nil
}
