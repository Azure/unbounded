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

func TestConfigureContainerdWritesRegistryHosts(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.Containerd{
			SandboxImage:      goalstates.SandboxImage,
			ContainerdBinPath: "/usr/local/bin/containerd",
			RuncBinaryPath:    "/usr/local/bin/runc",
			CNIBinDir:         goalstates.CNIBinDir,
			CNIConfDir:        goalstates.CNIConfigDir,
			MetricsAddress:    goalstates.ContainerdMetricsAddress,
			RegistryHosts: []goalstates.ContainerdRegistryHost{{
				Host:   "192.168.200.1:5555",
				Server: "http://192.168.200.1:5555",
			}},
		},
	}

	require.NoError(t, ConfigureContainerd(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(machineDir, "/etc/containerd/certs.d/192.168.200.1:5555/hosts.toml"))
	require.NoError(t, err)
	require.Contains(t, string(data), `server = "http://192.168.200.1:5555"`)
	require.Contains(t, string(data), `[host."http://192.168.200.1:5555"]`)
}
