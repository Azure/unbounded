// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nspawn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestFindActiveMachineInDir_Kube1(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	writeAppliedConfig(t, dir, goalstates.NSpawnMachineKube1, cfg, true)

	active, err := findActiveMachine(context.Background(), discardLogger(), appliedConfigPathInDir(dir), checksumPathInDir(dir), dir)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, goalstates.NSpawnMachineKube1, active.Name)
	assert.Equal(t, cfg.MachineName, active.Config.MachineName)
	assert.Equal(t, cfg.Cluster.Version, active.Config.Cluster.Version)
}

func TestFindActiveMachineInDir_MissingChecksumSidecar(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	writeAppliedConfig(t, dir, goalstates.NSpawnMachineKube2, cfg, false)

	active, err := findActiveMachine(context.Background(), discardLogger(), appliedConfigPathInDir(dir), checksumPathInDir(dir), dir)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, goalstates.NSpawnMachineKube2, active.Name)
}

func TestFindActiveMachineInDir_NoActiveMachine(t *testing.T) {
	dir := t.TempDir()

	active, err := findActiveMachine(context.Background(), discardLogger(), appliedConfigPathInDir(dir), checksumPathInDir(dir), dir)
	require.Error(t, err)
	assert.Nil(t, active)
	assert.True(t, errors.Is(err, ErrNoActiveMachine))
}

func TestFindActiveMachineInDir_MultipleActiveMachines(t *testing.T) {
	dir := t.TempDir()
	writeAppliedConfig(t, dir, goalstates.NSpawnMachineKube1, testConfig(), true)
	writeAppliedConfig(t, dir, goalstates.NSpawnMachineKube2, testConfig(), true)

	active, err := findActiveMachine(context.Background(), discardLogger(), appliedConfigPathInDir(dir), checksumPathInDir(dir), dir)
	require.Error(t, err)
	assert.Nil(t, active)
	assert.True(t, errors.Is(err, ErrMultipleActiveMachines))
}

func TestFindActiveMachineInDir_ChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	writeAppliedConfig(t, dir, goalstates.NSpawnMachineKube1, testConfig(), true)
	require.NoError(t, os.WriteFile(checksumPathInDir(dir)(goalstates.NSpawnMachineKube1), []byte("bad\n"), 0o600))

	active, err := findActiveMachine(context.Background(), discardLogger(), appliedConfigPathInDir(dir), checksumPathInDir(dir), dir)
	require.Error(t, err)
	assert.Nil(t, active)
	assert.True(t, errors.Is(err, goalstates.ErrChecksumMismatch))
}

func TestFindActiveMachineInDir_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	active, err := findActiveMachine(ctx, discardLogger(), appliedConfigPathInDir(dir), checksumPathInDir(dir), dir)
	require.Error(t, err)
	assert.Nil(t, active)
	assert.True(t, errors.Is(err, context.Canceled))
}

func testConfig() *config.AgentConfig {
	return &config.AgentConfig{
		MachineName: "test-machine",
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.96.0.10",
			Version:      "v1.33.1",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: "https://api.example.com:6443",
			Auth: config.KubeletAuthInfo{
				BootstrapToken: "abc123.xyz789",
			},
		},
		OCIImage: "ghcr.io/test/image:v1",
	}
}

func writeAppliedConfig(t *testing.T, dir, machineName string, cfg *config.AgentConfig, writeChecksum bool) {
	t.Helper()

	data, err := json.MarshalIndent(cfg, "", "  ")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(appliedConfigPathInDir(dir)(machineName), data, 0o600))
	if writeChecksum {
		checksum := goalstates.ComputeChecksum(data)
		require.NoError(t, os.WriteFile(checksumPathInDir(dir)(machineName), []byte(checksum+"\n"), 0o600))
	}
}

func appliedConfigPathInDir(dir string) func(string) string {
	return func(machineName string) string {
		return filepath.Join(dir, machineName+"-applied-config.json")
	}
}

func checksumPathInDir(dir string) func(string) string {
	return func(machineName string) string {
		return appliedConfigPathInDir(dir)(machineName) + ".sha256"
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}
