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
	"time"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

// MigrateHostRoot links the host root to the legacy root on a host installed
// by an agent released before the host root. Commands that change the host
// call it before resolving any path; see hostroot.Migrate.
func MigrateHostRoot(log *slog.Logger) error {
	return hostroot.Migrate(log, hostroot.Markers()...)
}

// hostRootAgentsPath records the digest of every daemon binary that has run on
// a host whose root is linked to the legacy root; see hostroot.ReconcileMove.
// It is under the agent config directory, which reset removes.
var hostRootAgentsPath = filepath.Join(goalstates.AgentConfigDir, "host-root-agents")

// hostRootRestartWait bounds how long a daemon that queued its own restart
// waits for systemd to replace it.
const hostRootRestartWait = 2 * time.Minute

// reconcileHostRootUnderLock runs the operator's ReconcileHostRoot for the
// active machine while holding installation ownership, which keeps an
// agent-upgrade or a reset from changing the layout under it. It reports
// whether the daemon's restart was queued; the lock is released either way.
//
// A failure is logged, never returned. The daemon is healthy either way, and
// failing its start would count toward the unit's start limit and could roll
// the binary back for a problem it does not have. The next start retries.
func reconcileHostRootUnderLock(ctx context.Context, log *slog.Logger, store *installstate.Store, operator nodeOperator, active *ActiveMachine) bool {
	lock, err := store.AcquireMutationLock()
	if err != nil {
		log.Warn("not reconciling the host root: installation ownership is not available", "error", err)

		return false
	}

	defer releaseInstallationLock(log, lock)

	restarted, err := operator.ReconcileHostRoot(ctx, log, active)
	if err != nil {
		log.Warn("could not move the agent's files to the host root; the next daemon start retries", "error", err)
	}

	return restarted
}

// awaitReplacement waits for systemd to replace a daemon that queued its own
// restart from the host root. It takes no work in the meantime: the restart
// stops it at any point. It returns an error if ctx ends or the restart has
// not come within wait, so the unit's Restart= starts the daemon from the
// rewritten units instead.
func awaitReplacement(ctx context.Context, log *slog.Logger, wait time.Duration) error {
	log.Info("waiting to be restarted from the host root", "timeout", wait)

	select {
	case <-ctx.Done():
		return fmt.Errorf("stopped while waiting to be restarted from the host root: %w", ctx.Err())
	case <-time.After(wait):
		return fmt.Errorf("not restarted from the host root within %s", wait)
	}
}

// reconcileHostRoot moves a host an older agent installed to the host root
// once that cannot strand a rollback, and, once nothing the agent runs is under
// the legacy root, removes the binary install scripts seed there for older
// agents. It reports whether it queued the daemon's restart.
func reconcileHostRoot(ctx context.Context, log *slog.Logger, operator nodeOperator, active *ActiveMachine) (bool, error) {
	released, err := hostroot.LegacyReleased()
	if err != nil {
		return false, err
	}

	// Installed under the host root, or under a link an operator made to
	// somewhere else, so there is nothing to move.
	if released {
		return false, hostroot.RemoveSeed(log)
	}

	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return false, err
	}

	return hostroot.ReconcileMove(ctx, log, hostroot.MoveOptions{
		Files: hostroot.Layout(),
		// The directories a fresh installation's PrepareHost creates.
		Subdirs:      []string{"bin", "libexec"},
		Record:       hostRootAgentsPath,
		SignalPath:   paths.SignalPath,
		CurrentPath:  paths.CurrentPath,
		LastGoodPath: paths.LastGoodPath,
		RewriteUnits: func(ctx context.Context) error {
			return rewriteHostRootUnits(ctx, log, operator, active)
		},
		Verify: verifyMovedDaemon,
		Restart: func(ctx context.Context) error {
			return operator.RestartAgentDaemon(ctx, log)
		},
	})
}

// verifyMovedDaemon runs the current daemon binary from a copy of the layout
// under root.
func verifyMovedDaemon(ctx context.Context, root string) error {
	return agentbinary.Verify(ctx, filepath.Join(root, "bin", hostroot.BinaryCurrentName))
}

// rewriteHostRootUnits points every unit and script that names the agent's
// files at the files under the host root.
func rewriteHostRootUnits(ctx context.Context, log *slog.Logger, operator nodeOperator, active *ActiveMachine) error {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return err
	}

	// The daemon unit, the recovery unit and the recovery script.
	if err := NewHostDaemonActivationService(log, paths).Prepare(ctx, paths.CurrentPath); err != nil {
		return err
	}

	// The LocalDNS network unit and its helper, on a host that has them. The
	// network they configure is already up, so the unit is not run again.
	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); err == nil {
		gs, err := goalstates.ResolveMachine(log, active.Config, active.Name, nil)
		if err != nil {
			return fmt.Errorf("resolve machine goal state: %w", err)
		}

		if err := nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Last, because it reloads systemd: the nspawn lifecycle helper, the
	// machine's service override and its config regeneration unit.
	if err := operator.EnsureLifecycleMigration(ctx, log, active); err != nil {
		return err
	}

	return fsutil.SyncFilesystems(goalstates.SystemdSystemDir, hostroot.Resolve())
}
