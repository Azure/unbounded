// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkOCIImageReachableName = "oci-image-reachable"

// Preflight returns the standard rootfs checks for a resolved machine goal state.
//
// The none mode installs the same release artifacts but has no OS image to pull
// and no machine to provision, so those two checks are left out for it. The
// container mode pulls the same OS image but provisions no nspawn machine, so
// only the machine check is left out for it; the container backend's own
// preflight (host phase) covers its directories.
func Preflight(log *slog.Logger, cfg config.AgentConfig, goalState *goalstates.MachineGoalState) []preflight.Checker {
	rootFS := goalState.RootFS

	hostNative := cfg.Isolation == string(isolation.ModeNone)
	noMachine := hostNative || cfg.Isolation == string(isolation.ModeContainer)

	var checks []preflight.Checker
	if !hostNative {
		checks = append(checks, CheckOCIImageReachable(log, rootFS))
	}

	checks = append(checks,
		CheckKubernetesArtifacts(log, rootFS),
		CheckCRIArtifacts(log, rootFS),
		CheckCNIArtifacts(log, rootFS),
	)

	if !noMachine {
		checks = append(checks, CheckNSpawnMachineProvisioning(log, rootFS))
	}

	if rootFS.LocalDNS.Enabled {
		checks = append(checks, CheckLocalDNSArtifact(log, rootFS))
	}

	return checks
}
