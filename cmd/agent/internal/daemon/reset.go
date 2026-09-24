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
	return ownedReset(log, installstate.DefaultStore(), func(prefix string) phases.Task {
		return phases.Serial(log, StopDaemon(log), resetResources(log, prefix))
	})
}

type lifecycleTask struct {
	name string
	run  func(context.Context) error
}

func (t lifecycleTask) Name() string                 { return t.name }
func (t lifecycleTask) Do(ctx context.Context) error { return t.run(ctx) }

// ownedReset runs a teardown under the installation lock. The teardown is
// built from the prefix once the lock is held, so that it and the sync of what
// it removed use the same one.
func ownedReset(log *slog.Logger, store *installstate.Store, build func(prefix string) phases.Task) phases.Task {
	// The composed name keeps the underlying cleanup sequence visible to callers
	// and to the reset ordering test. Task names do not depend on the prefix.
	return lifecycleTask{name: "owned-reset(" + build("").Name() + ")", run: func(ctx context.Context) error {
		lock, err := store.AcquireLock()
		if err != nil {
			return err
		}
		defer func() {
			if err := lock.Release(); err != nil {
				log.Error("release reset lock", "error", err)
			}
		}()

		return resetUnderLock(ctx, log, store, build)
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

func resetUnderLock(ctx context.Context, log *slog.Logger, store *installstate.Store, build func(prefix string) phases.Task) error {
	prefix, err := beginTeardown(log, store, func() string { return goalstates.HostPrefixFromAppliedConfig(log) })
	if err != nil {
		return err
	}
	// Cancel recovery waiting on ownership before removing its executable.
	if err := stopRecoveryUnit(ctx, log); err != nil {
		return err
	}

	return durableReset(ctx, store, build(prefix), teardownSyncPaths(prefix, store.Root()), unix.Syncfs)
}

// beginTeardown marks the installation as resetting and returns the prefix the
// reset works on.
//
// The record's prefix is used when it has one. When it does not, because it
// was unreadable, absent, or written before it carried one, the applied
// config's is. It is saved in the resetting record, so a reset that is retried
// after the applied config is gone still finds the same files.
func beginTeardown(log *slog.Logger, store *installstate.Store, appliedConfigPrefix func() string) (string, error) {
	r, err := recordForTeardown(log, store)
	if err != nil {
		return "", err
	}

	if r.HostPrefix == "" {
		r.HostPrefix = appliedConfigPrefix()
	}

	r.Phase = installstate.Resetting
	if err := store.Save(r); err != nil {
		return "", err
	}

	return r.HostPrefix, nil
}

// teardownSyncPaths returns the directories whose filesystems have to be
// persisted for a teardown to survive a crash part way through.
//
// Every prefix the host might hold files under is included, not just the
// recorded one: a host reprovisioned with a different prefix still has the old
// layout on disk, and the removal of those files has to be made durable too.
// A prefix that does not exist is not a problem here, because durableReset
// walks up to the nearest existing ancestor before opening anything.
func teardownSyncPaths(prefix, storeRoot string) []string {
	paths := append([]string{"/etc", "/var/lib/machines"}, goalstates.MergeHostPrefixes(prefix)...)

	return append(paths, storeRoot)
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

func resetResources(log *slog.Logger, prefix string) phases.Task {
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
		reset.CleanupNetwork(log, prefix),
		// Before the artifacts, so a failure here stops the reset while the
		// host is still recognizably installed. A unit that survived a reset
		// would bootstrap the host again on the next boot.
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log, prefix),
		reset.ReloadSystemd(log),
	)
}

func releaseInstallationLock(log *slog.Logger, lock *installstate.Lock) {
	if err := lock.Release(); err != nil {
		log.Error("release installation lock", "error", err)
	}
}
