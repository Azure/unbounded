// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
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
			name: "missing machine name is allowed (resolved at runtime)",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				kubeconfigPath: kubeconfigPath,
			},
		},
		{
			name: "invalid machine name",
			handler: manualBootstrapHandler{
				siteName:       "dc1",
				machineName:    "Bad_Name",
				kubeconfigPath: kubeconfigPath,
			},
			expectErr: "invalid machine name",
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
			name: "valid: offline artifacts file URL",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "file:///opt/unbounded/artifacts/v1.31.2",
			},
		},
		{
			name: "valid: offline artifacts absolute path",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "/opt/unbounded/artifacts/v1.31.2",
			},
		},
		{
			name: "valid: offline artifacts HTTPS source",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "https://artifacts.example.com/unbounded/v1.31.2.tar.gz?sp=r&sig=test-signature",
			},
		},
		{
			name: "invalid: offline artifacts HTTPS source without archive path",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "https://artifacts.example.com",
			},
			expectErr: "HTTPS URL must include a host and archive path",
		},
		{
			name: "valid: offline artifacts OCI source",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "oci://registry.example.com/unbounded/bootstrap-artifacts:v1",
			},
		},
		{
			name: "invalid: offline artifacts OCI source without tag",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "oci://registry.example.com/unbounded/bootstrap-artifacts",
			},
			expectErr: "OCI URL must include a tag or digest",
		},
		{
			name: "invalid: offline artifacts relative path",
			handler: manualBootstrapHandler{
				siteName:               "dc1",
				machineName:            "my-node",
				kubeconfigPath:         kubeconfigPath,
				offlineArtifactsSource: "artifacts/v1.31.2",
			},
			expectErr: "source without a scheme must be an absolute path",
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

func TestValidateHTTPSArtifactsSourceRedactsInvalidQuery(t *testing.T) {
	t.Parallel()

	err := validateHTTPSArtifactsSource("https://artifacts.example.test/%zz?sig=secret")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

func TestManualBootstrapHandler_BuildAgentConfig(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	h := &manualBootstrapHandler{
		siteName:               "dc1",
		machineName:            "my-node",
		nodeLabels:             []string{"env=prod"},
		taints:                 []string{"dedicated=gpu:NoSchedule"},
		nodeIP:                 " 10.0.0.15 ",
		ociImage:               "ghcr.io/azure/rootfs:v1",
		offlineArtifactsSource: " file:///opt/unbounded/artifacts/v1.31.2 ",
		kubeCli:                kubeCli,
		kubeConfig:             &rest.Config{Host: "https://my-api-server:6443"},
		logger:                 discardLogger(),
	}

	cfg, err := h.buildAgentConfig(context.Background())
	require.NoError(t, err)

	require.Equal(t, "my-node", cfg.MachineName)
	require.Equal(t, "https://my-api-server:6443", cfg.Kubelet.ApiServer)
	require.Equal(t, "10.0.0.10", cfg.Cluster.ClusterDNS)
	require.NotEmpty(t, cfg.Cluster.CaCertBase64)
	require.NotEmpty(t, cfg.Cluster.Version) // fake client returns empty string but it's still set
	require.Contains(t, cfg.Kubelet.Auth.BootstrapToken, "abc123.")
	require.Equal(t, "10.0.0.15", cfg.Kubelet.NodeIP)
	require.Equal(t, map[string]string{"env": "prod"}, cfg.Kubelet.Labels)
	require.Equal(t, []string{"dedicated=gpu:NoSchedule"}, cfg.Kubelet.RegisterWithTaints)
	require.Equal(t, "ghcr.io/azure/rootfs:v1", cfg.OCIImage)
	require.Equal(t, "file:///opt/unbounded/artifacts/v1.31.2", cfg.OfflineArtifacts.Source)
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
				Labels: map[string]string{"env": "prod"},
			},
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
			},
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
			},
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
			},
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
				Labels: map[string]string{"env": "prod"},
			},
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

		cfgWithOCI := &provision.UnboundedAgentConfig{
			AgentConfig: provision.AgentConfig{
				MachineName: "test-node",
				Cluster: provision.AgentClusterConfig{
					CaCertBase64: "dGVzdA==",
					ClusterDNS:   "10.0.0.10",
					Version:      "v1.30.0",
				},
				Kubelet: provision.AgentKubeletConfig{
					ApiServer: "https://api-server:6443",
					Auth: provision.KubeletAuthInfo{
						BootstrapToken: "abc123.0123456789abcdef",
					},
					Labels: map[string]string{"env": "prod"},
				},
				OCIImage: "ghcr.io/azure/agent:latest",
			},
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
// empty machine name (resolved at runtime) tests
// ---------------------------------------------------------------------------

func TestManualBootstrapHandler_BuildAgentConfig_EmptyMachineName(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	h := &manualBootstrapHandler{
		siteName:   "dc1",
		kubeCli:    kubeCli,
		kubeConfig: &rest.Config{Host: "https://my-api-server:6443"},
		logger:     discardLogger(),
	}

	cfg, err := h.buildAgentConfig(context.Background())
	require.NoError(t, err)
	require.Empty(t, cfg.MachineName)

	// The agent resolves the name on the host at startup; the marshaled
	// config carries an empty MachineName.
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))
	require.Equal(t, "", parsed["MachineName"])
}

func TestManualBootstrapHandler_RenderScript_EmptyMachineName(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger: discardLogger(),
	}

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
			},
		},
	}

	script, err := h.renderScript(cfg)
	require.NoError(t, err)

	// Header indicates runtime resolution and the JSON carries an empty name.
	require.Contains(t, script, "Machine:      (resolved at runtime)")
	require.Contains(t, script, `"MachineName": ""`)
	requireValidBashSyntax(t, script)
}

func TestManualBootstrapHandler_RenderCloudInit_EmptyMachineName(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		logger: discardLogger(),
	}

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
			},
		},
	}

	output, err := h.renderCloudInit(cfg)
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(output, "#cloud-config\n"))
	require.Contains(t, output, "Machine:      (resolved at runtime)")
	require.Contains(t, output, `"MachineName": ""`)
}

func TestManualBootstrapHandler_Execute_EmptyMachineName(t *testing.T) {
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
	})

	err := cmd.ExecuteContext(context.Background())
	require.NoError(t, err)

	script := buf.String()
	require.Contains(t, script, "#!/bin/bash")
	require.Contains(t, script, "Machine:      (resolved at runtime)")
	require.Contains(t, script, `"MachineName": ""`)
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
		"--offline-artifacts-source", "file:///opt/unbounded/artifacts/v1.31.2",
		"node-1",
	})

	err := cmd.ExecuteContext(context.Background())
	require.NoError(t, err)
	require.Contains(t, buf.String(), "export AGENT_URL='file:///tmp/unbounded-agent-linux-amd64.tar.gz'")
	require.Contains(t, buf.String(), `"OfflineArtifacts": {`)
	require.Contains(t, buf.String(), `"Source": "file:///opt/unbounded/artifacts/v1.31.2"`)
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
				Labels: map[string]string{"env": "prod"},
			},
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

	cfg := &provision.UnboundedAgentConfig{
		AgentConfig: provision.AgentConfig{
			MachineName: "test-node",
			Cluster: provision.AgentClusterConfig{
				CaCertBase64: "dGVzdA==",
				ClusterDNS:   "10.0.0.10",
				Version:      "v1.30.0",
			},
			Kubelet: provision.AgentKubeletConfig{
				ApiServer: "https://api-server:6443",
				Auth: provision.KubeletAuthInfo{
					BootstrapToken: "abc123.0123456789abcdef",
				},
			},
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

// ---------------------------------------------------------------------------
// parseAdditionalHostMount() tests
// ---------------------------------------------------------------------------

func TestParseAdditionalHostMount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantMount config.AdditionalHostMount
		wantErr   string
	}{
		{
			name:      "source only",
			input:     "/opt/config",
			wantMount: config.AdditionalHostMount{Source: "/opt/config"},
		},
		{
			name:      "source and target",
			input:     "/opt/config:/etc/config",
			wantMount: config.AdditionalHostMount{Source: "/opt/config", Target: "/etc/config"},
		},
		{
			name:      "source only read-only",
			input:     "/opt/config:ro",
			wantMount: config.AdditionalHostMount{Source: "/opt/config", ReadOnly: true},
		},
		{
			name:      "source and target read-only",
			input:     "/opt/config:/etc/config:ro",
			wantMount: config.AdditionalHostMount{Source: "/opt/config", Target: "/etc/config", ReadOnly: true},
		},
		{
			name:    "empty value",
			input:   "",
			wantErr: "mount spec must not be empty",
		},
		{
			name:    "relative source",
			input:   "opt/config",
			wantErr: "invalid --additional-host-mount",
		},
		{
			name:    "unclean source",
			input:   "/opt/../config",
			wantErr: "invalid --additional-host-mount",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseAdditionalHostMount(tt.input)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantMount, got)
		})
	}
}

func TestManualBootstrapHandler_BuildAgentConfig_AdditionalHostMounts(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	h := &manualBootstrapHandler{
		siteName:    "dc1",
		machineName: "my-node",
		additionalHostMounts: []string{
			"/opt/config:ro",
			"/var/lib/data:/data",
		},
		kubeCli:    kubeCli,
		kubeConfig: &rest.Config{Host: "https://my-api-server:6443"},
		logger:     discardLogger(),
	}

	cfg, err := h.buildAgentConfig(context.Background())
	require.NoError(t, err)

	require.Equal(t, []config.AdditionalHostMount{
		{Source: "/opt/config", ReadOnly: true},
		{Source: "/var/lib/data", Target: "/data"},
	}, cfg.AdditionalHostMounts)
}

// parseAdditionalHostDevice() tests
// ---------------------------------------------------------------------------

func TestParseAdditionalHostDevice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantDevice string
		wantErr    string
	}{
		{
			name:       "absolute /dev path",
			input:      "/dev/uinput",
			wantDevice: "/dev/uinput",
		},
		{
			name:       "systemd char group specifier",
			input:      "char-input",
			wantDevice: "char-input",
		},
		{
			name:       "systemd block group wildcard",
			input:      "block-*",
			wantDevice: "block-*",
		},
		{
			name:    "empty value",
			input:   "",
			wantErr: "device spec must not be empty",
		},
		{
			name:    "relative path",
			input:   "dev/uinput",
			wantErr: "invalid --additional-host-device",
		},
		{
			name:    "path outside /dev",
			input:   "/etc/config",
			wantErr: "invalid --additional-host-device",
		},
		{
			name:    "unclean /dev path",
			input:   "/dev/../dev/uinput",
			wantErr: "invalid --additional-host-device",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseAdditionalHostDevice(tt.input)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantDevice, got)
		})
	}
}

func TestManualBootstrapHandler_BuildAgentConfig_AdditionalHostDevices(t *testing.T) {
	t.Parallel()

	kubeCli := newFakeCluster(t, "dc1")

	h := &manualBootstrapHandler{
		siteName:              "dc1",
		machineName:           "my-node",
		additionalHostDevices: []string{"/dev/uinput", "char-input"},
		kubeCli:               kubeCli,
		kubeConfig:            &rest.Config{Host: "https://my-api-server:6443"},
		logger:                discardLogger(),
	}

	cfg, err := h.buildAgentConfig(context.Background())
	require.NoError(t, err)

	require.Equal(t, []string{"/dev/uinput", "char-input"}, cfg.AdditionalHostDevices)
}

// TestAgentSpecDrivesBothConfigAndInstallEnv pins the invariant that broke
// before: --host-prefix must reach the agent config and the install script
// environment. The script places the agent binary and the config tells the
// daemon where to write its own files; a host that received only one would be
// left half-installed.
func TestAgentSpecDrivesBothConfigAndInstallEnv(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{hostPrefix: "/opt/unbounded"}

	spec := h.agentSpec()
	require.Equal(t, "/opt/unbounded", spec.HostPrefix)
	require.Contains(t, h.installEnv(), "AGENT_HOST_PREFIX='/opt/unbounded'")
}

func TestAgentSpecSystemExtension(t *testing.T) {
	t.Parallel()

	// Unset leaves the spec nil so existing hosts are unaffected.
	require.Nil(t, (&manualBootstrapHandler{}).agentSpec().SystemExtension)

	h := &manualBootstrapHandler{
		systemExtensionName:   "unbounded-nspawn",
		systemExtensionSource: "/tmp/unbounded-nspawn.raw",
	}

	ext := h.agentSpec().SystemExtension
	require.NotNil(t, ext)
	require.Equal(t, "unbounded-nspawn", ext.Name)
	require.Equal(t, "/tmp/unbounded-nspawn.raw", ext.Source)
}

// TestAgentSpecPreservesExistingFields guards the unification: the spec now
// feeds both call sites, so a field dropped here would silently disappear from
// the rendered config.
func TestAgentSpecPreservesExistingFields(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{
		ociImage:     "ghcr.io/example/rootfs:v1",
		agentVersion: "v1.2.3",
		agentBaseURL: "https://example.invalid/releases",
		agentURL:     "file:///tmp/agent.tar.gz",
		localDNS:     true,
	}

	spec := h.agentSpec()
	require.Equal(t, "ghcr.io/example/rootfs:v1", spec.Image)
	require.Equal(t, "v1.2.3", spec.Version)
	require.Equal(t, "https://example.invalid/releases", spec.BaseURL)
	require.Equal(t, "file:///tmp/agent.tar.gz", spec.URL)
	require.NotNil(t, spec.LocalDNS)
	require.True(t, spec.LocalDNS.Enabled)

	env := h.installEnv()
	require.Contains(t, env, "AGENT_VERSION='v1.2.3'")
	require.Contains(t, env, "AGENT_URL='file:///tmp/agent.tar.gz'")
}

// renderIgnitionForTest renders the ignition variant and unmarshals it, which
// the cloud-init tests do not do for their own output. The result is consumed
// by a machine, so it has to be valid JSON with the expected shape rather than
// merely containing the right substrings.
func renderIgnitionForTest(t *testing.T, h *manualBootstrapHandler, cfg *provision.UnboundedAgentConfig) map[string]any {
	t.Helper()

	out, err := h.renderIgnition(cfg)
	require.NoError(t, err)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &parsed), "ignition output must be valid JSON")

	return parsed
}

func ignitionFilesByPath(t *testing.T, parsed map[string]any) map[string]map[string]any {
	t.Helper()

	storage, ok := parsed["storage"].(map[string]any)
	require.True(t, ok, "config must have storage")

	rawFiles, ok := storage["files"].([]any)
	require.True(t, ok, "storage must have files")

	byPath := map[string]map[string]any{}

	for _, raw := range rawFiles {
		file, ok := raw.(map[string]any)
		require.True(t, ok)

		byPath[file["path"].(string)] = file
	}

	return byPath
}

func TestRenderIgnitionStructure(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{}
	cfg := &provision.UnboundedAgentConfig{}
	cfg.MachineName = "node-1"

	parsed := renderIgnitionForTest(t, h, cfg)

	ign, ok := parsed["ignition"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ignitionSpecVersion, ign["version"])

	files := ignitionFilesByPath(t, parsed)
	assert.Contains(t, files, ignitionAgentConfigPath)
	assert.Contains(t, files, ignitionInstallPath)

	// Nothing may be written under /usr/local, which is read-only on the
	// immutable hosts this variant exists for.
	for path := range files {
		assert.NotContains(t, path, "/usr/local", "ignition must not write under /usr/local")
	}

	systemd, ok := parsed["systemd"].(map[string]any)
	require.True(t, ok, "config must have systemd")

	units, ok := systemd["units"].([]any)
	require.True(t, ok)
	require.Len(t, units, 1)

	unit := units[0].(map[string]any)
	assert.Equal(t, ignitionBootstrapUnit, unit["name"])
	assert.Equal(t, true, unit["enabled"], "the unit must be enabled or nothing runs it")
	assert.Contains(t, unit["contents"], "ExecStart=/bin/bash "+ignitionInstallPath)
	assert.Contains(t, unit["contents"], "UNBOUNDED_AGENT_CONFIG_FILE="+ignitionAgentConfigPath)
}

// TestRenderIgnitionEmbedsAgentConfig verifies the config round-trips through
// the data URL rather than merely appearing somewhere in the output.
func TestRenderIgnitionEmbedsAgentConfig(t *testing.T) {
	t.Parallel()

	cfg := &provision.UnboundedAgentConfig{}
	cfg.MachineName = "node-1"
	cfg.HostPrefix = "/opt/unbounded"

	parsed := renderIgnitionForTest(t, &manualBootstrapHandler{}, cfg)
	files := ignitionFilesByPath(t, parsed)

	contents := files[ignitionAgentConfigPath]["contents"].(map[string]any)
	source := contents["source"].(string)
	require.True(t, strings.HasPrefix(source, "data:;base64,"))

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(source, "data:;base64,"))
	require.NoError(t, err)

	var round provision.UnboundedAgentConfig
	require.NoError(t, json.Unmarshal(decoded, &round))
	assert.Equal(t, "node-1", round.MachineName)
	assert.Equal(t, "/opt/unbounded", round.HostPrefix)
}

// TestRenderIgnitionPlacesFetchableExtension covers the case that makes
// bootstrap single-pass: an extension Ignition can fetch is written before
// first boot, so systemd-sysext merges it before dbus starts.
func TestRenderIgnitionPlacesFetchableExtension(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{systemExtensionSHA256: strings.Repeat("ab", 32)}
	cfg := &provision.UnboundedAgentConfig{}
	cfg.SystemExtension = &config.AgentSystemExtension{
		Name:   "unbounded-nspawn",
		Source: "https://example.invalid/unbounded-nspawn.raw",
	}

	files := ignitionFilesByPath(t, renderIgnitionForTest(t, h, cfg))

	file, ok := files["/var/lib/extensions/unbounded-nspawn.raw"]
	require.True(t, ok, "a fetchable extension must be placed before first boot")

	contents := file["contents"].(map[string]any)
	assert.Equal(t, "https://example.invalid/unbounded-nspawn.raw", contents["source"])

	verification := contents["verification"].(map[string]any)
	assert.Equal(t, "sha256-"+strings.Repeat("ab", 32), verification["hash"])
}

// TestRenderIgnitionSkipsUnfetchableExtension pins the consequence of choosing
// OCI as the distribution channel: Ignition cannot fetch oci://, so the agent
// installs it later and the host needs a reboot before the extension's D-Bus
// services are usable.
func TestRenderIgnitionSkipsUnfetchableExtension(t *testing.T) {
	t.Parallel()

	cfg := &provision.UnboundedAgentConfig{}
	cfg.SystemExtension = &config.AgentSystemExtension{
		Name:   "unbounded-nspawn",
		Source: "oci://ghcr.io/example/sysext:255-33.azl3-amd64#unbounded-nspawn.raw",
	}

	files := ignitionFilesByPath(t, renderIgnitionForTest(t, &manualBootstrapHandler{}, cfg))

	assert.NotContains(t, files, "/var/lib/extensions/unbounded-nspawn.raw",
		"ignition cannot fetch oci:// and must leave it to the agent")
	// The rest of the bootstrap is unaffected.
	assert.Contains(t, files, ignitionAgentConfigPath)
}

// TestRenderIgnitionUnquotesInstallEnv guards a mismatch between two quoting
// regimes: AgentInstallEnv single-quotes values for a shell, but systemd parses
// Environment= itself and would treat those quotes as part of the value.
func TestRenderIgnitionUnquotesInstallEnv(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{hostPrefix: "/opt/unbounded", agentVersion: "v1.2.3"}

	parsed := renderIgnitionForTest(t, h, &provision.UnboundedAgentConfig{})
	units := parsed["systemd"].(map[string]any)["units"].([]any)
	contents := units[0].(map[string]any)["contents"].(string)

	assert.Contains(t, contents, "Environment=AGENT_HOST_PREFIX=/opt/unbounded")
	assert.Contains(t, contents, "Environment=AGENT_VERSION=v1.2.3")
	assert.NotContains(t, contents, "'", "shell quoting must not leak into a systemd unit")
}

func TestRenderIgnitionRejectsBadDigest(t *testing.T) {
	t.Parallel()

	h := &manualBootstrapHandler{systemExtensionSHA256: "not-a-digest"}
	cfg := &provision.UnboundedAgentConfig{}
	cfg.SystemExtension = &config.AgentSystemExtension{
		Name:   "unbounded-nspawn",
		Source: "https://example.invalid/unbounded-nspawn.raw",
	}

	_, err := h.renderIgnition(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "system-extension-sha256")
}

func TestParseBootstrapVariantIgnition(t *testing.T) {
	t.Parallel()

	got, err := parseBootstrapVariant("ignition")
	require.NoError(t, err)
	assert.Equal(t, variantIgnition, got)

	_, err = parseBootstrapVariant("nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ignition", "the error should list the valid variants")
}
