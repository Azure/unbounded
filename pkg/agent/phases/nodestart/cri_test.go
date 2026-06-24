// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func hostsTomlPath(machineDir, host string) string {
	return filepath.Join(machineDir, goalstates.ContainerdCertsDir, host, "hosts.toml")
}

func TestEnsureRegistryMirrorsWritesHostsFiles(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	gs := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.Containerd{
			RegistryMirrors: []config.ContainerdRegistryMirror{
				{
					Host:       "registry.k8s.io",
					Server:     "https://registry.k8s.io",
					Mirror:     "http://127.0.0.1:5000",
					SkipVerify: true,
				},
				{
					Host:   "index.docker.io",
					Server: "https://registry-1.docker.io",
					Mirror: "http://127.0.0.1:5000",
				},
			},
		},
	}

	require.NoError(t, ConfigureContainerd(gs).(*configureContainerd).ensureRegistryMirrors())

	data, err := os.ReadFile(hostsTomlPath(machineDir, "registry.k8s.io"))
	require.NoError(t, err)
	require.Contains(t, string(data), `server = "https://registry.k8s.io"`)
	require.Contains(t, string(data), `[host."http://127.0.0.1:5000"]`)
	require.Contains(t, string(data), "  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = true\n")
	require.Contains(t, string(data), registryMirrorMarker)

	data, err = os.ReadFile(hostsTomlPath(machineDir, "index.docker.io"))
	require.NoError(t, err)
	require.Contains(t, string(data), `server = "https://registry-1.docker.io"`)
	require.NotContains(t, string(data), "skip_verify")
}

func TestEnsureRegistryMirrorsNoneConfigured(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	gs := &goalstates.NodeStart{MachineDir: machineDir}

	require.NoError(t, ConfigureContainerd(gs).(*configureContainerd).ensureRegistryMirrors())

	_, err := os.Stat(filepath.Join(machineDir, goalstates.ContainerdCertsDir))
	require.True(t, os.IsNotExist(err))
}

func TestEnsureRegistryMirrorsPrunesStaleManagedEntries(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()

	// First pass writes two mirrors.
	gs := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.Containerd{
			RegistryMirrors: []config.ContainerdRegistryMirror{
				{Host: "registry.k8s.io", Server: "https://registry.k8s.io", Mirror: "http://127.0.0.1:5000"},
				{Host: "quay.io", Server: "https://quay.io", Mirror: "http://127.0.0.1:5000"},
			},
		},
	}
	require.NoError(t, ConfigureContainerd(gs).(*configureContainerd).ensureRegistryMirrors())

	// Second pass drops quay.io.
	gs.Containerd.RegistryMirrors = []config.ContainerdRegistryMirror{
		{Host: "registry.k8s.io", Server: "https://registry.k8s.io", Mirror: "http://127.0.0.1:5000"},
	}
	require.NoError(t, ConfigureContainerd(gs).(*configureContainerd).ensureRegistryMirrors())

	_, err := os.Stat(hostsTomlPath(machineDir, "registry.k8s.io"))
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(machineDir, goalstates.ContainerdCertsDir, "quay.io"))
	require.True(t, os.IsNotExist(err), "stale managed mirror dir should be pruned")
}

func TestEnsureRegistryMirrorsPreservesHandAuthoredEntries(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()

	// A hand-authored certs.d entry with no agent marker.
	handAuthored := filepath.Join(machineDir, goalstates.ContainerdCertsDir, "private.example.com")
	require.NoError(t, os.MkdirAll(handAuthored, 0o755))
	handAuthoredFile := filepath.Join(handAuthored, "hosts.toml")
	require.NoError(t, os.WriteFile(handAuthoredFile, []byte("server = \"https://private.example.com\"\n"), 0o644))

	gs := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.Containerd{
			RegistryMirrors: []config.ContainerdRegistryMirror{
				{Host: "registry.k8s.io", Server: "https://registry.k8s.io", Mirror: "http://127.0.0.1:5000"},
			},
		},
	}
	require.NoError(t, ConfigureContainerd(gs).(*configureContainerd).ensureRegistryMirrors())

	// The hand-authored entry must remain untouched.
	data, err := os.ReadFile(handAuthoredFile)
	require.NoError(t, err)
	require.Contains(t, string(data), "https://private.example.com")
	require.NotContains(t, string(data), registryMirrorMarker)
}

func TestEnsureRegistryMirrorsRejectsInvalid(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	gs := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.Containerd{
			RegistryMirrors: []config.ContainerdRegistryMirror{
				{Host: "registry.k8s.io/bad", Server: "https://registry.k8s.io", Mirror: "http://127.0.0.1:5000"},
			},
		},
	}

	err := ConfigureContainerd(gs).(*configureContainerd).ensureRegistryMirrors()
	require.Error(t, err)
}

func TestConfigureContainerdDoWritesMirrors(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	gs := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.Containerd{
			SandboxImage:      "registry.k8s.io/pause:3.10",
			ContainerdBinPath: "/opt/unbounded/bin/containerd",
			RuncBinaryPath:    "/opt/unbounded/bin/runc",
			CNIBinDir:         "/opt/cni/bin",
			CNIConfDir:        "/etc/cni/net.d",
			MetricsAddress:    "127.0.0.1:1338",
			RegistryMirrors: []config.ContainerdRegistryMirror{
				{Host: "registry.k8s.io", Server: "https://registry.k8s.io", Mirror: "http://127.0.0.1:5000"},
			},
		},
	}

	require.NoError(t, ConfigureContainerd(gs).Do(context.Background()))

	_, err := os.Stat(hostsTomlPath(machineDir, "registry.k8s.io"))
	require.NoError(t, err)
}
