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
		Containerd: goalstates.ResolveContainerd(""),
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

func TestConfigureContainerdEnablesDeviceOwnershipFromSecurityContext(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(""),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	path := filepath.Join(machineDir, goalstates.ContainerdConfigPath)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), `[plugins.'io.containerd.cri.v1.runtime']
device_ownership_from_security_context = true`)
}

func TestConfigureContainerdUpdatesManagedGantryHostsConfig(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	path := filepath.Join(machineDir, goalstates.ContainerdDefaultHostsPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(gantryHostsManagedMarker+"\nold = true\n"), 0o600))

	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(""),
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
		Containerd: goalstates.ResolveContainerd(""),
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
		Containerd: goalstates.ResolveContainerd(""),
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
		Containerd: goalstates.ResolveContainerd(""),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, data)
}
