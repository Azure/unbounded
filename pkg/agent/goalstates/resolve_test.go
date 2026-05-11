// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// discardLogger returns a logger that silently drops all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestResolveOCIImage_ConfigImageTakesPrecedence(t *testing.T) {
	// Even when env vars and GPU are present, configImage wins.
	t.Setenv("AGENT_OCI_IMAGE", "env-image:latest")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := ResolveOCIImage(discardLogger(), "config-image:v1", true)
	assert.Equal(t, "config-image:v1", got)
}

func TestResolveOCIImage_DisableEnvVar(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"true", "true", ""},
		{"1", "1", ""},
		{"TRUE", "TRUE", ""},
		// Falsy or unrecognised values should NOT disable; expect the default image.
		{"false", "false", DefaultOCIImage},
		{"0", "0", DefaultOCIImage},
		{"empty", "", DefaultOCIImage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_DISABLE_OCI_IMAGE", tt.value)
			t.Setenv("AGENT_OCI_IMAGE", "")

			got := ResolveOCIImage(discardLogger(), "", false)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveOCIImage_DisableDoesNotOverrideConfig(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := ResolveOCIImage(discardLogger(), "config-image:v2", false)
	assert.Equal(t, "config-image:v2", got)
}

func TestResolveOCIImage_EnvVarFallback(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "env-image:v3")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v3", got)
}

func TestResolveOCIImage_EnvVarTrimmed(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "  env-image:v4  ")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v4", got)
}

func TestResolveOCIImage_EnvVarWhitespaceOnly(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "   ")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultNoGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultWithGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := ResolveOCIImage(discardLogger(), "", true)
	assert.Equal(t, DefaultNvidiaOCImage, got)
}

func TestResolveOCIImage_Priority(t *testing.T) {
	// Verify the full priority chain: config > disable > env var > default.
	log := discardLogger()

	// 1. Config set - everything else ignored.
	t.Setenv("AGENT_OCI_IMAGE", "env")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "1")

	assert.Equal(t, "config", ResolveOCIImage(log, "config", true))

	// 2. No config, disable set - returns empty despite env var being set.
	assert.Equal(t, "", ResolveOCIImage(log, "", true))

	// 3. No config, disable off, env var set.
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "0")

	assert.Equal(t, "env", ResolveOCIImage(log, "", true))

	// 4. No config, disable off, no env var - GPU default.
	t.Setenv("AGENT_OCI_IMAGE", "")

	assert.Equal(t, DefaultNvidiaOCImage, ResolveOCIImage(log, "", true))
	assert.Equal(t, DefaultOCIImage, ResolveOCIImage(log, "", false))
}

// TestResolveKubelet_EmptyAuthAllowed verifies that resolveKubelet does
// not reject a config with an empty Kubelet.Auth. In the metalman
// PXE/attestation flow the bootstrap token is fetched from the metalman
// attest server by a later phase, so the agent must accept an empty Auth
// at config-load/resolution time.
func TestResolveKubelet_EmptyAuthAllowed(t *testing.T) {
	cfg := &config.AgentConfig{
		Cluster: config.AgentClusterConfig{
			// base64("ca-bytes") so the decode step succeeds.
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
		},
	}

	k, err := resolveKubelet(cfg)
	assert.NoError(t, err)
	assert.Empty(t, k.BootstrapToken)
	assert.Nil(t, k.ExecCredential)
}

// TestResolveKubelet_BootstrapTokenAccepted verifies that a populated
// BootstrapToken passes resolution.
func TestResolveKubelet_BootstrapTokenAccepted(t *testing.T) {
	cfg := &config.AgentConfig{
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
			Auth: config.KubeletAuthInfo{
				BootstrapToken: "abc123.secret456",
			},
		},
	}

	k, err := resolveKubelet(cfg)
	assert.NoError(t, err)
	assert.Equal(t, "abc123.secret456", k.BootstrapToken)
}

// TestResolveKubelet_BothAuthMethodsRejected verifies that genuine
// misconfigurations (both auth methods set) are still caught.
func TestResolveKubelet_BothAuthMethodsRejected(t *testing.T) {
	cfg := &config.AgentConfig{
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
			Auth: config.KubeletAuthInfo{
				BootstrapToken: "abc123.secret456",
				ExecCredential: &clientcmdapi.ExecConfig{
					Command: "/usr/local/bin/auth-plugin",
				},
			},
		},
	}

	_, err := resolveKubelet(cfg)
	assert.ErrorContains(t, err, "mutually exclusive")
}

func TestResolveNodeName(t *testing.T) {
	tests := []struct {
		name           string
		configNodeName string
		hostname       string
		machineName    string
		want           string
		wantErr        string
	}{
		{
			name:           "config override",
			configNodeName: "configured-node",
			hostname:       "worker-1",
			machineName:    "machine-1",
			want:           "configured-node",
		},
		{
			name:           "trimmed config override",
			configNodeName: " configured-node ",
			hostname:       "worker-1",
			machineName:    "machine-1",
			want:           "configured-node",
		},
		{
			name:           "invalid config override errors",
			configNodeName: "Configured_Node",
			hostname:       "worker-1",
			machineName:    "machine-1",
			wantErr:        "node name override",
		},
		{
			name:        "hostname",
			hostname:    "worker-1",
			machineName: "machine-1",
			want:        "worker-1",
		},
		{
			name:        "trimmed hostname",
			hostname:    " worker-1 ",
			machineName: "machine-1",
			want:        "worker-1",
		},
		{
			name:        "empty hostname falls back",
			hostname:    "",
			machineName: "machine-1",
			want:        "machine-1",
		},
		{
			name:        "invalid hostname falls back",
			hostname:    "WORKER_1",
			machineName: "machine-1",
			want:        "machine-1",
		},
		{
			name:        "invalid fallback errors",
			hostname:    "WORKER_1",
			machineName: "Machine_1",
			wantErr:     "not a valid Kubernetes node name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveNodeName(tt.configNodeName, tt.hostname, tt.machineName)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveNodeIdentity_ConfigNodeNameOverride(t *testing.T) {
	cfg := &config.AgentConfig{
		MachineName: "machine-1",
		NodeName:    "configured-node",
	}

	got, err := ResolveNodeIdentity(cfg, "kube1")
	require.NoError(t, err)

	assert.Equal(t, "kube1", got.MachineName)
	assert.Equal(t, "machine-1", got.KubeMachineName)
	assert.Equal(t, "configured-node", got.NodeName)
}
