// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
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
			ApiServer: "https://api.example.com:6443",
			Auth: provision.KubeletAuthInfo{
				BootstrapToken: "abc123.xyz789",
			},
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
	desired.Kubelet.Auth.BootstrapToken = "new123.newxyz"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_OciImageChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.OCIImage = "ghcr.io/test/image:v2"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_LabelsChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Kubelet.Labels = map[string]string{"env": "prod"}
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_TaintsChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Kubelet.RegisterWithTaints = []string{"dedicated=prod:NoSchedule"}
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_KubeletConfigurationChange(t *testing.T) {
	applied := baseConfig()
	applied.Kubelet.Configuration = map[string]any{"maxPods": float64(110)}
	desired := applied.DeepCopy()
	desired.Kubelet.Configuration["maxPods"] = float64(250)
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_ImageCredentialProviderChange(t *testing.T) {
	applied := baseConfig()
	applied.Kubelet.ImageCredentialProvider = &provision.ImageCredentialProvider{
		ConfigPath: "/etc/kubernetes/credential-provider.yaml",
		BinDir:     "/usr/local/lib/kubelet-credential-providers",
	}
	desired := applied.DeepCopy()
	desired.Kubelet.ImageCredentialProvider.BinDir = "/opt/credential-providers"
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_GantryChange(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Gantry = &provision.GantryConfig{Disabled: true}
	assert.True(t, hasDrift(applied, desired))
}

func Test_hasDrift_GantryExplicitFalseDoesNotDrift(t *testing.T) {
	applied := baseConfig()
	applied.Gantry = &provision.GantryConfig{}
	desired := baseConfig()
	assert.False(t, hasDrift(applied, desired))
}

// ---------------------------------------------------------------------------
// Active machine config serialization
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

	_ = origPath // Note: nspawnNodeOperator uses the const, so this test
	// validates the serialization/deserialization roundtrip rather than
	// the full active-machine discovery flow (which requires root filesystem access).
}

// ---------------------------------------------------------------------------
// Isolation mode of the active machine
// ---------------------------------------------------------------------------

func Test_activeMode(t *testing.T) {
	tests := []struct {
		name       string
		isolation  string
		unregister bool
		want       isolation.Mode
		wantErr    string
	}{
		{name: "legacy config without the field is nspawn", want: isolation.ModeNSpawn},
		{name: "explicit nspawn", isolation: "nspawn", want: isolation.ModeNSpawn},
		{name: "unknown mode is rejected", isolation: "microvm", wantErr: "unsupported isolation mode"},
		// Every shipped mode has a registered backend now, so the unregistered
		// path is exercised by making the lookup report the mode under test as
		// unregistered (see registeredBackend), which does not depend on which
		// modes ship. The agent must refuse rather than operate on it as nspawn.
		{name: "known mode without a backend is rejected", isolation: "container", unregister: true, wantErr: "no registered backend"},
		{name: "none is accepted once its backend is registered", isolation: "none", want: isolation.ModeNone},
		{name: "container is accepted once its backend is registered", isolation: "container", want: isolation.ModeContainer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Isolation = tt.isolation

			if tt.unregister {
				mode := isolation.Mode(tt.isolation)

				require.NoError(t, isolation.Registered(mode), "the mode under test must be registered to begin with")

				original := registeredBackend
				registeredBackend = func(m isolation.Mode) error {
					if m == mode {
						return fmt.Errorf("isolation mode %q: no registered backend", m)
					}

					return original(m)
				}

				t.Cleanup(func() { registeredBackend = original })
			}

			got, err := activeMode(cfg)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_hasDrift_IsolationChangeTriggersRepave(t *testing.T) {
	applied := baseConfig()
	desired := baseConfig()
	desired.Isolation = "container"
	// A different declared mode is drift: the repave makes the change.
	assert.True(t, hasDrift(applied, desired))
}
