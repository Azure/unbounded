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
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
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
	paths, err := goalstates.ResolvedAgentUpgradePathsFor(goalstates.HostPrefixFromAppliedConfig())
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

	return activateDaemonUnit(ctx, d.log, executil.Systemctl())
}

// activateDaemonUnit reloads, enables and starts the daemon unit.
//
// It is separate from writing the unit files so the command sequence can be
// exercised without a writable /etc, and because the order matters: see the
// reset-failed step below.
func activateDaemonUnit(ctx context.Context, log *slog.Logger, sc func(context.Context) *exec.Cmd) error {
	if err := executil.RunCmd(ctx, log, sc, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := executil.RunCmd(ctx, log, sc, "enable", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", goalstates.DaemonUnit, err)
	}

	// Clear any start-limit failure before starting. systemd refuses to start a
	// unit that exhausted StartLimitBurst until the failure is reset, and that
	// applies to manual starts too, so without this a retry cannot recover a
	// host whose daemon was already rate-limited into failure.
	//
	// Tolerated when host policy denies it: SELinux can withhold this from the
	// caller, and it unblocks a start rather than being required for one.
	if err := executil.RunCmd(ctx, log, sc, "reset-failed", goalstates.DaemonUnit); err != nil {
		log.Debug("could not reset daemon unit failure state", "unit", goalstates.DaemonUnit, "error", err)
	}

	if err := executil.RunCmd(ctx, log, sc, "start", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", goalstates.DaemonUnit, err)
	}

	log.Info("daemon unit started", "unit", goalstates.DaemonUnit)

	return nil
}

// InstallBootstrapBinary installs the staged bootstrap executable unless the
// host already has a usable daemon binary. The caller holds installation
// ownership; existing binary layouts are retained and upgrades use their normal
// activation path.
func InstallBootstrapBinary() error {
	if usableDaemonBinary(goalstates.DaemonBinaryPath) {
		return nil
	}

	source, err := os.Executable()
	if err != nil {
		return err
	}

	return fsutil.InstallFile(source, goalstates.DaemonBinaryPath, 0o755)
}

// usableDaemonBinary resolves symlinks on purpose. The healthy layout reaches
// the active slot through a symlink chain, so only the target tells us whether
// the host can actually run the daemon. A dangling link, or one aimed at
// something that is not an executable file, is exactly the state that sends a
// completed install into repair, and repair cannot replace a bad link either.
// Treating the link's mere presence as a usable binary would strand the host.
func usableDaemonBinary(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func renderDaemonAsset(name string, content []byte) ([]byte, error) {
	paths, err := goalstates.ResolvedAgentUpgradePathsFor(goalstates.HostPrefixFromAppliedConfig())
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
		DaemonDeferredExitCode       int
	}{
		DaemonUnit:                   goalstates.DaemonUnit,
		DaemonRecoveryUnit:           goalstates.DaemonRecoveryUnit,
		DaemonBinaryCurrentPath:      paths.CurrentPath,
		DaemonBinaryLastGoodPath:     paths.LastGoodPath,
		DaemonRecoveryScriptPath:     goalstates.DaemonRecoveryScriptPath,
		DaemonAgentUpgradeSignalPath: paths.SignalPath,
		DaemonDeferredExitCode:       DeferredExitCode,
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
// unbounded-agent-daemon systemd unit. Only an absent unit permits a failed
// stop; substantive service errors must retain reset ownership.
func StopDaemon(log *slog.Logger) phases.Task {
	return &stopDaemon{log: log}
}

func (t *stopDaemon) Name() string { return "stop-daemon" }

func (t *stopDaemon) Do(ctx context.Context) error {
	if err := executil.RunCmd(ctx, t.log, executil.Systemctl(), "stop", goalstates.DaemonUnit); err != nil {
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

type removeFirstBootUnit struct {
	log *slog.Logger
}

// RemoveFirstBootBootstrapUnit returns a task that disables and removes the
// unit an Ignition config installs to bootstrap the agent.
func RemoveFirstBootBootstrapUnit(log *slog.Logger) phases.Task {
	return &removeFirstBootUnit{log: log}
}

func (t *removeFirstBootUnit) Name() string { return "remove-first-boot-unit" }

func (t *removeFirstBootUnit) Do(ctx context.Context) error {
	return removeFirstBootBootstrapUnit(ctx, t.log)
}

// removeFirstBootBootstrapUnit disables and removes the unit an Ignition config
// installs to bootstrap the agent.
//
// Reset has to take this with it. The unit is installed into
// multi-user.target and carries no completion condition, so it runs on every
// boot and relies on the agent's ownership record to decide there is nothing to
// do. Reset removes that record, so a unit left behind would find a host with
// no installation and bootstrap it again, undoing the reset on the next boot.
//
// Absent on every host not provisioned through Ignition, which is the common
// case, so a missing unit is success rather than something to report.
func removeFirstBootBootstrapUnit(ctx context.Context, log *slog.Logger) error {
	return removeFirstBootBootstrapUnitIn(ctx, log, goalstates.SystemdSystemDir)
}

// removeFirstBootBootstrapUnitIn takes the unit directory so the sequence can
// be exercised without writing to /etc.
func removeFirstBootBootstrapUnitIn(ctx context.Context, log *slog.Logger, unitDir string) error {
	unitPath := filepath.Join(unitDir, goalstates.FirstBootBootstrapUnit)

	if _, err := os.Lstat(unitPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	log.Info("removing first-boot bootstrap unit", "unit", goalstates.FirstBootBootstrapUnit)

	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "disable", goalstates.FirstBootBootstrapUnit); err != nil {
		// Disable removes the enablement symlink. If it failed but the unit
		// file is already gone, there is nothing left to start.
		if _, statErr := os.Lstat(unitPath); !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("disable %s: %w", goalstates.FirstBootBootstrapUnit, err)
		}
	}

	return removeOwnedFile(unitPath)
}

func disableAndRemoveDaemonUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "disable", goalstates.DaemonUnit); err != nil {
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

// VerifyDaemonInstalled checks installed daemon assets and service state. An
// active daemon already proves it resolved an applied config at startup, so the
// applied-config check belongs to RepairDaemon rather than here.
func VerifyDaemonInstalled(ctx context.Context, log *slog.Logger) error {
	paths, err := goalstates.ResolvedAgentUpgradePathsFor(goalstates.HostPrefixFromAppliedConfig())
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

	return fsutil.SyncFilesystems("/usr/local", goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
