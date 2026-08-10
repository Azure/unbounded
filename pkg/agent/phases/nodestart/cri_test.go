// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

func TestConfigureContainerdSetsObservabilityDefaults(t *testing.T) {
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
	require.Contains(t, string(data), "[debug]\nlevel = \"info\"")
	require.Contains(t, string(data), "[plugins.\"io.containerd.grpc.v1.cri\"]\n"+
		"sandbox_image = \""+goalState.Containerd.SandboxImage+"\"\n"+
		"image_pull_progress_timeout = \"15m\"")
}

func TestConfigureContainerdGatesNVIDIAStartup(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
		Nvidia: goalstates.NvidiaHost{
			Required:       true,
			GPUDevicePaths: []string{"/dev/nvidia0"},
			LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/usr/lib/libcuda.so.1"}},
		},
	}
	goalState.Containerd.NvidiaRuntime.Enabled = true

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	service, err := os.ReadFile(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd))
	require.NoError(t, err)
	require.Contains(t, string(service), "Requires=unbounded-nvidia-ready.service")
	require.Contains(t, string(service), "After=unbounded-nvidia-ready.service")

	readyService, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.SystemdSystemDir,
		goalstates.SystemdUnitNVIDIAReady,
	))
	require.NoError(t, err)
	require.Contains(t, string(readyService),
		"ExecStart=/bin/sh -c 'until test -e /run/unbounded/nvidia-ready; do sleep 1; done'")
}

func TestConfigureContainerdDoesNotGateCPUNodes(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	service, err := os.ReadFile(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd))
	require.NoError(t, err)
	require.NotContains(t, string(service), "unbounded-nvidia-ready.service")

	_, err = os.Stat(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitNVIDIAReady))
	require.ErrorIs(t, err, os.ErrNotExist)
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
