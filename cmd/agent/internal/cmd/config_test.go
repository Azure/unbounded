package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/project-unbounded/unbounded-kube/internal/provision"
)

func writeConfigFile(t *testing.T, cfg provision.AgentConfig) string {
	t.Helper()

	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "agent-config.json")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	return path
}

func sampleConfig() provision.AgentConfig {
	return provision.AgentConfig{
		MachineName: "test-machine",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.34.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "api.example.com:443",
			BootstrapToken: "abc123.secret456",
			Labels: map[string]string{
				"env": "test",
			},
			RegisterWithTaints: []string{
				"dedicated=gpu:NoSchedule",
				"workload=ml:PreferNoSchedule",
			},
		},
	}
}

func TestLoadConfig_FromFile(t *testing.T) {
	cfg := sampleConfig()
	path := writeConfigFile(t, cfg)

	t.Setenv(configFileEnv, path)

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Equal(t, "test-machine", got.MachineName)
	assert.Equal(t, "dGVzdC1jYQ==", got.Cluster.CaCertBase64)
	assert.Equal(t, "10.0.0.10", got.Cluster.ClusterDNS)
	// Version should have the leading "v" stripped.
	assert.Equal(t, "1.34.0", got.Cluster.Version)
	assert.Equal(t, "https://api.example.com:443", got.Kubelet.ApiServer)
	assert.Equal(t, "abc123.secret456", got.Kubelet.BootstrapToken)
	assert.Equal(t, map[string]string{"env": "test"}, got.Kubelet.Labels)
	assert.Equal(t, []string{"dedicated=gpu:NoSchedule", "workload=ml:PreferNoSchedule"}, got.Kubelet.RegisterWithTaints)
}

func TestLoadConfig_FromFile_VersionWithoutPrefix(t *testing.T) {
	cfg := sampleConfig()
	cfg.Cluster.Version = "1.33.1" // no "v" prefix
	path := writeConfigFile(t, cfg)

	t.Setenv(configFileEnv, path)

	got, err := loadConfig()
	require.NoError(t, err)
	assert.Equal(t, "1.33.1", got.Cluster.Version)
}

func TestLoadConfig_FromFile_MissingFile(t *testing.T) {
	t.Setenv(configFileEnv, "/tmp/does-not-exist-"+t.Name()+".json")

	_, err := loadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read agent config file")
}

func TestLoadConfig_FromFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0o644))

	t.Setenv(configFileEnv, path)

	_, err := loadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode agent config file")
}

func TestLoadConfig_FromEnv(t *testing.T) {
	// Ensure UNBOUNDED_AGENT_CONFIG_FILE is not set so we fall back to env vars.
	t.Setenv(configFileEnv, "")

	t.Setenv("MACHINA_MACHINE_NAME", "env-machine")
	t.Setenv("KUBE_VERSION", "1.33.0")
	t.Setenv("API_SERVER", "api.env.example.com:443")
	t.Setenv("BOOTSTRAP_TOKEN", "envtok.envsec")
	t.Setenv("CA_CERT_BASE64", "ZW52LWNh")
	t.Setenv("CLUSTER_DNS", "10.96.0.10")
	t.Setenv("NODE_LABELS", "env=staging,team=infra")
	t.Setenv("REGISTER_WITH_TAINTS", "dedicated=gpu:NoSchedule,workload=ml:PreferNoSchedule")
	t.Setenv("AGENT_ATTEST_URL", "")

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Equal(t, "env-machine", got.MachineName)
	assert.Equal(t, "ZW52LWNh", got.Cluster.CaCertBase64)
	assert.Equal(t, "10.96.0.10", got.Cluster.ClusterDNS)
	assert.Equal(t, "1.33.0", got.Cluster.Version)
	assert.Equal(t, "https://api.env.example.com:443", got.Kubelet.ApiServer)
	assert.Equal(t, "envtok.envsec", got.Kubelet.BootstrapToken)
	assert.Equal(t, map[string]string{"env": "staging", "team": "infra"}, got.Kubelet.Labels)
	assert.Equal(t, []string{"dedicated=gpu:NoSchedule", "workload=ml:PreferNoSchedule"}, got.Kubelet.RegisterWithTaints)
	assert.Nil(t, got.Attest)
}

func TestLoadConfig_FromEnv_NoLabels(t *testing.T) {
	t.Setenv(configFileEnv, "")

	t.Setenv("MACHINA_MACHINE_NAME", "no-label-machine")
	t.Setenv("KUBE_VERSION", "1.33.0")
	t.Setenv("API_SERVER", "api.example.com:443")
	t.Setenv("BOOTSTRAP_TOKEN", "tok.sec")
	t.Setenv("CA_CERT_BASE64", "Y2E=")
	t.Setenv("CLUSTER_DNS", "10.0.0.10")
	t.Setenv("NODE_LABELS", "")
	t.Setenv("REGISTER_WITH_TAINTS", "")

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Empty(t, got.Kubelet.Labels)
	assert.Empty(t, got.Kubelet.RegisterWithTaints)
}

func TestLoadConfig_FromEnv_MissingRequired(t *testing.T) {
	t.Setenv(configFileEnv, "")

	// Set all required vars except MACHINA_MACHINE_NAME.
	t.Setenv("KUBE_VERSION", "1.33.0")
	t.Setenv("API_SERVER", "api.example.com:443")
	t.Setenv("BOOTSTRAP_TOKEN", "tok.sec")
	t.Setenv("CA_CERT_BASE64", "Y2E=")
	t.Setenv("CLUSTER_DNS", "10.0.0.10")
	t.Setenv("MACHINA_MACHINE_NAME", "")

	_, err := loadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MACHINA_MACHINE_NAME")
}

func TestLoadConfig_FromEnv_InvalidLabels(t *testing.T) {
	t.Setenv(configFileEnv, "")

	t.Setenv("MACHINA_MACHINE_NAME", "machine")
	t.Setenv("KUBE_VERSION", "1.33.0")
	t.Setenv("API_SERVER", "api.example.com:443")
	t.Setenv("BOOTSTRAP_TOKEN", "tok.sec")
	t.Setenv("CA_CERT_BASE64", "Y2E=")
	t.Setenv("CLUSTER_DNS", "10.0.0.10")
	t.Setenv("NODE_LABELS", "bad-label-no-equals")

	_, err := loadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid NODE_LABELS entry")
}

func TestLoadConfig_FilePreferredOverEnv(t *testing.T) {
	// When both the config file and env vars are set, the file takes precedence.
	cfg := sampleConfig()
	path := writeConfigFile(t, cfg)

	t.Setenv(configFileEnv, path)
	// Set env vars with different values to confirm they're ignored.
	t.Setenv("MACHINA_MACHINE_NAME", "env-machine-ignored")
	t.Setenv("API_SERVER", "env-api-ignored")

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Equal(t, "test-machine", got.MachineName)
	assert.Equal(t, "https://api.example.com:443", got.Kubelet.ApiServer)
}

func TestLoadConfig_FromFile_NoTaints(t *testing.T) {
	cfg := sampleConfig()
	cfg.Kubelet.RegisterWithTaints = nil
	path := writeConfigFile(t, cfg)

	t.Setenv(configFileEnv, path)

	got, err := loadConfig()
	require.NoError(t, err)
	assert.Empty(t, got.Kubelet.RegisterWithTaints)
}

func TestLoadConfig_FromEnv_WithTaintsSingle(t *testing.T) {
	t.Setenv(configFileEnv, "")

	t.Setenv("MACHINA_MACHINE_NAME", "taint-machine")
	t.Setenv("KUBE_VERSION", "1.33.0")
	t.Setenv("API_SERVER", "api.example.com:443")
	t.Setenv("BOOTSTRAP_TOKEN", "tok.sec")
	t.Setenv("CA_CERT_BASE64", "Y2E=")
	t.Setenv("CLUSTER_DNS", "10.0.0.10")
	t.Setenv("NODE_LABELS", "")
	t.Setenv("REGISTER_WITH_TAINTS", "dedicated=gpu:NoSchedule")
	t.Setenv("AGENT_ATTEST_URL", "")

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Equal(t, []string{"dedicated=gpu:NoSchedule"}, got.Kubelet.RegisterWithTaints)
}

func TestLoadConfig_FromEnv_WithAttestURL(t *testing.T) {
	t.Setenv(configFileEnv, "")

	t.Setenv("MACHINA_MACHINE_NAME", "metal-node")
	t.Setenv("KUBE_VERSION", "1.34.0")
	t.Setenv("API_SERVER", "api.example.com:443")
	t.Setenv("CA_CERT_BASE64", "Y2E=")
	t.Setenv("CLUSTER_DNS", "10.0.0.10")
	t.Setenv("AGENT_ATTEST_URL", "http://10.0.0.1:8880")
	// BOOTSTRAP_TOKEN is not set — attestation replaces it.
	t.Setenv("BOOTSTRAP_TOKEN", "")

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Equal(t, "metal-node", got.MachineName)
	assert.NotNil(t, got.Attest)
	assert.Equal(t, "http://10.0.0.1:8880", got.Attest.URL)
	assert.Empty(t, got.Kubelet.BootstrapToken)
}

func TestLoadConfig_FromEnv_NoTokenAndNoAttest(t *testing.T) {
	// Without AGENT_ATTEST_URL, BOOTSTRAP_TOKEN is required.
	t.Setenv(configFileEnv, "")

	t.Setenv("MACHINA_MACHINE_NAME", "machine")
	t.Setenv("KUBE_VERSION", "1.33.0")
	t.Setenv("API_SERVER", "api.example.com:443")
	t.Setenv("CA_CERT_BASE64", "Y2E=")
	t.Setenv("CLUSTER_DNS", "10.0.0.10")
	t.Setenv("BOOTSTRAP_TOKEN", "")
	t.Setenv("AGENT_ATTEST_URL", "")

	_, err := loadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BOOTSTRAP_TOKEN")
}

func TestLoadConfig_FromFile_WithAttest(t *testing.T) {
	cfg := provision.AgentConfig{
		MachineName: "metal-node-file",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			Version:      "v1.34.0",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer: "api.example.com:443",
		},
		Attest: &provision.AgentAttestConfig{
			URL: "http://192.168.1.1:8880",
		},
	}
	path := writeConfigFile(t, cfg)

	t.Setenv(configFileEnv, path)

	got, err := loadConfig()
	require.NoError(t, err)

	assert.Equal(t, "metal-node-file", got.MachineName)
	assert.NotNil(t, got.Attest)
	assert.Equal(t, "http://192.168.1.1:8880", got.Attest.URL)
	assert.Empty(t, got.Kubelet.BootstrapToken)
}
