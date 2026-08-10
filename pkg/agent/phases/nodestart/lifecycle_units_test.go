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

func TestEnsureNSpawnLifecycleUnitsMigratesLegacyGPUNode(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	state := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: true}),
		Kubelet:    goalstates.Kubelet{KubeletBinPath: "/usr/local/bin/kubelet"},
		Nvidia:     goalstates.NvidiaHost{Required: true},
	}
	require.NoError(t, EnsureNSpawnLifecycleUnits(state).Do(context.Background()))
	require.NoError(t, EnsureNSpawnLifecycleUnits(state).Do(context.Background()))

	containerd := readLifecycleUnit(t, machineDir, goalstates.SystemdUnitContainerd)
	require.Contains(t, containerd, "Requires=unbounded-nvidia-ready.service")

	kubelet := readLifecycleUnit(t, machineDir, goalstates.SystemdUnitKubelet)
	require.Contains(t, kubelet, "Requires=unbounded-nvidia-ready.service")

	ready := readLifecycleUnit(t, machineDir, goalstates.SystemdUnitNVIDIAReady)
	require.Contains(t, ready, goalstates.NVIDIAReadyPath)
}

func TestEnsureNSpawnLifecycleUnitsKeepsLegacyCPUNodeUngated(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	state := &goalstates.NodeStart{
		MachineDir: machineDir,
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{}),
		Kubelet:    goalstates.Kubelet{KubeletBinPath: "/usr/local/bin/kubelet"},
	}
	require.NoError(t, EnsureNSpawnLifecycleUnits(state).Do(context.Background()))

	require.NotContains(t, readLifecycleUnit(t, machineDir, goalstates.SystemdUnitContainerd), "unbounded-nvidia-ready")
	require.NotContains(t, readLifecycleUnit(t, machineDir, goalstates.SystemdUnitKubelet), "unbounded-nvidia-ready")

	_, err := os.Stat(filepath.Join(machineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitNVIDIAReady))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func readLifecycleUnit(t *testing.T, machineDir, unit string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(machineDir, goalstates.SystemdSystemDir, unit))
	require.NoError(t, err)

	return string(data)
}
