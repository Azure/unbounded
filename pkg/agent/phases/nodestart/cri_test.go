// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureContainerdWritesGantryHostsConfig(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	path := filepath.Join(machineDir, goalstates.ContainerdDefaultHostsPath)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, gantryHostsConfig, string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestConfigureContainerdNVIDIARuntimeDropIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		enabled    bool
		lastStatus *goalstates.GPUStatus
		want       bool
	}{
		{name: "CPU node", enabled: false},
		{name: "GPU node", enabled: true, want: true},
		{
			name:       "GPU node whose GPUs worked at the last start",
			enabled:    true,
			lastStatus: &goalstates.GPUStatus{Ready: true, Reason: goalstates.GPUReasonSetupSucceeded},
			want:       true,
		},
		{
			// The post-start hook removed the drop-in and started containerd
			// without it; writing it back would not reach the running containerd.
			name:       "GPU node whose GPU setup failed at the last start",
			enabled:    true,
			lastStatus: &goalstates.GPUStatus{Reason: goalstates.GPUReasonSetupFailed},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machineDir := t.TempDir()
			dropIn := filepath.Join(machineDir, goalstates.NvidiaRuntimeDropInPath)

			// Start from an existing drop-in to show it is removed when it should be.
			require.NoError(t, os.MkdirAll(filepath.Dir(dropIn), 0o755))
			require.NoError(t, os.WriteFile(dropIn, []byte("stale"), 0o644))

			if tt.lastStatus != nil {
				require.NoError(t, goalstates.WriteGPUStatus(machineDir, *tt.lastStatus))
			}

			goalState := &goalstates.NodeStart{
				MachineDir: machineDir,
				Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: tt.enabled}),
			}
			require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

			if !tt.want {
				require.NoFileExists(t, dropIn)
				return
			}

			data, err := os.ReadFile(dropIn)
			require.NoError(t, err)
			require.Contains(t, string(data), `default_runtime_name = "nvidia"`)
			require.Contains(t, string(data), `BinaryName = "/usr/bin/nvidia-container-runtime"`)
		})
	}
}

func TestConfigureContainerdSetsImagePullTuning(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	path := filepath.Join(machineDir, goalstates.ContainerdConfigPath)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "[plugins.\"io.containerd.grpc.v1.cri\"]\n"+
		"sandbox_image = \""+goalState.Containerd.SandboxImage+"\"\n"+
		"image_pull_progress_timeout = \"30m\"")
	require.Contains(t, string(data), "[plugins.\"io.containerd.transfer.v1.local\"]\n"+
		"max_concurrent_downloads = 6")
}

func TestConfigureContainerdDoesNotInstallLifecycleReadinessGate(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	service, err := os.ReadFile(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd))
	require.NoError(t, err)
	require.NotContains(t, string(service), "unbounded-nvidia-ready")
}

func TestConfigureContainerdUpdatesManagedGantryHostsConfig(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	path := filepath.Join(machineDir, goalstates.ContainerdDefaultHostsPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(gantryHostsManagedMarker+"\nold = true\n"), 0o600))

	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, gantryHostsConfig, string(data))
}

func TestConfigureContainerdRefusesUnmanagedGantryHostsConfig(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	path := filepath.Join(machineDir, goalstates.ContainerdDefaultHostsPath)
	original := []byte("# managed by someone else\n")

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, original, 0o644))

	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	err := ConfigureContainerd(goalState).Do(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to overwrite unmanaged containerd hosts file")

	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, original, data)
}

func TestConfigureContainerdSkipsGantryHostsConfigWhenDisabled(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	path := filepath.Join(machineDir, goalstates.ContainerdDefaultHostsPath)
	original := []byte("# managed by someone else\n")

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, original, 0o644))

	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
		Gantry:     goalstates.Gantry{Disabled: true},
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, data)
}

func TestConfigureContainerdLeavesPerRegistryHostsConfig(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	path := filepath.Join(machineDir, goalstates.ContainerdCertsDir, "ghcr.io", "hosts.toml")
	original := []byte("server = \"https://ghcr.io\"\n")

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, original, 0o644))

	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, data)
}
