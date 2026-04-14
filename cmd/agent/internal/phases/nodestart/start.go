// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"log/slog"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
)

// StartNode returns a composite task that configures and starts an nspawn
// machine node: configuring containerd and kubelet in parallel, then starting
// the nspawn machine, setting up NVIDIA (if applicable), and starting
// containerd and kubelet in sequence.
//
// This is the shared node-start sequence used by both the initial agent start
// and node update flows.
func StartNode(log *slog.Logger, gs *goalstates.NodeStart) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log,
			ConfigureContainerd(gs),
			ConfigureKubelet(gs),
		),
		StartNSpawnMachine(log, gs),
		SetupNVIDIA(log, gs),
		StartContainerd(log, gs),
		StartKubelet(log, gs),
	)
}
