// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded/cmd/agent/internal/phases"
)

type removeNSpawnConfig struct {
	log         *slog.Logger
	machineName string
}

// RemoveNSpawnConfig returns a task that removes the nspawn configuration file
// and the systemd service override directory for the named machine.
func RemoveNSpawnConfig(log *slog.Logger, machineName string) phases.Task {
	return &removeNSpawnConfig{log: log, machineName: machineName}
}

func (t *removeNSpawnConfig) Name() string { return "remove-nspawn-config" }

func (t *removeNSpawnConfig) Do(_ context.Context) error {
	nspawnFile := fmt.Sprintf("%s/%s.nspawn", goalstates.SystemdNSpawnDir, t.machineName)
	overrideDir := fmt.Sprintf("%s/systemd-nspawn@%s.service.d", goalstates.SystemdSystemDir, t.machineName)

	t.log.Info("removing nspawn configuration", "nspawn_file", nspawnFile, "override_dir", overrideDir)

	removeFileIfExists(t.log, nspawnFile)
	removeAllIfExists(t.log, overrideDir)

	return nil
}

type removeAppliedConfig struct {
	log         *slog.Logger
	machineName string
}

// RemoveAppliedConfig returns a task that removes the applied agent config
// file for the named machine. Errors are logged but do not fail the task.
func RemoveAppliedConfig(log *slog.Logger, machineName string) phases.Task {
	return &removeAppliedConfig{log: log, machineName: machineName}
}

func (t *removeAppliedConfig) Name() string { return "remove-applied-config" }

func (t *removeAppliedConfig) Do(_ context.Context) error {
	path := goalstates.AppliedConfigPath(t.machineName)
	removeFileIfExists(t.log, path)

	checksumPath := goalstates.AppliedConfigChecksumPath(t.machineName)
	removeFileIfExists(t.log, checksumPath)

	return nil
}

// CleanupMachine returns a composite task that removes all artifacts of an
// nspawn machine: its nspawn configuration, rootfs, and applied config.
func CleanupMachine(log *slog.Logger, machineName string) phases.Task {
	return phases.Serial(log,
		RemoveNSpawnConfig(log, machineName),
		RemoveMachine(log, machineName),
		RemoveAppliedConfig(log, machineName),
	)
}
