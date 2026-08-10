// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestLoadAppliedConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "applied-config.json")
	checksumPath := configPath + ".sha256"
	want := provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"}
	data, err := json.Marshal(&want)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, data, 0o600))
	require.NoError(t, os.WriteFile(checksumPath, []byte(goalstates.ComputeChecksum(data)+"\n"), 0o600))

	got, ok, err := loadAppliedConfig(testLogger(), configPath, checksumPath)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, &want, got)
}

func TestLoadAppliedConfigMissingAndCorrupt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	got, ok, err := loadAppliedConfig(testLogger(), filepath.Join(dir, "missing"), filepath.Join(dir, "missing.sha256"))
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, got)

	configPath := filepath.Join(dir, "applied.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"MachineName":"machine-1"}`), 0o600))
	require.NoError(t, os.WriteFile(configPath+".sha256", []byte("invalid"), 0o600))
	_, _, err = loadAppliedConfig(testLogger(), configPath, configPath+".sha256")
	require.ErrorIs(t, err, goalstates.ErrChecksumMismatch)
}

func TestNSpawnLifecyclePreStartRefreshesConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath, checksumPath := writeAppliedConfig(t, dir)
	root := temporaryRootFS(dir)
	root.Nvidia = completeNVIDIA("fresh")

	var reloaded bool

	err := nspawnLifecyclePreStart(
		context.Background(), testLogger(), "kube1", configPath, checksumPath,
		func(_ *provision.AgentConfig, _ string) (*goalstates.RootFS, error) { return root, nil },
		phases.ExecuteTask,
		func(context.Context, *slog.Logger) error { reloaded = true; return nil },
	)
	require.NoError(t, err)
	require.True(t, reloaded)

	nspawnData, err := os.ReadFile(root.NSpawnConfigFile)
	require.NoError(t, err)
	require.Contains(t, string(nspawnData), root.Nvidia.LibDirMounts[0].HostDir)

	containerdUnit, err := os.ReadFile(filepath.Join(root.MachineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd))
	require.NoError(t, err)
	require.Contains(t, string(containerdUnit), goalstates.SystemdUnitNVIDIAReady)
}

func TestNSpawnLifecyclePreStartKeepsFreshBootstrapConfigWithoutAppliedConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	called := false
	err := nspawnLifecyclePreStart(
		context.Background(), testLogger(), "kube1",
		filepath.Join(dir, "missing.json"), filepath.Join(dir, "missing.sha256"),
		func(*provision.AgentConfig, string) (*goalstates.RootFS, error) {
			called = true

			return nil, errors.New("unexpected resolve")
		},
		func(context.Context, *slog.Logger, phases.Task) error { return errors.New("unexpected execute") },
		func(context.Context, *slog.Logger) error { return errors.New("unexpected reload") },
	)
	require.NoError(t, err)
	require.False(t, called)
}

func TestNSpawnLifecyclePreStartCPUNodeStaysCPU(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath, checksumPath := writeAppliedConfig(t, dir)
	root := temporaryRootFS(dir)
	err := nspawnLifecyclePreStart(
		context.Background(), testLogger(), "kube1", configPath, checksumPath,
		func(_ *provision.AgentConfig, _ string) (*goalstates.RootFS, error) { return root, nil },
		phases.ExecuteTask,
		func(context.Context, *slog.Logger) error { return nil },
	)
	require.NoError(t, err)

	containerdUnit, err := os.ReadFile(filepath.Join(root.MachineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd))
	require.NoError(t, err)
	require.Contains(t, string(containerdUnit), goalstates.SystemdUnitNVIDIAReady)

	_, err = os.Stat(filepath.Join(root.MachineDir, goalstates.NvidiaRuntimeDropInPath))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func writeAppliedConfig(t *testing.T, dir string) (string, string) {
	t.Helper()

	path := filepath.Join(dir, "applied.json")
	data, err := json.Marshal(&provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	checksumPath := path + ".sha256"
	require.NoError(t, os.WriteFile(checksumPath, []byte(goalstates.ComputeChecksum(data)), 0o600))

	return path, checksumPath
}

func completeNVIDIA(suffix string) goalstates.NvidiaHost {
	return goalstates.NvidiaHost{
		Required:        true,
		GPUDevicePaths:  []string{"/dev/nvidia0"},
		ContainerLibDir: "/usr/lib/x86_64-linux-gnu",
		LibMappings: []goalstates.NvidiaLibMapping{{
			HostPath:      "/host/" + suffix + "/libcuda.so.1",
			ContainerPath: "/run/host-nvidia/0/libcuda.so.1",
			LinkPath:      "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		}},
		LibDirMounts:  []goalstates.NvidiaLibDirMount{{HostDir: "/host/" + suffix, ContainerDir: "/run/host-nvidia/0"}},
		DriverVersion: "580.1-" + suffix,
	}
}

func temporaryRootFS(dir string) *goalstates.RootFS {
	return &goalstates.RootFS{
		MachineDir:             filepath.Join(dir, "kube1"),
		NSpawnConfigFile:       filepath.Join(dir, "kube1.nspawn"),
		ServiceOverrideFile:    filepath.Join(dir, "override.conf"),
		ConfigRegenerationFile: filepath.Join(dir, "regeneration.service"),
	}
}
