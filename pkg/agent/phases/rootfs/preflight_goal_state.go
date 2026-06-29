// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"log/slog"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkOCIImageReachableName = "oci-image-reachable"

// Preflight returns the standard rootfs checks for a resolved machine goal state.
func Preflight(log *slog.Logger, _ *provision.UnboundedAgentConfig, goalState *goalstates.MachineGoalState) []preflight.Checker {
	var rootFS *goalstates.RootFS
	if goalState != nil {
		rootFS = goalState.RootFS
	}

	return []preflight.Checker{
		CheckOCIImageReachable(log, rootFS),
		CheckKubernetesArtifacts(log, rootFS),
		CheckCRIArtifacts(log, rootFS),
		CheckCNIArtifacts(log, rootFS),
		CheckNSpawnMachineProvisioning(log, rootFS),
	}
}
