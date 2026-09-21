// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Provision returns a composite task that provisions a complete nspawn machine
// rootfs: bootstrapping the workspace, then downloading Kubernetes, CRI, and
// CNI binaries in parallel with OS configuration.
//
// This is the shared rootfs provisioning sequence used by both the initial
// agent start and node update flows.
func Provision(log *slog.Logger, gs *goalstates.RootFS) phases.Task {
	return provisionWithWorkspace(log, gs, EnsureNSpawnWorkspace(log, gs))
}

// ProvisionOwned replays an initial bootstrap rootfs under established host
// ownership. Never use it for a slot that may have started a node.
func ProvisionOwned(log *slog.Logger, gs *goalstates.RootFS) phases.Task {
	return provisionWithWorkspace(log, gs, &ensureNSpawnWorkspace{log: log, goalState: gs, ownedReplay: true})
}

func provisionWithWorkspace(log *slog.Logger, gs *goalstates.RootFS, workspace phases.Task) phases.Task {
	return phases.Serial(log,
		workspace,
		phases.Parallel(log,
			DownloadKubeBinaries(log, gs),
			DownloadCRIBinaries(log, gs),
			DownloadCNIBinaries(log, gs),
			ConfigureOS(gs),
			DisableResolved(gs),
			ConfigureLocalDNS(log, gs),
		),
	)
}
