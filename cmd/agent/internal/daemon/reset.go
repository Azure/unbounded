// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetAgentResources returns a task that removes the unbounded-agent and all
// associated resources without stopping the daemon process.
func ResetAgentResources(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		// Marking first means an interrupted teardown is never mistaken for an
		// unfinished install that bootstrap may resume: a half-removed host
		// would otherwise satisfy the resume conditions.
		markResetting(log),
		RemoveDaemonUnit(log),
		phases.Parallel(log,
			reset.StopMachine(log, goalstates.NSpawnMachineKube1),
			reset.StopMachine(log, goalstates.NSpawnMachineKube2),
		),
		reset.RemoveWireGuardKeys(log),
		phases.Parallel(log,
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube1),
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube1),
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube1),
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube2),
		),
		reset.CleanupNetwork(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
		// Last: everything above resolves paths through this record, and a
		// teardown interrupted before here has to be able to resume. Clearing
		// it only after systemd has forgotten the removed units means an
		// interrupted reset never leaves identity gone while units linger.
		clearInstallState(log),
	)
}

type installStateTask struct {
	name string
	log  *slog.Logger
	run  func(*slog.Logger) error
}

func (t *installStateTask) Name() string { return t.name }

func (t *installStateTask) Do(context.Context) error { return t.run(t.log) }

// markResetting records that teardown has begun, before anything is removed.
//
// A record that cannot be read is reported rather than skipped. It may describe
// files this reset is about to walk past, and treating it as absent would let
// teardown claim success while leaving them behind.
func markResetting(log *slog.Logger) phases.Task {
	return &installStateTask{name: "mark-resetting", log: log, run: func(log *slog.Logger) error {
		store := installstate.DefaultStore()

		rec, err := store.Load()
		if err != nil {
			if errors.Is(err, installstate.ErrNotFound) {
				// No record: a host provisioned before this state existed, or
				// one already torn down. Neither blocks a reset.
				log.Debug("no installation record to mark as resetting")

				return nil
			}

			return fmt.Errorf(
				"installation record at %s cannot be read, so reset cannot tell what it owns: %w",
				store.StatePath(), err,
			)
		}

		// Recorded before anything is removed, so an interrupted teardown is
		// never mistaken for an unfinished install that bootstrap may resume.
		rec.Checkpoint = installstate.CheckpointResetting
		if err := store.Save(rec); err != nil {
			return err
		}

		return nil
	}}
}

// clearInstallState removes the installation record and completion marker.
func clearInstallState(log *slog.Logger) phases.Task {
	return &installStateTask{name: "clear-install-state", log: log, run: func(log *slog.Logger) error {
		if err := installstate.DefaultStore().Remove(); err != nil {
			return err
		}

		log.Debug("installation record cleared")

		return nil
	}}
}
