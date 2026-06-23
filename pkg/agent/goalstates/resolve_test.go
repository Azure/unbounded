// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"log/slog"
	"os"
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
	t.Setenv("AGENT_OCI_IMAGE", "env-image:latest")

	got := ResolveOCIImage(discardLogger(), "config-image:v1", true)
	assert.Equal(t, "config-image:v1", got)
}

func TestResolveOCIImage_EnvVarFallback(t *testing.T) {
	t.Setenv("AGENT_OCI_IMAGE", "env-image:v3")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v3", got)
}

func TestResolveOCIImage_EnvVarTrimmed(t *testing.T) {
	t.Setenv("AGENT_OCI_IMAGE", "  env-image:v4  ")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, "env-image:v4", got)
}

func TestResolveOCIImage_EnvVarWhitespaceOnly(t *testing.T) {
	t.Setenv("AGENT_OCI_IMAGE", "   ")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultNoGPU(t *testing.T) {
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := ResolveOCIImage(discardLogger(), "", false)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultWithGPU(t *testing.T) {
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := ResolveOCIImage(discardLogger(), "", true)
	assert.Equal(t, DefaultNvidiaOCImage, got)
}

func TestResolveOCIImage_Priority(t *testing.T) {
	// Verify the full priority chain: config > env var > default.
	log := discardLogger()

	// 1. Config set - everything else ignored.
	t.Setenv("AGENT_OCI_IMAGE", "env")

	assert.Equal(t, "config", ResolveOCIImage(log, "config", true))

	// 2. No config, env var set.
	assert.Equal(t, "env", ResolveOCIImage(log, "", true))

	// 3. No config, no env var - GPU default.
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

func TestResolveKubelet_NodeIP(t *testing.T) {
	cfg := &config.AgentConfig{
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
			NodeIP:    "10.0.0.15,fd00::15",
		},
	}

	k, err := resolveKubelet(cfg)
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.15,fd00::15", k.NodeIP)
}

func TestResolveKubelet_InvalidNodeIPRejected(t *testing.T) {
	cfg := &config.AgentConfig{
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
			NodeIP:    "not-an-ip",
		},
	}

	_, err := resolveKubelet(cfg)
	assert.ErrorContains(t, err, "invalid Kubelet.NodeIP")
}

func TestResolveMachine_UsesConfigNodeName(t *testing.T) {
	cfg := &config.AgentConfig{
		MachineName: "machine-1",
		NodeName:    "configured-node",
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
		},
	}

	got, err := ResolveMachine(discardLogger(), cfg, "kube1", nil)
	require.NoError(t, err)
	hostname, err := os.Hostname()
	require.NoError(t, err)

	assert.Equal(t, "configured-node", cfg.NodeName)
	assert.Equal(t, hostname, got.RootFS.Hostname)
	assert.Equal(t, "configured-node", got.NodeStart.NodeName)
	assert.Equal(t, "kube1", got.NodeStart.MachineName)
	assert.Equal(t, "machine-1", got.NodeStart.KubeMachineName)
}
