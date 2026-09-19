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
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetAgent removes the unbounded-agent and all associated resources, stopping
// the daemon first. The daemon's own operation path stops it last instead, so
// that ordering stays with the caller.
func ResetAgent(log *slog.Logger) phases.Task {
	return ownedReset(log, installstate.DefaultStore(), phases.Serial(log, StopDaemon(log), resetResources(log)))
}

type lifecycleTask struct {
	name string
	run  func(context.Context) error
}

func (t lifecycleTask) Name() string                 { return t.name }
func (t lifecycleTask) Do(ctx context.Context) error { return t.run(ctx) }

func ownedReset(log *slog.Logger, store *installstate.Store, inner phases.Task) phases.Task {
	// The composed name keeps the underlying cleanup sequence visible to callers
	// and to the reset ordering test.
	return lifecycleTask{name: "owned-reset(" + inner.Name() + ")", run: func(ctx context.Context) error {
		lock, err := store.AcquireLock()
		if err != nil {
			return err
		}
		defer func() {
			if err := lock.Release(); err != nil {
				log.Error("release reset lock", "error", err)
			}
		}()

		return resetUnderLock(ctx, log, store, inner)
	}}
}

// recordForTeardown returns the record reset should mark as resetting.
//
// Any record it cannot read is replaced rather than obeyed. Reset is about to
// delete it, so refusing to proceed protects nothing and costs everything:
// decide rejects the same unreadable record, so start is refused too, and the
// host is left with no way out through either path. The guide tells operators
// to keep this file intact, so it must not be the thing that strands them.
func recordForTeardown(log *slog.Logger, store *installstate.Store) (installstate.Record, error) {
	r, err := store.Load()
	if err == nil {
		return r, nil
	}

	if !errors.Is(err, installstate.ErrNotFound) {
		log.Warn("installation record is unreadable; replacing it for teardown", "error", err)
	}

	return installstate.NewRecord("legacy-reset", "legacy-reset", "")
}

func resetUnderLock(ctx context.Context, log *slog.Logger, store *installstate.Store, inner phases.Task) error {
	r, err := recordForTeardown(log, store)
	if err != nil {
		return err
	}

	r.Phase = installstate.Resetting
	if err := store.Save(r); err != nil {
		return err
	}
	// Cancel recovery waiting on ownership before removing its executable.
	if err := stopRecoveryUnit(ctx, log); err != nil {
		return err
	}

	return durableReset(ctx, store, inner, []string{"/etc", "/var/lib/machines", "/usr/local", store.Root()}, unix.Syncfs)
}

func stopRecoveryUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "stop", goalstates.DaemonRecoveryUnit); err != nil {
		out, inspectErr := executil.OutputCmd(ctx, log, "systemctl", "show", goalstates.DaemonRecoveryUnit, "--property=LoadState", "--value")
		if inspectErr != nil || strings.TrimSpace(out) != "not-found" {
			return fmt.Errorf("stop daemon recovery: %w", err)
		}
	}

	return nil
}

func durableReset(ctx context.Context, store *installstate.Store, inner phases.Task, paths []string, syncfs func(int) error) error {
	var handles []*os.File
	defer func() {
		for _, f := range handles {
			_ = f.Close() //nolint:errcheck // Read-only directory descriptor; teardown sync errors are returned below.
		}
	}()

	for _, path := range paths {
		for {
			if _, err := os.Stat(path); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}

			parent := filepath.Dir(path)
			if parent == path {
				return fmt.Errorf("no filesystem ancestor for %s", path)
			}

			path = parent
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}

		handles = append(handles, f)
	}

	if err := inner.Do(ctx); err != nil {
		return err
	}

	if err := fsutil.SyncOpenFilesystems(handles, syncfs); err != nil {
		return err
	}

	return store.Remove()
}

func resetResources(log *slog.Logger) phases.Task {
	return phases.Serial(log,
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
	)
}

func releaseInstallationLock(log *slog.Logger, lock *installstate.Lock) {
	if err := lock.Release(); err != nil {
		log.Error("release installation lock", "error", err)
	}
}
