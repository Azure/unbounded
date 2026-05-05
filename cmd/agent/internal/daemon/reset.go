// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetAgent returns a task that resets the host by stopping the daemon and
// removing the unbounded-agent and all associated resources.
func ResetAgent(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		StopDaemon(log),
		ResetAgentResources(log),
	)
}

// ResetAgentResources returns a task that removes the unbounded-agent and all
// associated resources without stopping the daemon process.
func ResetAgentResources(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		RemoveDaemonUnit(log),
		phases.Parallel(log,
			reset.StopMachine(log, goalstates.NSpawnMachineKube1),
			reset.StopMachine(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveNetworkInterfaces(log),
			reset.RemoveWireGuardKeys(log),
		),
		phases.Parallel(log,
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube1),
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube1),
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube2),
		),
		reset.CleanupRoutes(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}
