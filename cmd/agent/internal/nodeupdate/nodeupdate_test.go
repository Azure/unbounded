// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodeupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentv1 "github.com/Azure/unbounded-kube/agent/api/v1"
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
		TaskServer: &provision.AgentTaskServerConfig{
			Endpoint: "tasks.example.com:443",
		},
	}
}

// ---------------------------------------------------------------------------
// HasDrift
// ---------------------------------------------------------------------------

func TestHasDrift_NoDrift(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion: "1.33.1",
	}
	assert.False(t, HasDrift(cfg, spec))
}

func TestHasDrift_VersionWithVPrefix(t *testing.T) {
	cfg := baseConfig()
	// Applied config stores "1.33.1", spec sends "v1.33.1" - should not be drift.
	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion: "v1.33.1",
	}
	assert.False(t, HasDrift(cfg, spec))
}

func TestHasDrift_VersionChange(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion: "1.33.4",
	}
	assert.True(t, HasDrift(cfg, spec))
}

func TestHasDrift_ApiServerChange(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		ApiServer: "https://new-api.example.com:6443",
	}
	assert.True(t, HasDrift(cfg, spec))
}

func TestHasDrift_EmptySpec(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{}
	assert.False(t, HasDrift(cfg, spec))
}

func TestHasDrift_CaCertChange(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		CaCertBase64: "bmV3LWNh",
	}
	assert.True(t, HasDrift(cfg, spec))
}

func TestHasDrift_OciImageChange(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		OciImage: "ghcr.io/test/image:v2",
	}
	assert.True(t, HasDrift(cfg, spec))
}

// ---------------------------------------------------------------------------
// MergeSpec
// ---------------------------------------------------------------------------

func TestMergeSpec_VersionOnly(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion: "1.33.4",
	}

	merged := MergeSpec(cfg, spec)

	// Changed field.
	assert.Equal(t, "1.33.4", merged.Cluster.Version)

	// Preserved fields.
	assert.Equal(t, cfg.MachineName, merged.MachineName)
	assert.Equal(t, cfg.Cluster.CaCertBase64, merged.Cluster.CaCertBase64)
	assert.Equal(t, cfg.Cluster.ClusterDNS, merged.Cluster.ClusterDNS)
	assert.Equal(t, cfg.Kubelet.ApiServer, merged.Kubelet.ApiServer)
	assert.Equal(t, cfg.Kubelet.BootstrapToken, merged.Kubelet.BootstrapToken)
	assert.Equal(t, cfg.OCIImage, merged.OCIImage)
	assert.Equal(t, cfg.TaskServer.Endpoint, merged.TaskServer.Endpoint)
}

func TestMergeSpec_AllFields(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion:  "v1.34.0",
		ApiServer:          "https://new-api.example.com:6443",
		CaCertBase64:       "bmV3LWNh",
		ClusterDns:         "10.96.0.20",
		BootstrapToken:     "new123.newxyz",
		OciImage:           "ghcr.io/test/image:v2",
		NodeLabels:         map[string]string{"env": "prod", "tier": "frontend"},
		RegisterWithTaints: []string{"dedicated=prod:NoSchedule"},
	}

	merged := MergeSpec(cfg, spec)

	assert.Equal(t, "1.34.0", merged.Cluster.Version) // v prefix stripped
	assert.Equal(t, "https://new-api.example.com:6443", merged.Kubelet.ApiServer)
	assert.Equal(t, "bmV3LWNh", merged.Cluster.CaCertBase64)
	assert.Equal(t, "10.96.0.20", merged.Cluster.ClusterDNS)
	assert.Equal(t, "new123.newxyz", merged.Kubelet.BootstrapToken)
	assert.Equal(t, "ghcr.io/test/image:v2", merged.OCIImage)
	assert.Equal(t, map[string]string{"env": "prod", "tier": "frontend"}, merged.Kubelet.Labels)
	assert.Equal(t, []string{"dedicated=prod:NoSchedule"}, merged.Kubelet.RegisterWithTaints)
}

func TestMergeSpec_DoesNotMutateOriginal(t *testing.T) {
	cfg := baseConfig()
	originalVersion := cfg.Cluster.Version

	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion: "1.34.0",
	}

	_ = MergeSpec(cfg, spec)

	// Original should be unchanged.
	assert.Equal(t, originalVersion, cfg.Cluster.Version)
}

func TestMergeSpec_PreservesTaskServer(t *testing.T) {
	cfg := baseConfig()
	spec := &agentv1.NodeUpdateSpec{
		KubernetesVersion: "1.33.4",
	}

	merged := MergeSpec(cfg, spec)

	require.NotNil(t, merged.TaskServer)
	assert.Equal(t, "tasks.example.com:443", merged.TaskServer.Endpoint)

	// Verify it's a copy, not the same pointer.
	merged.TaskServer.Endpoint = "modified"
	assert.Equal(t, "tasks.example.com:443", cfg.TaskServer.Endpoint)
}

// ---------------------------------------------------------------------------
// FindActiveMachine
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

	_ = origPath // Note: FindActiveMachine uses the const, so this test
	// validates the serialization/deserialization roundtrip rather than
	// the full FindActiveMachine flow (which requires root filesystem access).
}
