// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

// MigrateHostRoot links the host root to the legacy root on a host installed
// by an agent released before the host root. Commands that change the host
// call it before resolving any path; see hostroot.Migrate.
func MigrateHostRoot(log *slog.Logger) error {
	return hostroot.Migrate(log, goalstates.HostRootMarkers()...)
}

// hostRootAgentsPath records the SHA-256 digest of every agent binary that has
// run as the daemon on a host whose root is linked to the legacy root. Only
// agents that know about the host root ever write it, so a binary whose digest
// is missing predates the host root and needs the legacy layout to run.
//
// It is under the agent config directory, which reset removes.
var hostRootAgentsPath = filepath.Join(goalstates.AgentConfigDir, "host-root-agents")

// hostRootSteps is the host work reconcileHostRoot orders. The daemon uses
// hostRootHost; tests substitute a fake to check the order and the resume.
type hostRootSteps interface {
	// State reports what the host root is.
	State() (hostroot.State, error)
	// RecordSelf records this daemon's binary as one that knows the host root.
	RecordSelf() error
	// Ready reports whether the host can be moved, and if not, why.
	Ready() (bool, string, error)
	// Move puts a copy of the legacy layout in place of the link.
	Move(context.Context) error
	// RewriteUnits points every unit and script at the files under the host
	// root, and reloads systemd.
	RewriteUnits(context.Context) error
	// FinishMove removes the layout under the legacy root, then marks the
	// move finished.
	FinishMove() error
	// RemoveSeed removes a binary an install script seeded under the legacy
	// root for older agents.
	RemoveSeed() error
	// Restart restarts the daemon, whose own binary the move removed.
	Restart(context.Context) error
}

// reconcileHostRoot moves the agent's files from the legacy root into a real
// directory at the host root on a host installed by an agent released before
// it, once the move cannot strand a rollback.
//
// A linked host is moved only when neither the current nor the last-good
// binary predates the host root. Until then the upgrade from the older agent
// can still roll back to it, and it needs its files under the legacy root.
// After the next AgentUpgrade the older binary is no longer in either slot,
// and the host is moved at the daemon start that follows it.
//
// The move keeps every path the units name valid at each step: the legacy
// files stay until the units name the new ones. A move interrupted after the
// swap is finished at the next start; one interrupted before it starts over.
//
// On a host installed under the host root, it removes the binary install
// scripts seed under the legacy root for older agents.
func reconcileHostRoot(ctx context.Context, log *slog.Logger, steps hostRootSteps) error {
	state, err := steps.State()
	if err != nil {
		return err
	}

	switch state {
	case hostroot.StateInstalled:
		return steps.RemoveSeed()
	case hostroot.StateMoving:
		log.Info("finishing the move of the agent's files to the host root", "path", hostroot.Path)

		return completeHostRootMove(ctx, steps)
	case hostroot.StateLinked:
	case hostroot.StateAbsent, hostroot.StateOther:
		return nil
	}

	if err := steps.RecordSelf(); err != nil {
		return err
	}

	ready, reason, err := steps.Ready()
	if err != nil {
		return err
	}

	if !ready {
		log.Info("keeping the agent's files under the legacy root", "path", hostroot.LegacyPath, "reason", reason)

		return nil
	}

	log.Info("moving the agent's files to the host root", "from", hostroot.LegacyPath, "to", hostroot.Path)

	if err := steps.Move(ctx); err != nil {
		return err
	}

	return completeHostRootMove(ctx, steps)
}

func completeHostRootMove(ctx context.Context, steps hostRootSteps) error {
	// The units first, so nothing names the legacy files when they go.
	if err := steps.RewriteUnits(ctx); err != nil {
		return err
	}

	if err := steps.FinishMove(); err != nil {
		return err
	}

	// The running daemon's binary is gone from disk. It keeps running, but
	// anything that copies its own executable, such as the nspawn lifecycle
	// helper during a repave, would find nothing.
	return steps.Restart(ctx)
}

// reconcileHostRootUnderLock runs reconcileHostRoot for the active machine
// while holding installation ownership, which keeps an agent-upgrade or a reset
// from changing the layout under it.
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

// hostRootHost is the real host work of reconcileHostRoot.
type hostRootHost struct {
	log      *slog.Logger
	active   *ActiveMachine
	operator nodeOperator
	agents   string
}

func newHostRootHost(log *slog.Logger, active *ActiveMachine, operator nodeOperator) hostRootHost {
	return hostRootHost{log: log, active: active, operator: operator, agents: hostRootAgentsPath}
}

func (h hostRootHost) State() (hostroot.State, error) { return hostroot.CurrentState() }

func (h hostRootHost) RecordSelf() error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve the daemon's executable: %w", err)
	}

	digest, err := fileDigest(self)
	if err != nil {
		return err
	}

	return recordAgentDigest(h.agents, digest)
}

func (h hostRootHost) Ready() (bool, string, error) {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return false, "", err
	}

	return hostRootMoveReady(h.agents, paths.SignalPath, paths.CurrentPath, paths.LastGoodPath)
}

func (h hostRootHost) Move(ctx context.Context) error {
	// The directories a fresh installation's PrepareHost creates, so a moved
	// host is laid out the same way.
	return hostroot.Move(ctx, h.log, goalstates.HostLayout(), "bin", "libexec")
}

func (h hostRootHost) RewriteUnits(ctx context.Context) error {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return err
	}

	// The daemon unit, the recovery unit and the recovery script.
	if err := NewHostDaemonActivationService(h.log, paths).Prepare(ctx, paths.CurrentPath); err != nil {
		return err
	}

	if err := h.rewriteLocalDNS(); err != nil {
		return err
	}

	// Last, because it reloads systemd: the nspawn lifecycle helper, the
	// machine's service override and its config regeneration unit.
	if err := h.operator.EnsureLifecycleMigration(ctx, h.log, h.active); err != nil {
		return err
	}

	return fsutil.SyncFilesystems(goalstates.SystemdSystemDir, hostroot.Resolve())
}

// rewriteLocalDNS rewrites the LocalDNS network unit and its helper on a host
// that has them. The network they configure is already up, so the unit is not
// run again.
func (h hostRootHost) rewriteLocalDNS() error {
	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}

	gs, err := goalstates.ResolveMachine(h.log, h.active.Config, h.active.Name, nil)
	if err != nil {
		return fmt.Errorf("resolve machine goal state: %w", err)
	}

	return nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart)
}

func (h hostRootHost) FinishMove() error {
	for _, path := range goalstates.LegacyLayoutFiles() {
		if err := removeOwnedFile(path); err != nil {
			return err
		}
	}

	if err := fsutil.SyncFilesystems(hostroot.LegacyPath); err != nil {
		return err
	}

	// Only a linked host reads it, and this one is no longer linked.
	if err := removeOwnedFile(h.agents); err != nil {
		return err
	}

	return hostroot.FinishMove()
}

func (h hostRootHost) RemoveSeed() error {
	return hostroot.RemoveSeed(h.log, goalstates.LegacySeedFile())
}

func (h hostRootHost) Restart(ctx context.Context) error {
	return h.operator.RestartAgentDaemon(ctx, h.log)
}

// hostRootMoveReady reports whether a linked host can be moved: no
// AgentUpgrade is waiting to be reported, and the current and last-good links
// resolve to binaries recorded in agentsPath. A binary that is not recorded has
// never run the code that records it, so it predates the host root.
func hostRootMoveReady(agentsPath, signalPath, currentPath, lastGoodPath string) (bool, string, error) {
	if _, err := os.Stat(signalPath); err == nil {
		return false, "an AgentUpgrade has not been reported yet", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, "", fmt.Errorf("inspect AgentUpgrade signal: %w", err)
	}

	known, err := loadAgentDigests(agentsPath)
	if err != nil {
		return false, "", err
	}

	for _, slot := range [][2]string{{"current", currentPath}, {"last-good", lastGoodPath}} {
		name, link := slot[0], slot[1]

		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			return false, fmt.Sprintf("the %s binary does not resolve: %v", name, err), nil
		}

		digest, err := fileDigest(target)
		if err != nil {
			return false, "", err
		}

		if !known[digest] {
			return false, fmt.Sprintf("the %s binary %s predates the host root; the move follows the next AgentUpgrade", name, target), nil
		}
	}

	return true, "", nil
}

// recordAgentDigest adds digest to the file at path unless it is there.
func recordAgentDigest(path, digest string) error {
	known, err := loadAgentDigests(path)
	if err != nil {
		return err
	}

	if known[digest] {
		return nil
	}

	known[digest] = true

	return writeFile(path, []byte(strings.Join(slices.Sorted(maps.Keys(known)), "\n")+"\n"), 0o600)
}

// loadAgentDigests reads the digests recorded at path. A missing file records
// none.
func loadAgentDigests(path string) (map[string]bool, error) {
	known := map[string]bool{}

	data, err := os.ReadFile(path) //nolint:gosec // The agent's own record.
	if errors.Is(err, os.ErrNotExist) {
		return known, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	for _, field := range strings.Fields(string(data)) {
		known[field] = true
	}

	return known, nil
}

func fileDigest(path string) (string, error) {
	sum, err := agentbinary.FileSHA256(path)

	return hex.EncodeToString(sum[:]), err
}
