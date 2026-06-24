// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const (
	checkGoalStateName         = "goal-state"
	checkOCIImageReachableName = "oci-image-reachable"
)

type goalStateChecker struct {
	log       *slog.Logger
	config    *config.AgentConfig
	downloads *goalstates.DownloadOverrides
}

// CheckGoalState returns a checker that validates the agent config can be
// resolved into a machine goal state and that an OCI rootfs image is selected.
func CheckGoalState(log *slog.Logger, cfg *config.AgentConfig, downloads *goalstates.DownloadOverrides) preflight.Checker {
	return goalStateChecker{log: log, config: cfg, downloads: downloads}
}

func (c goalStateChecker) Name() string { return checkGoalStateName }

func (c goalStateChecker) Check(context.Context) []preflight.Result {
	gs, err := goalstates.ResolveMachine(c.log, c.config, goalstates.NSpawnMachineKube1, c.downloads)
	if err != nil {
		return preflight.ResultsError(checkGoalStateName, "goal state", "goal state could not be resolved")
	}

	if gs.RootFS.OCIImage == "" {
		// TODO: replace this with an OCI manifest reachability check that uses
		// the same registry parsing and plain-HTTP handling as OCI rootfs
		// provisioning, without pulling image layers.
		return preflight.ResultsError(checkOCIImageReachableName, "rootfs image", "OCI rootfs image is required but no image was selected")
	}

	return preflight.ResultsOK(checkGoalStateName, "goal state", "goal state resolved")
}
