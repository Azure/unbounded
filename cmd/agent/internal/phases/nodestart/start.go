// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"log/slog"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// StartNode returns a composite task that configures and starts an nspawn
// machine node: configuring containerd and kubelet in parallel, then starting
// the nspawn machine, setting up NVIDIA (if applicable), starting containerd
// and kubelet in sequence, and finally persisting the applied config for
// drift detection by the daemon.
//
// This is the shared node-start sequence used by both the initial agent start
// and node update flows. The cfg parameter is the agent config that will be
// persisted as the applied config after a successful start.
func StartNode(log *slog.Logger, gs *goalstates.NodeStart, cfg *provision.AgentConfig) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log,
			ConfigureContainerd(gs),
			ConfigureKubelet(gs),
		),
		StartNSpawnMachine(log, gs),
		SetupNVIDIA(log, gs),
		StartContainerd(log, gs),
		StartKubelet(log, gs),
		PersistAppliedConfig(log, gs.MachineName, cfg),
	)
}
