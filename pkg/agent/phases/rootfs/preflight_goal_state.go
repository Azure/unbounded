// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkOCIImageReachableName = "oci-image-reachable"

// Preflight returns the standard rootfs checks for a resolved machine goal state.
func Preflight(log *slog.Logger, _ config.AgentConfig, goalState *goalstates.MachineGoalState) []preflight.Checker {
	rootFS := goalState.RootFS

	checks := []preflight.Checker{
		CheckOCIImageReachable(log, rootFS),
		CheckKubernetesArtifacts(log, rootFS),
		CheckCRIArtifacts(log, rootFS),
		CheckCNIArtifacts(log, rootFS),
		CheckNSpawnMachineProvisioning(log, rootFS),
	}
	if rootFS.LocalDNS.Enabled {
		checks = append(checks, CheckLocalDNSArtifact(log, rootFS))
	}

	return checks
}
