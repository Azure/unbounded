// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/provision"
)

// ---------------------------------------------------------------------------
// validate() tests
// ---------------------------------------------------------------------------

func TestManualBootstrapHandler_Validate(t *testing.T) {
	t.Parallel()

	kubeconfigPath := writeTempKubeconfig(t)

	tests := []struct {
		name      string
		handler   manualBootstrapHandler
		expectErr string
	}{
		{
			name: "valid: required fields",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
			},
		},
		{
			name: "valid: with labels",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
				nodeLabels:     []string{"env=prod", "tier=frontend"},
			},
		},
		{
			name: "missing site name",
			handler: manualBootstrapHandler{
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
			},
			expectErr: "site name is required",
		},
		{
			name: "missing machine name",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				kubeconfigPath: kubeconfigPath,
			},
			expectErr: "machine name is required",
		},
		{
			name: "kubeconfig not readable",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: "/nonexistent/kubeconfig",
			},
			expectErr: "is not readable",
		},
		{
			name: "invalid node label",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
				nodeLabels:     []string{"bad-label"},
			},
			expectErr: "invalid --node-label",
		},
		{
			name: "invalid variant",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
				variant:        "unknown",
			},
			expectErr: "unknown variant",
		},
		{
			name: "valid: cloud-init variant",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
				variant:        "cloud-init",
			},
		},
		{
			name: "valid: variant defaults to script when empty",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "my-node",
				kubeconfigPath: kubeconfigPath,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.handler.validate()

			if tt.expectErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.expectErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildAgentConfig() tests
// ---------------------------------------------------------------------------

// newFakeCluster creates a fake kubernetes clientset pre-seeded with the
// resources that buildAgentConfig needs: a bootstrap token, the
// kube-root-ca.crt ConfigMap, and the kube-dns Service.
func newFakeCluster(t *testing.T, siteName string) *fake.Clientset {
	t.Helper()

	return fake.NewClientset(
		newBootstrapTokenSecret(siteName),
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-root-ca.crt",
				Namespace: metav1.NamespacePublic,
			},
			Data: map[string]string{
				"ca.crt": "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n",
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-dns",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "10.0.0.10",
			},
		},
	)
}

func TestManualBootstrapHandler_BuildAgentConfig(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	h := &manualBootstrapHandler{
		siteName:    "dc1",
		machineName: "my-node",
		nodeLabels:  []string{"env=prod"},
		taints:      []string{"dedicated=gpu:NoSchedule"},
		ociImage:    "ghcr.io/azure/rootfs:v1",
		kubeCli:     kubeCli,
		kubeConfig:  &rest.Config{Host: "https://my-api-server:6443"},
		logger:      discardLogger(),
	}

	cfg, err := h.buildAgentConfig(context.Background())
	require.NoError(t, err)

	require.Equal(t, "my-node", cfg.MachineName)
	require.Equal(t, "https://my-api-server:6443", cfg.Kubelet.ApiServer)
	require.Equal(t, "10.0.0.10", cfg.Cluster.ClusterDNS)
	require.NotEmpty(t, cfg.Cluster.CaCertBase64)
	require.NotEmpty(t, cfg.Cluster.Version) // fake client returns empty string but it's still set
	require.Contains(t, cfg.Kubelet.BootstrapToken, "abc123.")
	require.Equal(t, map[string]string{"env": "prod"}, cfg.Kubelet.Labels)
	require.Equal(t, []string{"dedicated=gpu:NoSchedule"}, cfg.Kubelet.RegisterWithTaints)
	require.Equal(t, "ghcr.io/azure/rootfs:v1", cfg.OCIImage)
}

func TestManualBootstrapHandler_BuildAgentConfig_KubernetesVersionOverride(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	h := &manualBootstrapHandler{
		siteName:          "dc1",
		machineName:       "my-node",
		kubernetesVersion: "v1.31.2",
		kubeCli:           kubeCli,
		kubeConfig:        &rest.Config{Host: "https://my-api-server:6443"},
		logger:            discardLogger(),
	}

	cfg, err := h.buildAgentConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v1.31.2", cfg.Cluster.Version)
}

func TestManualBootstrapHandler_BuildAgentConfig_NoBootstrapToken(t *testing.T) {
	t.Parallel()

	// No bootstrap token secret seeded.
	kubeCli := fake.NewClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-root-ca.crt",
				Namespace: metav1.NamespacePublic,
			},
			Data: map[string]string{
				"ca.crt": "FAKE",
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kube-dns",
				Namespace: metav1.NamespaceSystem,
			},
			Spec: corev1.ServiceSpec{
				ClusterIP: "10.0.0.10",
			},
		},
	)

	h := &manualBootstrapHandler{
		siteName:    "dc1",
		machineName: "my-node",
		kubeCli:     kubeCli,
		kubeConfig:  &rest.Config{Host: "https://my-api-server:6443"},
		logger:      discardLogger(),
	}

	_, err := h.buildAgentConfig(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "bootstrap token")
}

// ---------------------------------------------------------------------------
// renderScript() tests
// ---------------------------------------------------------------------------

func TestManualBootstrapHandler_RenderScript(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger: discardLogger(),
	}

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
			Labels:         map[string]string{"env": "prod"},
		},
	}

	script, err := h.renderScript(cfg)
	require.NoError(t, err)

	// No download overrides: no `export AGENT_*=` lines should appear in
	// the outer script (the embedded install script references the vars
	// in its own help text, but must not be pre-set by the wrapper).
	require.NotContains(t, script, "export AGENT_VERSION=")
	require.NotContains(t, script, "export AGENT_URL=")
	require.NotContains(t, script, "export AGENT_BASE_URL=")

	// Should start with a shebang.
	require.Contains(t, script, "#!/bin/bash")
	require.Contains(t, script, "set -eo pipefail")

	// Should contain the machine name in the header.
	require.Contains(t, script, "test-node")

	// Should contain the JSON config inline.
	require.Contains(t, script, `"MachineName": "test-node"`)
	require.Contains(t, script, `"ApiServer": "https://api-server:6443"`)
	require.Contains(t, script, `"BootstrapToken": "abc123.0123456789abcdef"`)

	// Should write config to a temp file.
	require.Contains(t, script, "UNBOUNDED_AGENT_CONFIG_FILE=")
	require.Contains(t, script, "AGENT_CONFIG_EOF")

	// Should contain the install script parts (architecture detection, download).
	require.Contains(t, script, "uname -m")
	require.Contains(t, script, "unbounded-agent")

	// Verify the embedded JSON is valid by extracting it from between the
	// heredoc markers. The config is written as:
	//   cat > ... <<'AGENT_CONFIG_EOF'
	//   { ... }
	//   AGENT_CONFIG_EOF
	scriptBytes := []byte(script)
	marker := []byte("AGENT_CONFIG_EOF")
	firstMarker := bytes.Index(scriptBytes, marker)
	require.Greater(t, firstMarker, 0, "first AGENT_CONFIG_EOF marker not found")

	// JSON starts on the line after the first marker.
	jsonStart := firstMarker + len(marker) + 1 // +1 for the newline
	secondMarker := bytes.Index(scriptBytes[jsonStart:], marker)
	require.Greater(t, secondMarker, 0, "second AGENT_CONFIG_EOF marker not found")

	jsonData := scriptBytes[jsonStart : jsonStart+secondMarker-1] // -1 to strip trailing newline

	var parsed provision.AgentConfig
	require.NoError(t, json.Unmarshal(jsonData, &parsed))
	require.Equal(t, "test-node", parsed.MachineName)
	require.Equal(t, "https://api-server:6443", parsed.Kubelet.ApiServer)

	// The uninstall script is no longer embedded in the bootstrap script;
	// reset is handled by `unbounded-agent reset`.
	require.NotContains(t, script, "UNINSTALL_SCRIPT_EOF")
	require.NotContains(t, script, "unbounded-agent-uninstall.sh")
}

func TestManualBootstrapHandler_RenderScript_WithAgentURL(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger: discardLogger(),
	}

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
		},
	}

	h.agentURL = "file:///tmp/unbounded-agent-linux-amd64.tar.gz"

	script, err := h.renderScript(cfg)
	require.NoError(t, err)

	agentURLExport := "export AGENT_URL='file:///tmp/unbounded-agent-linux-amd64.tar.gz'"
	installScript := "bash <<'INSTALL_SCRIPT_EOF'"

	require.Contains(t, script, agentURLExport)
	require.Contains(t, script, installScript)
	require.Less(t, strings.Index(script, agentURLExport), strings.Index(script, installScript))
}

func TestManualBootstrapHandler_RenderScript_WithUnsafeAgentURL(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger:   discardLogger(),
		agentURL: `https://example.test/download?name="agent"&cmd=$(touch /tmp/pwned)`,
	}

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
		},
	}

	script, err := h.renderScript(cfg)
	require.NoError(t, err)
	require.Contains(t, script, `export AGENT_URL='https://example.test/download?name="agent"&cmd=$(touch /tmp/pwned)'`)
	require.NotContains(t, script, `export AGENT_URL="https://example.test/download?name="agent"&cmd=$(touch /tmp/pwned)"`)

	installScript := "bash <<'INSTALL_SCRIPT_EOF'"
	require.Less(t, strings.Index(script, "export AGENT_URL='https://example.test/download?name=\"agent\"&cmd=$(touch /tmp/pwned)'"), strings.Index(script, installScript))
}

func TestManualBootstrapHandler_RenderScript_WithoutAgentURL(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger: discardLogger(),
	}

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
		},
	}

	script, err := h.renderScript(cfg)
	require.NoError(t, err)
	require.NotContains(t, script, "export AGENT_URL=")
}

func TestManualBootstrapHandler_RenderCloudInit(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger: discardLogger(),
	}

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
			Labels:         map[string]string{"env": "prod"},
		},
	}

	t.Run("basic", func(t *testing.T) {
		t.Parallel()

		output, err := h.renderCloudInit(cfg)
		require.NoError(t, err)

		// Must start with the cloud-config header.
		require.True(t, strings.HasPrefix(output, "#cloud-config\n"), "output must start with #cloud-config header")

		// Must contain the machine name in the comment header.
		require.Contains(t, output, "test-node")

		// Must write the agent config file.
		require.Contains(t, output, "/etc/unbounded/agent/config.json")
		require.Contains(t, output, `"MachineName": "test-node"`)
		require.Contains(t, output, `"ApiServer": "https://api-server:6443"`)

		// Must write the install script.
		require.Contains(t, output, "/usr/local/bin/unbounded-agent-install.sh")
		require.Contains(t, output, "unbounded-agent")

		// runcmd must set UNBOUNDED_AGENT_CONFIG_FILE and run the install script.
		require.Contains(t, output, "UNBOUNDED_AGENT_CONFIG_FILE=/etc/unbounded/agent/config.json")
		require.Contains(t, output, "bash /usr/local/bin/unbounded-agent-install.sh")

		// AGENT_OCI_IMAGE env var should not be present (OCI image is in the JSON config).
		require.NotContains(t, output, "AGENT_OCI_IMAGE")

		// The uninstall script is no longer embedded in cloud-init;
		// reset is handled by `unbounded-agent reset`.
		require.NotContains(t, output, "unbounded-agent-uninstall.sh")
	})

	t.Run("with OCI image", func(t *testing.T) {
		t.Parallel()

		cfgWithOCI := &provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer:      "https://api-server:6443",
				BootstrapToken: "abc123.0123456789abcdef",
				Labels:         map[string]string{"env": "prod"},
			},
			OCIImage: "ghcr.io/azure/agent:latest",
		}

		withOCI := &manualBootstrapHandler{
			logger: discardLogger(),
		}

		output, err := withOCI.renderCloudInit(cfgWithOCI)
		require.NoError(t, err)

		// OCIImage should appear in the JSON config, not as a separate env var.
		require.Contains(t, output, `"OCIImage": "ghcr.io/azure/agent:latest"`)
		require.NotContains(t, output, "AGENT_OCI_IMAGE")
	})
}

// ---------------------------------------------------------------------------
// execute() integration test
// ---------------------------------------------------------------------------

func TestManualBootstrapHandler_Execute_WithAgentURL(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	var buf bytes.Buffer

	kubeconfigPath := writeTempKubeconfig(t)

	h := &manualBootstrapHandler{
		out:            &buf,
		kubeCli:        kubeCli,
		kubeConfig:     &rest.Config{Host: "https://my-api-server:6443"},
		kubeconfigPath: kubeconfigPath,
		logger:         discardLogger(),
	}

	cmd := newMachineManualBootstrapCommand(h)
	cmd.SetArgs([]string{
		"--site", "dc1",
		"--kubeconfig", kubeconfigPath,
		"--agent-url", "file:///tmp/unbounded-agent-linux-amd64.tar.gz",
		"node-1",
	})

	err := cmd.ExecuteContext(context.Background())
	require.NoError(t, err)
	require.Contains(t, buf.String(), "export AGENT_URL='file:///tmp/unbounded-agent-linux-amd64.tar.gz'")
}

func TestManualBootstrapHandler_Execute(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	t.Run("script variant", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		h := &manualBootstrapHandler{
			siteName:       "dc1",
			machineName:    "my-node",
			nodeLabels:     []string{"env=prod"},
			out:            &buf,
			kubeCli:        kubeCli,
			kubeConfig:     &rest.Config{Host: "https://my-api-server:6443"},
			kubeconfigPath: writeTempKubeconfig(t),
			logger:         discardLogger(),
		}

		err := h.execute(context.Background())
		require.NoError(t, err)

		script := buf.String()
		require.Contains(t, script, "#!/bin/bash")
		require.Contains(t, script, "my-node")
		require.Contains(t, script, "abc123.")
		require.Contains(t, script, "unbounded-agent")
	})

	t.Run("cloud-init variant", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer

		h := &manualBootstrapHandler{
			siteName:       "dc1",
			machineName:    "my-node",
			variant:        "cloud-init",
			out:            &buf,
			kubeCli:        kubeCli,
			kubeConfig:     &rest.Config{Host: "https://my-api-server:6443"},
			kubeconfigPath: writeTempKubeconfig(t),
			logger:         discardLogger(),
		}

		err := h.execute(context.Background())
		require.NoError(t, err)

		output := buf.String()
		require.True(t, strings.HasPrefix(output, "#cloud-config\n"))
		require.Contains(t, output, "my-node")
		require.Contains(t, output, "abc123.")
		require.Contains(t, output, "/etc/unbounded/agent/config.json")
	})
}

// ---------------------------------------------------------------------------
// Agent download override tests
// ---------------------------------------------------------------------------

func TestManualBootstrapHandler_InstallEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler manualBootstrapHandler
		want    []string
	}{
		{
			name:    "no overrides",
			handler: manualBootstrapHandler{},
			want:    nil,
		},
		{
			name:    "pinned version",
			handler: manualBootstrapHandler{agentVersion: "v0.0.10"},
			want:    []string{"AGENT_VERSION='v0.0.10'"},
		},
		{
			name:    "base URL override",
			handler: manualBootstrapHandler{agentBaseURL: "https://mirror.example.com/releases"},
			want:    []string{"AGENT_BASE_URL='https://mirror.example.com/releases'"},
		},
		{
			name:    "full URL override",
			handler: manualBootstrapHandler{agentURL: "https://mirror.example.com/agent.tar.gz"},
			want:    []string{"AGENT_URL='https://mirror.example.com/agent.tar.gz'"},
		},
		{
			name: "all three set",
			handler: manualBootstrapHandler{
				agentVersion: "v0.0.10",
				agentBaseURL: "https://mirror.example.com/releases",
				agentURL:     "https://mirror.example.com/agent.tar.gz",
			},
			want: []string{
				"AGENT_VERSION='v0.0.10'",
				"AGENT_BASE_URL='https://mirror.example.com/releases'",
				"AGENT_URL='https://mirror.example.com/agent.tar.gz'",
			},
		},
		{
			name:    "value containing a single quote is escaped",
			handler: manualBootstrapHandler{agentVersion: "v'1"},
			want:    []string{`AGENT_VERSION='v'\''1'`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.handler.installEnv()
			require.Equal(t, tt.want, got)
		})
	}
}

func TestManualBootstrapHandler_RenderScript_DownloadOverrides(t *testing.T) {
	t.Parallel()

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
			Labels:         map[string]string{"env": "prod"},
		},
	}

	h := &manualBootstrapHandler{
		logger:       discardLogger(),
		agentVersion: "v0.0.10",
		agentBaseURL: "https://mirror.example.com/releases",
	}

	script, err := h.renderScript(cfg)
	require.NoError(t, err)

	// Overrides must be exported in the outer shell before the embedded
	// install script heredoc.
	require.Contains(t, script, "export AGENT_VERSION='v0.0.10'")
	require.Contains(t, script, "export AGENT_BASE_URL='https://mirror.example.com/releases'")

	// The exports must appear before the embedded install script heredoc.
	exportIdx := strings.Index(script, "export AGENT_VERSION=")
	heredocIdx := strings.Index(script, "INSTALL_SCRIPT_EOF")

	require.Greater(t, exportIdx, 0)
	require.Greater(t, heredocIdx, exportIdx)

	// Script should still be valid bash syntax.
	requireValidBashSyntax(t, script)
}

func TestManualBootstrapHandler_RenderCloudInit_DownloadOverrides(t *testing.T) {
	t.Parallel()

	cfg := &provision.AgentConfig{
		MachineName: "test-node",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdA==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.30.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api-server:6443",
			BootstrapToken: "abc123.0123456789abcdef",
		},
	}

	h := &manualBootstrapHandler{
		logger:   discardLogger(),
		agentURL: "https://mirror.example.com/agent.tar.gz",
	}

	output, err := h.renderCloudInit(cfg)
	require.NoError(t, err)

	// The override must be exported in runcmd before invoking the install
	// script.
	require.Contains(t, output, "export AGENT_URL='https://mirror.example.com/agent.tar.gz'")

	exportIdx := strings.Index(output, "export AGENT_URL=")
	runIdx := strings.Index(output, "bash /usr/local/bin/unbounded-agent-install.sh")

	require.Greater(t, exportIdx, 0)
	require.Greater(t, runIdx, exportIdx)
}

// requireValidBashSyntax shells out to `bash -n` to syntax-check a script.
// It skips the test if bash is not available in the test environment.
func requireValidBashSyntax(t *testing.T, script string) {
	t.Helper()

	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not found in PATH: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "bootstrap-*.sh")
	require.NoError(t, err)

	_, err = f.WriteString(script)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	cmd := exec.Command(bashPath, "-n", f.Name())

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bash -n failed: %s", string(out))
}
