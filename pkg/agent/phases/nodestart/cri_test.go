// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
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

func TestConfigureContainerdSetsImageVerifier(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(t.Context()))

	data, err := os.ReadFile(filepath.Join(machineDir, goalstates.ContainerdConfigPath))
	require.NoError(t, err)

	var cfg struct {
		Imports []string `toml:"imports"`
		Plugins map[string]struct {
			BinDir             string `toml:"bin_dir"`
			MaxVerifiers       int    `toml:"max_verifiers"`
			PerVerifierTimeout string `toml:"per_verifier_timeout"`
		} `toml:"plugins"`
		Metrics struct {
			Address string `toml:"address"`
		} `toml:"metrics"`
	}

	require.NoError(t, toml.Unmarshal(data, &cfg))
	require.Contains(t, cfg.Plugins, "io.containerd.image-verifier.v1.bindir")
	verifier := cfg.Plugins["io.containerd.image-verifier.v1.bindir"]
	require.Equal(t, "/opt/containerd/image-verifier/bin", verifier.BinDir)
	require.Equal(t, 10, verifier.MaxVerifiers)
	require.Equal(t, "10s", verifier.PerVerifierTimeout)
	require.Equal(t, []string{"/etc/containerd/conf.d/*.toml"}, cfg.Imports)
	require.Contains(t, cfg.Plugins, "io.containerd.grpc.v1.cri")
	require.Contains(t, cfg.Plugins, "io.containerd.transfer.v1.local")
	require.Equal(t, goalState.Containerd.MetricsAddress, cfg.Metrics.Address)
}

func TestConfigureContainerdLeavesImageVerifierDirectory(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"absent", "empty", "populated"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()

			machineDir := t.TempDir()

			binDir := filepath.Join(machineDir, "opt/containerd/image-verifier/bin")
			if state != "absent" {
				require.NoError(t, os.MkdirAll(binDir, 0o755))
			}

			verifierPath := filepath.Join(binDir, "customer-verifier")

			verifier := []byte("#!/bin/sh\nexit 0\n")
			if state == "populated" {
				require.NoError(t, os.WriteFile(verifierPath, verifier, 0o755))
			}

			goalState := &goalstates.NodeStart{
				MachineDir: machineDir,
				Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
			}

			first := &configureContainerd{goalState: goalState}
			require.NoError(t, first.Do(t.Context()))
			require.True(t, first.changed)

			second := &configureContainerd{goalState: goalState}
			require.NoError(t, second.Do(t.Context()))
			require.False(t, second.changed, "an identical reapply must not restart containerd")

			if state == "absent" {
				_, err := os.Stat(filepath.Join(machineDir, "opt/containerd"))
				require.ErrorIs(t, err, os.ErrNotExist)

				return
			}

			entries, err := os.ReadDir(binDir)
			require.NoError(t, err)

			if state == "empty" {
				require.Empty(t, entries)

				return
			}

			require.Len(t, entries, 1)

			data, err := os.ReadFile(verifierPath)
			require.NoError(t, err)
			require.Equal(t, verifier, data)

			info, err := os.Stat(verifierPath)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
		})
	}
}

func TestConfigureContainerdReportsConfigWriteFailure(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(machineDir, "etc"), nil, 0o644))
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
	}

	require.ErrorContains(t, ConfigureContainerd(goalState).Do(t.Context()), "ensure containerd config:")
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
