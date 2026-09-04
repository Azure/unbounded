// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// StartNode returns a composite task that configures and starts an nspawn
// machine node: configuring containerd and kubelet in parallel, then starting
// the nspawn machine and starting containerd and kubelet in sequence. NVIDIA
// setup is owned by the nspawn ExecStartPost lifecycle hook, which safely
// restarts GPU services around driver rewiring.
//
// This is the shared node-start sequence used by both the initial agent start
// and node update flows. Callers that need to persist the applied config for
// drift detection should append that step separately.
func StartNode(log *slog.Logger, gs *goalstates.NodeStart) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log,
			ConfigureContainerd(gs),
			ConfigureKubelet(gs),
			ConfigureLocalDNS(gs),
		),
		SetupLocalDNSNetwork(log, gs),
		StartNSpawnMachine(log, gs),
		WaitForLocalDNS(log, gs),
		StartContainerd(log, gs),
		ImportContainerImages(log, gs),
		StartKubelet(log, gs),
	)
}
