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

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
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

// reconcileHostRootUnderLock runs the operator's ReconcileHostRoot for the
// active machine while holding installation ownership, which keeps an
// agent-upgrade or a reset from changing the layout under it.
//
// A failure is logged, never returned. The daemon is healthy either way, and
// failing its start would count towards the unit's start limit and could roll
// the binary back for a problem it does not have. The next start retries.
func reconcileHostRootUnderLock(ctx context.Context, log *slog.Logger, store *installstate.Store, operator nodeOperator, active *ActiveMachine) {
	lock, err := store.AcquireMutationLock()
	if err != nil {
		log.Warn("not reconciling the host root: installation ownership is not available", "error", err)

		return
	}

	defer releaseInstallationLock(log, lock)

	if err := operator.ReconcileHostRoot(ctx, log, active); err != nil {
		log.Warn("could not move the agent's files to the host root; the next daemon start retries", "error", err)
	}
}

// reconcileHostRoot moves a host an older agent installed to the host root
// once that cannot strand a rollback, and on a host installed under the host
// root removes the binary install scripts seed under the legacy root for older
// agents.
func reconcileHostRoot(ctx context.Context, log *slog.Logger, operator nodeOperator, active *ActiveMachine) error {
	state, err := hostroot.CurrentState()
	if err != nil {
		return err
	}

	if state == hostroot.StateInstalled {
		return hostroot.RemoveSeed(log)
	}

	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return err
	}

	_, err = hostroot.ReconcileMove(ctx, log, hostroot.MoveOptions{
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
		Restart: func(ctx context.Context) error {
			return operator.RestartAgentDaemon(ctx, log)
		},
	})

	return err
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
