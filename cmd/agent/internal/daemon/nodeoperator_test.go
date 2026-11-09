// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
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

func writeAppliedFixture(t *testing.T, dir, name string, cfg *provision.AgentConfig) {
	t.Helper()

	data, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+"-applied-config.json"), data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+"-applied-config.json.sha256"), []byte(goalstates.ComputeChecksum(data)+"\n"), 0o600))
}

func appliedIn(dir string) func(string) string {
	return func(name string) string { return filepath.Join(dir, name+"-applied-config.json") }
}

// A none node records its applied config under a name older agents never read,
// and the agent finds it there with the slot it belongs to.
func TestFindActiveMachine_FindsANoneNodeUnderItsOwnName(t *testing.T) {
	dir := t.TempDir()

	cfg := baseConfig()
	cfg.Isolation = "none"
	writeAppliedFixture(t, dir, "none-kube1", cfg)

	active, err := findActiveMachineIn(discardLogger(), appliedIn(dir))
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeNone, active.Mode)
	assert.Equal(t, "kube1", active.Name)

	_, err = os.Stat(filepath.Join(dir, "kube1-applied-config.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestFindActiveMachine_PrefersTheNSpawnSlots(t *testing.T) {
	dir := t.TempDir()
	writeAppliedFixture(t, dir, "kube2", baseConfig())

	active, err := findActiveMachineIn(discardLogger(), appliedIn(dir))
	require.NoError(t, err)
	assert.Equal(t, isolation.ModeNSpawn, active.Mode)
	assert.Equal(t, "kube2", active.Name)
}

// The file name and the recorded mode must agree: a none config in an nspawn
// slot (or the reverse) fails closed.
func TestFindActiveMachine_RefusesAModeStoredUnderAnotherModesName(t *testing.T) {
	dir := t.TempDir()

	cfg := baseConfig()
	cfg.Isolation = "none"
	writeAppliedFixture(t, dir, "kube1", cfg)

	_, err := findActiveMachineIn(discardLogger(), appliedIn(dir))
	require.ErrorContains(t, err, `records isolation mode "none"`)

	dir = t.TempDir()
	writeAppliedFixture(t, dir, "none-kube1", baseConfig())

	_, err = findActiveMachineIn(discardLogger(), appliedIn(dir))
	require.ErrorContains(t, err, `stored as the "none" config`)
}

func TestFindActiveMachine_NoAppliedConfig(t *testing.T) {
	_, err := findActiveMachineIn(discardLogger(), appliedIn(t.TempDir()))
	require.ErrorContains(t, err, "no applied config found")
}

func TestFindActiveMachine_BadChecksumFails(t *testing.T) {
	dir := t.TempDir()

	cfg := baseConfig()
	cfg.Isolation = "none"
	writeAppliedFixture(t, dir, "none-kube1", cfg)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "none-kube1-applied-config.json.sha256"), []byte("deadbeef\n"), 0o600))

	_, err := findActiveMachineIn(discardLogger(), appliedIn(dir))
	require.ErrorContains(t, err, "checksum")
}
