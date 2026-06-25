// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkGoalStateName         = "goal-state"
	checkOCIImageReachableName = "oci-image-reachable"
)

type goalStateChecker struct {
	log    *slog.Logger
	err    error
	rootFS *goalstates.RootFS
}

// CheckGoalState validates the agent config resolved into a machine goal state.
func CheckGoalState(log *slog.Logger, err error, rootFS *goalstates.RootFS) preflight.Checker {
	return goalStateChecker{log: log, err: err, rootFS: rootFS}
}

func (c goalStateChecker) Name() string { return checkGoalStateName }

func (c goalStateChecker) Check(context.Context) []preflight.Result {
	if c.err != nil || c.rootFS == nil {
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
