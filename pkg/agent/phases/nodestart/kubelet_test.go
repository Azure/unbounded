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

func TestConfigureKubeletWritesHostnameOverride(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			NodeIP:     "10.0.0.15",
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
		},
	}

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.Contains(t, string(data), "--hostname-override=worker-1")
	require.Contains(t, string(data), "--node-ip=10.0.0.15")
}

func TestConfigureKubeletOmitsNodeIPWhenEmpty(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	goalState := &goalstates.NodeStart{
		MachineDir: machineDir,
		NodeName:   "worker-1",
		Kubelet: goalstates.Kubelet{
			APIServer:  "https://api.example.com",
			CACertData: []byte("ca"),
			KubeletAuthInfo: config.KubeletAuthInfo{
				BootstrapToken: "token",
			},
			ClusterDNS: "10.0.0.10",
		},
	}

	require.NoError(t, ConfigureKubelet(goalState).Do(context.Background()))

	data, err := os.ReadFile(filepath.Join(
		machineDir,
		goalstates.KubeletServiceDropInDir,
		"20-node-config.conf",
	))
	require.NoError(t, err)
	require.NotContains(t, string(data), "--node-ip=")
}
