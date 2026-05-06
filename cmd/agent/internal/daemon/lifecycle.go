// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// ---------------------------------------------------------------------------
// EnableDaemon
// ---------------------------------------------------------------------------

//go:embed assets/unbounded-agent-daemon.service
var daemonServiceContent []byte

//go:embed assets/unbounded-agent-daemon-recovery.service
var daemonRecoveryServiceContent []byte

//go:embed assets/unbounded-agent-daemon-recovery.sh
var daemonRecoveryScriptContent []byte

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
	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)

	if err := writeFile(unitPath, daemonServiceContent, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	if err := writeFile(recoveryUnitPath, daemonRecoveryServiceContent, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", recoveryUnitPath, err)
	}

	if err := writeFile(goalstates.DaemonRecoveryScriptPath, daemonRecoveryScriptContent, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", goalstates.DaemonRecoveryScriptPath, err)
	}

	if err := ensureDaemonSymlinkTargets(); err != nil {
		return err
	}

	sc := executil.Systemctl()

	if err := executil.RunCmd(ctx, d.log, sc, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := executil.RunCmd(ctx, d.log, sc, "enable", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", goalstates.DaemonUnit, err)
	}

	if err := executil.RunCmd(ctx, d.log, sc, "start", goalstates.DaemonUnit); err != nil {
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
	sc := executil.Systemctl()

	if err := executil.RunCmd(ctx, t.log, sc, "stop", goalstates.DaemonUnit); err != nil {
		t.log.Warn("failed to stop daemon (may not be running)", "error", err)
	}

	if err := executil.RunCmd(ctx, t.log, sc, "disable", goalstates.DaemonUnit); err != nil {
		t.log.Warn("failed to disable daemon (may not be enabled)", "error", err)
	}

	if err := executil.RunCmd(ctx, t.log, sc, "disable", goalstates.DaemonRecoveryUnit); err != nil {
		t.log.Warn("failed to disable daemon recovery unit (may not be enabled)", "error", err)
	}

	removeFileIfExists(t.log, filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit))
	removeFileIfExists(t.log, filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit))

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
		goalstates.DaemonBinaryPath,
		goalstates.DaemonBinaryBluePath,
		goalstates.DaemonBinaryGreenPath,
		goalstates.DaemonBinaryCurrentPath,
		goalstates.DaemonBinaryLastGoodPath,
		goalstates.DaemonRecoveryScriptPath,
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

func ensureDaemonSymlinkTargets() error {
	if _, err := os.Lstat(goalstates.DaemonBinaryCurrentPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(goalstates.DaemonBinaryPath, goalstates.DaemonBinaryCurrentPath); err != nil {
			return fmt.Errorf("create current daemon binary symlink: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat current daemon binary symlink: %w", err)
	}

	currentTarget, err := resolveSymlink(goalstates.DaemonBinaryCurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	if _, err := os.Lstat(goalstates.DaemonBinaryLastGoodPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(currentTarget, goalstates.DaemonBinaryLastGoodPath); err != nil {
			return fmt.Errorf("create last-good daemon binary symlink: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat last-good daemon binary symlink: %w", err)
	}

	return nil
}
