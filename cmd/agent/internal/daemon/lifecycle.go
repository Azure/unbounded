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
	"github.com/Azure/unbounded/pkg/agent/installstate"
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
	log        *slog.Logger
	hostPrefix string
}

// EnableDaemon returns a task that installs, enables, and starts the
// unbounded-agent-daemon systemd unit on the host. The unit runs
// "unbounded-agent daemon" which watches the Machine CR for this node
// and reconciles the local state to match.
func EnableDaemon(log *slog.Logger, hostPrefix string) phases.Task {
	return &enableDaemon{log: log, hostPrefix: hostPrefix}
}

func (d *enableDaemon) Name() string { return "enable-daemon" }

func (d *enableDaemon) Do(ctx context.Context) error {
	paths, err := goalstates.ResolvedAgentUpgradePaths(d.hostPrefix)
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	if err := agentbinary.EnsureDaemonBinaryLinks(ctx, d.log, paths); err != nil {
		return err
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)

	daemonService, err := renderDaemonAssetForPaths("daemon-service", daemonServiceContent, paths)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", unitPath, err)
	}

	if err := writeFile(unitPath, daemonService, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)

	recoveryService, err := renderDaemonAssetForPaths("daemon-recovery-service", daemonRecoveryServiceContent, paths)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", recoveryUnitPath, err)
	}

	if err := writeFile(recoveryUnitPath, recoveryService, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", recoveryUnitPath, err)
	}

	recoveryScript, err := renderDaemonAssetForPaths("daemon-recovery-script", daemonRecoveryScriptContent, paths)
	if err != nil {
		return fmt.Errorf("rendering %s: %w", paths.RecoveryScriptPath, err)
	}

	if err := writeFile(paths.RecoveryScriptPath, recoveryScript, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", paths.RecoveryScriptPath, err)
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
		DaemonRecoveryScriptPath:     paths.RecoveryScriptPath,
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
// unbounded-agent-daemon systemd unit. Errors from stop and disable are
// logged but do not fail the task since the unit may not be present.
func StopDaemon(log *slog.Logger) phases.Task {
	return &stopDaemon{log: log}
}

func (t *stopDaemon) Name() string { return "stop-daemon" }

func (t *stopDaemon) Do(ctx context.Context) error {
	if err := executil.RunCmd(ctx, t.log, executil.Systemctl(), "stop", goalstates.DaemonUnit); err != nil {
		t.log.Warn("failed to stop daemon (may not be running)", "error", err)
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

// teardownHostPrefixes returns every prefix teardown must sweep.
//
// The installation record is consulted first because it is written before the
// first mutation, so it is present even when bootstrap failed before the
// applied config existed. Without it, a failed custom-prefix install falls back
// to the default and leaves its files behind while deleting the configuration
// that named them.
func teardownHostPrefixes() []string {
	var recorded string
	if rec, err := installstate.DefaultStore().Load(); err == nil {
		recorded = rec.HostPrefix
	}

	return goalstates.MergeHostPrefixes(recorded, goalstates.HostPrefixFromAppliedConfig())
}

func disableAndRemoveDaemonUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "disable", goalstates.DaemonUnit); err != nil {
		log.Warn("failed to disable daemon (may already be absent or systemd unavailable)", "error", err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)
	if err := removeOwnedFile(unitPath); err != nil {
		return err
	}

	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)
	if err := removeOwnedFile(recoveryUnitPath); err != nil {
		return err
	}

	for _, prefix := range teardownHostPrefixes() {
		if err := removeOwnedFile(goalstates.ResolveHostPaths(prefix).DaemonRecoveryScript); err != nil {
			return err
		}
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

	// Remove known file paths under every prefix the agent could have used.
	// Teardown must not depend on the applied config still being present, and a
	// host may carry files from a previous prefix.
	for _, prefix := range teardownHostPrefixes() {
		hostPaths := goalstates.ResolveHostPaths(prefix)

		paths, err := goalstates.ResolvedAgentUpgradePaths(prefix)
		if err != nil {
			return fmt.Errorf("resolve agent paths for cleanup: %w", err)
		}

		for _, path := range []string{
			paths.BinaryPath,
			paths.BluePath,
			paths.GreenPath,
			paths.CurrentPath,
			paths.LastGoodPath,
			hostPaths.NSpawnLifecycleBinary,
			hostPaths.DaemonRecoveryScript,
			hostPaths.LocalDNSNetworkHelper,
			filepath.Join(hostPaths.BinDir, "unbounded-agent-install.sh"),
			filepath.Join(hostPaths.BinDir, "unbounded-agent-uninstall.sh"),
		} {
			if path == "" {
				continue
			}

			if err := removeOwnedFile(path); err != nil {
				return err
			}
		}
	}

	// Remove directories.
	for _, dir := range []string{
		"/etc/unbounded/agent",
		"/tmp/unbounded-agent",
	} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove owned directory %s: %w", dir, err)
		}
	}

	// Remove temp config files matching /tmp/unbounded-agent-config.*.json.
	matches, _ := filepath.Glob("/tmp/unbounded-agent-config.*.json") //nolint:errcheck // Pattern is valid; only errors on malformed globs.
	for _, m := range matches {
		removeFileIfExists(t.log, m)
	}

	return nil
}

// Keep installation identity when substantive deletion fails. ENOENT alone
// means an earlier attempt already completed this removal.
func removeOwnedFile(path string) error {
	// On a read-only mount unlink may return EROFS even when the name is
	// absent. Inspect first so sweeping the legacy /usr/local prefix on ACL
	// does not turn absence into a substantive cleanup failure.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect owned file %s: %w", path, err)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove owned file %s: %w", path, err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// VerifyDaemonInstalled
// ---------------------------------------------------------------------------

// VerifyDaemonInstalled reports whether the agent daemon is actually installed
// and running on this host.
//
// This is what stops a record from vouching for itself. A record can outlive
// what it describes: an incomplete teardown, a rolled-back image, or a unit
// removed by hand all leave state claiming an installation that is not there.
// Bootstrap skipping its work on that claim is the failure the durable
// completion marker was introduced to prevent, so the claim is checked against
// systemd before it is believed.
func VerifyDaemonInstalled(ctx context.Context, log *slog.Logger) error {
	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)
	if _, err := os.Stat(unitPath); err != nil {
		return fmt.Errorf("agent daemon unit %s is not present: %w", unitPath, err)
	}

	// `systemctl is-enabled` exits non-zero for a unit that is not enabled,
	// which is the case being detected rather than an error to report.
	enabled, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "systemctl", "is-enabled", goalstates.DaemonUnit)
	if err != nil || strings.TrimSpace(enabled) != "enabled" {
		return fmt.Errorf("agent daemon unit %s is not enabled (%s)",
			goalstates.DaemonUnit, strings.TrimSpace(enabled))
	}

	active, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, "systemctl", "is-active", goalstates.DaemonUnit)
	if err != nil || strings.TrimSpace(active) != "active" {
		return fmt.Errorf("agent daemon unit %s is not active (%s)",
			goalstates.DaemonUnit, strings.TrimSpace(active))
	}

	return nil
}

// RepairDaemon uses the currently applied installation and never recreates an
// applied config from first-boot input. In particular, repave may have retired
// kube1 and advanced to kube2 since bootstrap completed.
func RepairDaemon(ctx context.Context, log *slog.Logger) error {
	if transition, err := readRepaveState(goalstates.AgentConfigDir); err != nil {
		return err
	} else if transition != nil {
		// The bootstrap coordinator already owns the installation lock.
		if err := driveRepave(ctx, log, transition); err != nil {
			return err
		}
	}

	active, err := (nspawnNodeOperator{}).FindActiveMachine(log)
	if err != nil {
		return fmt.Errorf("identify current installation for daemon repair: %w", err)
	}

	if err := EnableDaemon(log, active.Config.HostPrefix).Do(ctx); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(goalstates.HostPrefixOrDefault(active.Config.HostPrefix), goalstates.AgentConfigDir, goalstates.SystemdSystemDir)
}
