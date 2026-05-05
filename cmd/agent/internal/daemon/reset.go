// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

func resetAgent(ctx context.Context, log *slog.Logger) error {
	return phases.Serial(log,
		StopDaemon(log),
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
	).Do(ctx)
}
