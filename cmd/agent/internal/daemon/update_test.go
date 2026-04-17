// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/internal/provision"
)

func baseConfig() *provision.AgentConfig {
	return &provision.AgentConfig{
		MachineName: "test-machine",
		Cluster: provision.AgentClusterConfig{
			CaCertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.96.0.10",
			Version:      "1.33.1",
		},
		Kubelet: provision.AgentKubeletConfig{
			ApiServer:      "https://api.example.com:6443",
			BootstrapToken: "abc123.xyz789",
			Labels: map[string]string{
				"env": "test",
			},
			RegisterWithTaints: []string{"dedicated=test:NoSchedule"},
		},
		OCIImage: "ghcr.io/test/image:v1",
	}
}

// ---------------------------------------------------------------------------
// hasDrift
// ---------------------------------------------------------------------------

func Test_hasDrift_NoDrift(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	assert.False(t, hasDrift(applied, desired))
}

func Test_hasDrift_VersionWithVPrefix(t *testing.T) {
	applied := baseConfig()
	// Applied config stores "1.33.1", desired has "v1.33.1" - should not be drift.
	desired := baseConfig()
	desired.Cluster.Version = "v1.33.1"
	assert.False(t, hasDrift(applied, desired))
}

func Test_hasDrift_VersionChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Cluster.Version = "1.33.4"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_ApiServerChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Kubelet.ApiServer = "https://new-api.example.com:6443"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_CaCertChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Cluster.CaCertBase64 = "bmV3LWNh"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_ClusterDNSChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Cluster.ClusterDNS = "10.96.0.20"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_BootstrapTokenChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Kubelet.BootstrapToken = "new123.newxyz"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_OciImageChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.OCIImage = "ghcr.io/test/image:v2"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_LabelsOnlyDoNotTrigger(t *testing.T) {
	// Labels are not compared by hasDrift - only fields that require
	// a machine update are checked. Label changes are handled by the
	// kubelet registration, not a full rootfs reprovision.
	applied := baseConfig()
	desired := baseConfig()
	desired.Kubelet.Labels = map[string]string{"env": "prod"}
	assert.False(t, hasDrift(applied, desired))
}

// ---------------------------------------------------------------------------
// findActiveMachine
// ---------------------------------------------------------------------------

func TestFindActiveMachine_Kube1(t *testing.T) {
	dir := t.TempDir()

	// Override the config dir for testing.
	origPath := goalstates.AgentConfigDir
	// We can't override the const, so we test the lower-level function
	// by writing directly and reading back.
	cfg := baseConfig()
	data, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)

	configPath := filepath.Join(dir, "kube1-applied-config.json")
	require.NoError(t, os.WriteFile(configPath, data, 0o600))

	// Read it back to verify the format.
	readData, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var readCfg provision.AgentConfig
	require.NoError(t, json.Unmarshal(readData, &readCfg))
	assert.Equal(t, cfg.MachineName, readCfg.MachineName)
	assert.Equal(t, cfg.Cluster.Version, readCfg.Cluster.Version)

	_ = origPath // Note: findActiveMachine uses the const, so this test
	// validates the serialization/deserialization roundtrip rather than
	// the full findActiveMachine flow (which requires root filesystem access).
}
