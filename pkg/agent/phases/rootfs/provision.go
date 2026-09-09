// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

// RebuildPolicy re-exports the OCI rebuild policy so callers do not have to
// import the OCI package to say what may happen to an existing rootfs.
type RebuildPolicy = oci.RebuildPolicy

const (
	// RebuildNever leaves an existing, unmarked rootfs alone. Use this unless
	// the caller has established the directory is its own.
	RebuildNever = oci.RebuildNever

	// RebuildOwned allows re-extracting over a rootfs the caller has
	// established belongs to an installation of its own that has not started a
	// node.
	RebuildOwned = oci.RebuildOwned
)

// Provision returns a composite task that provisions a complete nspawn machine
// rootfs: bootstrapping the workspace, then downloading Kubernetes, CRI, and
// CNI binaries in parallel with OS configuration.
//
// This is the shared rootfs provisioning sequence used by both the initial
// agent start and node update flows. rebuild decides what happens if the
// machine directory already has unmarked content; see RebuildPolicy.
func Provision(log *slog.Logger, gs *goalstates.RootFS, rebuild RebuildPolicy) phases.Task {
	return phases.Serial(log,
		EnsureNSpawnWorkspace(log, gs, rebuild),
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
