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
	// Even when env vars and GPU are present, configImage wins.
	t.Setenv("AGENT_OCI_IMAGE", "env-image:latest")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := resolveOCIImage(discardLogger(), "config-image:v1", true, hostDistroUbuntu2404)
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

			got := resolveOCIImage(discardLogger(), "", false, hostDistroUbuntu2404)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveOCIImage_DisableDoesNotOverrideConfig(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "true")

	got := resolveOCIImage(discardLogger(), "config-image:v2", false, hostDistroUbuntu2404)
	assert.Equal(t, "config-image:v2", got)
}

func TestResolveOCIImage_EnvVarFallback(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "env-image:v3")

	got := resolveOCIImage(discardLogger(), "", false, hostDistroUbuntu2404)
	assert.Equal(t, "env-image:v3", got)
}

func TestResolveOCIImage_EnvVarTrimmed(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "  env-image:v4  ")

	got := resolveOCIImage(discardLogger(), "", false, hostDistroUbuntu2404)
	assert.Equal(t, "env-image:v4", got)
}

func TestResolveOCIImage_EnvVarWhitespaceOnly(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "   ")

	got := resolveOCIImage(discardLogger(), "", false, hostDistroUbuntu2404)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultNoGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := resolveOCIImage(discardLogger(), "", false, hostDistroUbuntu2404)
	assert.Equal(t, DefaultOCIImage, got)
}

func TestResolveOCIImage_DefaultWithGPU(t *testing.T) {
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
	t.Setenv("AGENT_OCI_IMAGE", "")

	got := resolveOCIImage(discardLogger(), "", true, hostDistroUbuntu2404)
	assert.Equal(t, DefaultNvidiaOCIImage, got)
}

func TestResolveOCIImage_Priority(t *testing.T) {
	// Verify the full priority chain: config > disable > env var > default.
	log := discardLogger()

	// 1. Config set - everything else ignored.
	t.Setenv("AGENT_OCI_IMAGE", "env")
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "1")

	assert.Equal(t, "config", resolveOCIImage(log, "config", true, hostDistroUbuntu2404))

	// 2. No config, disable set - returns empty despite env var being set.
	assert.Equal(t, "", resolveOCIImage(log, "", true, hostDistroUbuntu2404))

	// 3. No config, disable off, env var set.
	t.Setenv("AGENT_DISABLE_OCI_IMAGE", "0")

	assert.Equal(t, "env", resolveOCIImage(log, "", true, hostDistroUbuntu2404))

	// 4. No config, disable off, no env var - GPU default.
	t.Setenv("AGENT_OCI_IMAGE", "")

	assert.Equal(t, DefaultNvidiaOCIImage, resolveOCIImage(log, "", true, hostDistroUbuntu2404))
	assert.Equal(t, DefaultOCIImage, resolveOCIImage(log, "", false, hostDistroUbuntu2404))
}

func TestResolveOCIImage_DefaultForHostDistro(t *testing.T) {
	tests := []struct {
		name      string
		distro    string
		nvidiaGPU bool
		want      string
	}{
		{
			name:   "ubuntu 2404",
			distro: hostDistroUbuntu2404,
			want:   DefaultOCIImage,
		},
		{
			name:      "ubuntu 2404 nvidia",
			distro:    hostDistroUbuntu2404,
			nvidiaGPU: true,
			want:      DefaultNvidiaOCIImage,
		},
		{
			name:   "ubuntu 2604",
			distro: hostDistroUbuntu2604,
			want:   DefaultUbuntu2604OCIImage,
		},
		{
			name:      "ubuntu 2604 nvidia",
			distro:    hostDistroUbuntu2604,
			nvidiaGPU: true,
			want:      DefaultUbuntu2604NvidiaOCIImage,
		},
		{
			name:   "azure linux 3",
			distro: hostDistroAzureLinux3,
			want:   DefaultAzureLinux3OCIImage,
		},
		{
			name:      "azure linux 3 nvidia",
			distro:    hostDistroAzureLinux3,
			nvidiaGPU: true,
			want:      DefaultAzureLinux3NvidiaOCIImage,
		},
		{
			name:   "unknown falls back to ubuntu 2404",
			distro: "",
			want:   DefaultOCIImage,
		},
		{
			name:      "unknown with nvidia falls back to ubuntu 2404 nvidia",
			distro:    "",
			nvidiaGPU: true,
			want:      DefaultNvidiaOCIImage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT_DISABLE_OCI_IMAGE", "")
			t.Setenv("AGENT_OCI_IMAGE", "")

			got := resolveOCIImage(discardLogger(), "", tt.nvidiaGPU, tt.distro)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHostDistroFromOSReleaseData(t *testing.T) {
	tests := []struct {
		name      string
		osRelease string
		want      string
	}{
		{
			name: "ubuntu 2404",
			osRelease: `ID=ubuntu
VERSION_ID="24.04"`,
			want: hostDistroUbuntu2404,
		},
		{
			name: "ubuntu 2604",
			osRelease: `ID=ubuntu
VERSION_ID="26.04"`,
			want: hostDistroUbuntu2604,
		},
		{
			name: "azure linux 3",
			osRelease: `ID="azurelinux"
VERSION_ID="3.0"`,
			want: hostDistroAzureLinux3,
		},
		{
			name: "fedora falls back to azure linux 3",
			osRelease: `ID=fedora
VERSION_ID="44"
ID_LIKE="rhel fedora"`,
			want: hostDistroAzureLinux3,
		},
		{
			name: "rhel-like falls back to azure linux 3",
			osRelease: `ID="my-enterprise-linux"
VERSION_ID="9"
ID_LIKE="rhel fedora"`,
			want: hostDistroAzureLinux3,
		},
		{
			name: "unknown distro",
			osRelease: `ID=debian
VERSION_ID="13"`,
		},
		{
			name: "malformed lines ignored",
			osRelease: `# comment
ID = ubuntu
ID=ubuntu
VERSION_ID=24.04`,
			want: hostDistroUbuntu2404,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hostDistroFromOSReleaseData([]byte(tt.osRelease))
			assert.Equal(t, tt.want, got)
		})
	}
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

func TestResolveMachine_AdditionalHostDevices(t *testing.T) {
	cfg := &config.AgentConfig{
		MachineName:           "machine-1",
		NodeName:              "configured-node",
		AdditionalHostDevices: []string{"char-input", "/dev/uinput"},
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
		},
	}

	got, err := ResolveMachine(discardLogger(), cfg, "kube1", nil)
	require.NoError(t, err)

	require.Equal(t, []string{"char-input", "/dev/uinput"}, got.RootFS.HostDevices.Additional)
	require.Contains(t, got.RootFS.HostDevices.Paths(), "/dev/uinput")
	require.NotContains(t, got.RootFS.HostDevices.Paths(), "char-input")
	require.Equal(t, []string{"char-input"}, got.RootFS.HostDevices.DeviceGroupSpecifiers())
}

func TestResolveMachine_InvalidAdditionalHostDevice(t *testing.T) {
	cfg := &config.AgentConfig{
		MachineName:           "machine-1",
		NodeName:              "configured-node",
		AdditionalHostDevices: []string{"/etc/passwd"},
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2EtYnl0ZXM=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com",
		},
	}

	_, err := ResolveMachine(discardLogger(), cfg, "kube1", nil)
	require.ErrorContains(t, err, "AdditionalHostDevices")
}
