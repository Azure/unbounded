// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/bootstrap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
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
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	if err := agentbinary.EnsureDaemonBinaryLinks(ctx, d.log, paths); err != nil {
		return err
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)

	daemonService, err := renderDaemonAsset("daemon-service", daemonServiceContent)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", unitPath, err)
	}

	if err := writeFile(unitPath, daemonService, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)

	recoveryService, err := renderDaemonAsset("daemon-recovery-service", daemonRecoveryServiceContent)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", recoveryUnitPath, err)
	}

	if err := writeFile(recoveryUnitPath, recoveryService, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", recoveryUnitPath, err)
	}

	recoveryScript, err := renderDaemonAsset("daemon-recovery-script", daemonRecoveryScriptContent)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", goalstates.DaemonRecoveryScriptPath, err)
	}

	if err := writeFile(goalstates.DaemonRecoveryScriptPath, recoveryScript, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", goalstates.DaemonRecoveryScriptPath, err)
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

// InstallBootstrapBinary installs the staged bootstrap executable if the host
// has no daemon binary yet. The caller holds installation ownership; existing
// binary layouts are retained and upgrades use their normal activation path.
func InstallBootstrapBinary() error {
	if _, err := os.Lstat(goalstates.DaemonBinaryPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	source, err := os.Executable()
	if err != nil {
		return err
	}

	return installBinary(source, goalstates.DaemonBinaryPath)
}

func renderDaemonAsset(name string, content []byte) ([]byte, error) {
	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return nil, err
	}

	return renderDaemonAssetForPaths(name, content, paths)
}

func renderDaemonAssetForPaths(name string, content []byte, paths goalstates.AgentUpgradePaths) ([]byte, error) {
	data := struct {
		DaemonUnit                   string
		DaemonRecoveryUnit           string
		DaemonBinaryCurrentPath      string
		DaemonBinaryLastGoodPath     string
		DaemonRecoveryScriptPath     string
		DaemonAgentUpgradeSignalPath string
	}{
		DaemonUnit:                   goalstates.DaemonUnit,
		DaemonRecoveryUnit:           goalstates.DaemonRecoveryUnit,
		DaemonBinaryCurrentPath:      paths.CurrentPath,
		DaemonBinaryLastGoodPath:     paths.LastGoodPath,
		DaemonRecoveryScriptPath:     goalstates.DaemonRecoveryScriptPath,
		DaemonAgentUpgradeSignalPath: paths.SignalPath,
	}

	tmpl, err := template.New(name).Parse(string(content))
	if err != nil {
		return nil, err
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return nil, err
	}

	return rendered.Bytes(), nil
}

// ---------------------------------------------------------------------------
// StopDaemon
// ---------------------------------------------------------------------------

type stopDaemon struct {
	log *slog.Logger
}

// StopDaemon returns a task that stops, disables, and removes the
// unbounded-agent-daemon systemd unit. Offline hosts and absent units permit
// cleanup; substantive service errors on a running systemd remain failures.
func StopDaemon(log *slog.Logger) phases.Task {
	return &stopDaemon{log: log}
}

func (t *stopDaemon) Name() string { return "stop-daemon" }

func (t *stopDaemon) Do(ctx context.Context) error {
	if err := executil.RunCmd(ctx, t.log, executil.Systemctl(), "stop", goalstates.DaemonUnit); err != nil && !reset.SystemdUnavailable() {
		state, inspectErr := executil.OutputCmd(ctx, t.log, "systemctl", "show", goalstates.DaemonUnit, "--property=LoadState", "--value")
		if inspectErr != nil || strings.TrimSpace(state) != "not-found" {
			return fmt.Errorf("stop daemon: %w", err)
		}
	}

	return disableAndRemoveDaemonUnit(ctx, t.log)
}

// ---------------------------------------------------------------------------
// RemoveDaemonUnit
// ---------------------------------------------------------------------------

type removeDaemonUnit struct {
	log *slog.Logger
}

// RemoveDaemonUnit returns a task that disables and removes the
// unbounded-agent-daemon systemd unit without stopping the running service.
func RemoveDaemonUnit(log *slog.Logger) phases.Task {
	return &removeDaemonUnit{log: log}
}

func (t *removeDaemonUnit) Name() string { return "remove-daemon-unit" }

func (t *removeDaemonUnit) Do(ctx context.Context) error {
	return disableAndRemoveDaemonUnit(ctx, t.log)
}

func disableAndRemoveDaemonUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "disable", goalstates.DaemonUnit); err != nil && !reset.SystemdUnavailable() {
		if _, statErr := os.Lstat(filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)); !errors.Is(statErr, os.ErrNotExist) {
			return err
		}
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)
	if err := removeOwnedFile(unitPath); err != nil {
		return err
	}

	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)
	if err := removeOwnedFile(recoveryUnitPath); err != nil {
		return err
	}

	if err := removeOwnedFile(goalstates.DaemonRecoveryScriptPath); err != nil {
		return err
	}

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
		goalstates.NSpawnLifecycleBinaryPath,
		goalstates.DaemonRecoveryScriptPath,
		"/usr/local/bin/unbounded-agent-install.sh",
		"/usr/local/bin/unbounded-agent-uninstall.sh",
	} {
		if err := removeOwnedFile(path); err != nil {
			return err
		}
	}

	// Remove directories.
	for _, dir := range []string{
		"/etc/unbounded/agent",
		"/tmp/unbounded-agent",
	} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}

	// Remove temp config files matching /tmp/unbounded-agent-config.*.json.
	matches, _ := filepath.Glob("/tmp/unbounded-agent-config.*.json") //nolint:errcheck // Pattern is valid; only errors on malformed globs.
	for _, m := range matches {
		if err := removeOwnedFile(m); err != nil {
			return err
		}
	}

	return nil
}

func removeOwnedFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove owned artifact %s: %w", path, err)
	}

	return nil
}

func VerifyDaemonInstalled(ctx context.Context, log *slog.Logger) error {
	if _, err := (nspawnNodeOperator{}).FindActiveMachine(log); err != nil {
		return err
	}

	paths, err := goalstates.ResolvedAgentUpgradePaths()
	if err != nil {
		return err
	}

	for _, name := range []string{goalstates.DaemonUnit, goalstates.DaemonRecoveryUnit} {
		if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, name)); err != nil {
			return err
		}
	}

	for _, path := range []string{paths.CurrentPath, paths.LastGoodPath, paths.BinaryPath, goalstates.DaemonRecoveryScriptPath} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("daemon binary is not executable: %s", path)
		}
	}

	for _, check := range []string{"is-enabled", "is-active"} {
		out, err := executil.OutputCmd(ctx, log, "systemctl", check, goalstates.DaemonUnit)

		want := "active"
		if check == "is-enabled" {
			want = "enabled"
		}

		if err != nil {
			return fmt.Errorf("daemon %s check: %w", check, err)
		}

		if strings.TrimSpace(out) != want {
			return fmt.Errorf("daemon %s check failed: %s", check, out)
		}
	}

	return nil
}

// RepairDaemon requires the caller's installation lock. It uses current applied
// configuration, never the original bootstrap input that may name a retired slot.
func RepairDaemon(ctx context.Context, log *slog.Logger) error {
	if _, err := (nspawnNodeOperator{}).FindActiveMachine(log); err != nil {
		return err
	}

	if err := InstallBootstrapBinary(); err != nil {
		return err
	}

	if err := EnableDaemon(log).Do(ctx); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems("/usr/local", goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
