// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetAgentResources returns a task that removes the unbounded-agent and all
// associated resources without stopping the daemon process.
//
// The whole sequence runs under the host installation lock. Bootstrap and reset
// mutate the same files and the same installation record, and the bootstrap
// unit retries on a timer, so a reset starting while a bootstrap is partway
// through is a real possibility: without the lock they would interleave, and
// the loser would be a half-removed host that neither one owns.
func ResetAgentResources(log *slog.Logger) phases.Task {
	return withInstallLock(log, resetAgentResources(log))
}

func resetAgentResources(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		// Marking first means an interrupted teardown is never mistaken for an
		// unfinished install that bootstrap may resume: a half-removed host
		// would otherwise satisfy the resume conditions.
		markResetting(log),
		removeBootstrapUnit(log),
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

// Remove the first-boot entry point before its config or completion record.
// Otherwise rebooting a reset Ignition host can restart a dangling bootstrap.
func removeBootstrapUnit(log *slog.Logger) phases.Task {
	return &installStateTask{name: "remove-bootstrap-unit", log: log, run: func(ctx context.Context, log *slog.Logger) error {
		const unit = "unbounded-agent-bootstrap.service"

		path := filepath.Join(goalstates.SystemdSystemDir, unit)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}

		if err := executil.RunCmd(ctx, log, executil.Systemctl(), "disable", "--now", unit); err != nil {
			return fmt.Errorf("disable bootstrap unit before reset: %w", err)
		}

		return os.Remove(path)
	}}
}

type installStateTask struct {
	name string
	log  *slog.Logger
	run  func(context.Context, *slog.Logger) error
}

func (t *installStateTask) Name() string { return t.name }

func (t *installStateTask) Do(ctx context.Context) error { return t.run(ctx, t.log) }

// markResetting records that teardown has begun, before anything is removed.
//
// A record that cannot be read is reported rather than skipped. It may describe
// files this reset is about to walk past, and treating it as absent would let
// teardown claim success while leaving them behind.
func markResetting(log *slog.Logger) phases.Task {
	return &installStateTask{name: "mark-resetting", log: log, run: func(_ context.Context, log *slog.Logger) error {
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
	return &installStateTask{name: "clear-install-state", log: log, run: func(_ context.Context, log *slog.Logger) error {
		if err := installstate.DefaultStore().Remove(); err != nil {
			return err
		}

		log.Debug("installation record cleared")

		return nil
	}}
}

// lockedTask wraps a task so it runs while holding the host installation lock.
type lockedTask struct {
	log   *slog.Logger
	inner phases.Task
}

func (t *lockedTask) Name() string { return t.inner.Name() }

func (t *lockedTask) Do(ctx context.Context) error {
	lock, err := installstate.AcquireLock()
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.Release(); err != nil {
			t.log.Warn("releasing the installation lock", "error", err)
		}
	}()

	return t.inner.Do(ctx)
}

// withInstallLock returns a task that holds the host installation lock for the
// duration of the wrapped task.
//
// Acquisition does not block: the callers are a CLI command and a retrying
// reconciler, so failing with a clear "something else is running" is more
// useful than queueing behind work that may itself be stuck.
func withInstallLock(log *slog.Logger, inner phases.Task) phases.Task {
	return &lockedTask{log: log, inner: inner}
}
