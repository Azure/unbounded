// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// ---------------------------------------------------------------------------
// EnableDaemon
// ---------------------------------------------------------------------------

//go:embed assets/unbounded-agent-daemon.service
var daemonServiceContent []byte

type enableDaemon struct {
	log *slog.Logger
}

// EnableDaemon returns a task that installs, enables, and starts the
// unbounded-agent-daemon systemd unit on the host. The unit runs
// "unbounded-agent daemon" which watches the Machine CR for this node
// and reconciles the local state to match.
func EnableDaemon(log *slog.Logger) phases.Task {
	return &enableDaemon{log: log}
}

func (d *enableDaemon) Name() string { return "enable-daemon" }

func (d *enableDaemon) Do(ctx context.Context) error {
	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)

	if err := writeFile(unitPath, daemonServiceContent, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	sc := systemctl()

	if err := runCmd(ctx, d.log, sc, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := runCmd(ctx, d.log, sc, "enable", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", goalstates.DaemonUnit, err)
	}

	if err := runCmd(ctx, d.log, sc, "start", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", goalstates.DaemonUnit, err)
	}

	d.log.Info("daemon unit started", "unit", goalstates.DaemonUnit)

	return nil
}

// ---------------------------------------------------------------------------
// StopDaemon
// ---------------------------------------------------------------------------

type stopDaemon struct {
	log *slog.Logger
}

// StopDaemon returns a task that stops, disables, and removes the
// unbounded-agent-daemon systemd unit. Errors from stop and disable are
// logged but do not fail the task since the unit may not be present.
func StopDaemon(log *slog.Logger) phases.Task {
	return &stopDaemon{log: log}
}

func (t *stopDaemon) Name() string { return "stop-daemon" }

func (t *stopDaemon) Do(ctx context.Context) error {
	sc := systemctl()

	if err := runCmd(ctx, t.log, sc, "stop", goalstates.DaemonUnit); err != nil {
		t.log.Warn("failed to stop daemon (may not be running)", "error", err)
	}

	if err := runCmd(ctx, t.log, sc, "disable", goalstates.DaemonUnit); err != nil {
		t.log.Warn("failed to disable daemon (may not be enabled)", "error", err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)
	removeFileIfExists(t.log, unitPath)

	return nil
}

// ---------------------------------------------------------------------------
// RemoveAgentArtifacts
// ---------------------------------------------------------------------------

type removeAgentArtifacts struct {
	log *slog.Logger
}

// RemoveAgentArtifacts returns a task that removes the agent binary, install
// script, legacy uninstall script, config directory, and temp files.
func RemoveAgentArtifacts(log *slog.Logger) phases.Task {
	return &removeAgentArtifacts{log: log}
}

func (t *removeAgentArtifacts) Name() string { return "remove-agent-artifacts" }

func (t *removeAgentArtifacts) Do(_ context.Context) error {
	t.log.Info("removing agent binaries and configuration")

	// Remove known file paths.
	for _, path := range []string{
		"/usr/local/bin/unbounded-agent",
		"/usr/local/bin/unbounded-agent-install.sh",
		"/usr/local/bin/unbounded-agent-uninstall.sh",
	} {
		removeFileIfExists(t.log, path)
	}

	// Remove directories.
	for _, dir := range []string{
		"/etc/unbounded/agent",
		"/tmp/unbounded-agent",
	} {
		removeAllIfExists(t.log, dir)
	}

	// Remove temp config files matching /tmp/unbounded-agent-config.*.json.
	matches, _ := filepath.Glob("/tmp/unbounded-agent-config.*.json") //nolint:errcheck // Pattern is valid; only errors on malformed globs.
	for _, m := range matches {
		removeFileIfExists(t.log, m)
	}

	return nil
}
