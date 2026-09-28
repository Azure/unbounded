// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// stubMachineRun puts a recording systemd-run on PATH, since MachineRun shells
// out to it, and returns the file each invocation appends its arguments to.
func stubMachineRun(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + record + "\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755))
	t.Setenv("PATH", dir)

	return record
}

func machineRunCalls(t *testing.T, record string) string {
	t.Helper()

	data, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return ""
	}

	require.NoError(t, err)

	return string(data)
}

func reconfigureTask(t *testing.T, wasRunning, containerdChanged, kubeletChanged bool) (*restartReconfigured, string) {
	t.Helper()

	record := stubMachineRun(t)

	return &restartReconfigured{
		log:          slog.New(slog.DiscardHandler),
		goalState:    &goalstates.NodeStart{MachineName: goalstates.NSpawnMachineKube1},
		startMachine: &startNSpawnMachine{wasRunning: wasRunning},
		containerd:   &configureContainerd{changeTracker: changeTracker{changed: containerdChanged}},
		kubelet:      &configureKubelet{changeTracker: changeTracker{changed: kubeletChanged}},
	}, record
}

// TestNoRestartWhenThisRunStartedTheMachine covers the fresh install. The
// services start after the configuration is written, so they already read it.
func TestNoRestartWhenThisRunStartedTheMachine(t *testing.T) {
	task, record := reconfigureTask(t, false, true, true)

	require.NoError(t, task.Do(t.Context()))
	require.Empty(t, machineRunCalls(t, record), "a machine this run started needs no restart")
}

// TestNoRestartWhenNothingChanged is the ordinary retry: bootstrap is rerun
// after a failure, the configuration is identical, and the node is left alone.
func TestNoRestartWhenNothingChanged(t *testing.T) {
	task, record := reconfigureTask(t, true, false, false)

	require.NoError(t, task.Do(t.Context()))
	require.Empty(t, machineRunCalls(t, record), "an unchanged reapply must not disturb a running node")
}

// TestRestartsOnlyTheServiceWhoseConfigChanged keeps a kubelet change from
// bouncing the container runtime underneath it.
func TestRestartsOnlyTheServiceWhoseConfigChanged(t *testing.T) {
	task, record := reconfigureTask(t, true, false, true)

	require.NoError(t, task.Do(t.Context()))

	calls := machineRunCalls(t, record)
	require.Contains(t, calls, "restart "+goalstates.SystemdUnitKubelet)
	require.NotContains(t, calls, "restart "+goalstates.SystemdUnitContainerd)
}

// TestRestartsContainerdBeforeKubelet pins the order. kubelet talks to
// containerd, so restarting kubelet into a restarting runtime would only make
// it retry.
func TestRestartsContainerdBeforeKubelet(t *testing.T) {
	task, record := reconfigureTask(t, true, true, true)

	require.NoError(t, task.Do(t.Context()))

	calls := machineRunCalls(t, record)
	require.Less(t,
		indexOfUnit(calls, goalstates.SystemdUnitContainerd),
		indexOfUnit(calls, goalstates.SystemdUnitKubelet),
		"containerd must restart before kubelet",
	)
}

func indexOfUnit(calls, unit string) int {
	for i := 0; i+len(unit) <= len(calls); i++ {
		if calls[i:i+len(unit)] == unit {
			return i
		}
	}

	return -1
}
