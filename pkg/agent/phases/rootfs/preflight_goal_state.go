// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkGoalStateName         = "goal-state"
	checkOCIImageReachableName = "oci-image-reachable"
)

type goalStateChecker struct {
	log    *slog.Logger
	rootFS *goalstates.RootFS
}

// Preflight returns the standard rootfs checks for a resolved machine goal state.
func Preflight(log *slog.Logger, _ *provision.UnboundedAgentConfig, goalState *goalstates.MachineGoalState) []preflight.Checker {
	var rootFS *goalstates.RootFS
	if goalState != nil {
		rootFS = goalState.RootFS
	}

	return []preflight.Checker{
		CheckGoalState(log, rootFS),
		CheckNSpawnMachineProvisioning(log, rootFS),
	}
}

// CheckGoalState validates the agent config resolved into a machine goal state.
func CheckGoalState(log *slog.Logger, rootFS *goalstates.RootFS) preflight.Checker {
	return goalStateChecker{log: log, rootFS: rootFS}
}

func (c goalStateChecker) Name() string { return checkGoalStateName }

func (c goalStateChecker) Check(context.Context) []preflight.Result {
	if c.rootFS == nil {
		return preflight.ResultsError(checkGoalStateName, "goal state", "goal state could not be resolved")
	}

	if c.rootFS.OCIImage == "" {
		// TODO: replace this with an OCI manifest reachability check that uses
		// the same registry parsing and plain-HTTP handling as OCI rootfs
		// provisioning, without pulling image layers.
		return preflight.ResultsError(
			checkOCIImageReachableName,
			"rootfs image",
			"OCI rootfs image is required but no image was selected",
		)
	}

	return preflight.ResultsOK(checkGoalStateName, "goal state", "goal state resolved")
}
