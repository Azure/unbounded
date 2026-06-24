// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func validRootFSGoalState(t *testing.T) *goalstates.RootFS {
	t.Helper()

	dir := t.TempDir()

	return &goalstates.RootFS{
		MachineDir:          dir,
		NSpawnConfigFile:    dir + "/nspawn/kube1.nspawn",
		ServiceOverrideFile: dir + "/systemd/system/systemd-nspawn@kube1.service.d/override.conf",
		OCIImage:            "registry.example.com/unbounded/rootfs:v1",
	}
}

func TestCheckGoalStateOK(t *testing.T) {
	results := CheckGoalState(slog.New(slog.DiscardHandler), nil, validRootFSGoalState(t)).Check(context.Background())

	assert.Equal(t, []preflight.Result{preflight.OK(checkGoalStateName, "goal state", "goal state resolved")}, results)
}

func TestCheckGoalStateResolveError(t *testing.T) {
	results := CheckGoalState(slog.New(slog.DiscardHandler), errors.New("boom"), nil).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkGoalStateName, results[0].Name)
	assert.Equal(t, "goal state could not be resolved", results[0].Message)
}
