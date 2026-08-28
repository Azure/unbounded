// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"text/template"

	"github.com/Azure/unbounded/internal/executil"
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

func disableAndRemoveDaemonUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "disable", goalstates.DaemonUnit); err != nil {
		log.Warn("failed to disable daemon (may already be absent or systemd unavailable)", "error", err)
	}

	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)
	removeFileIfExists(log, unitPath)

	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)
	removeFileIfExists(log, recoveryUnitPath)

	for _, prefix := range goalstates.KnownHostPrefixes(goalstates.HostPrefixFromAppliedConfig()) {
		removeFileIfExists(log, goalstates.ResolveHostPaths(prefix).DaemonRecoveryScript)
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
	for _, prefix := range goalstates.KnownHostPrefixes(goalstates.HostPrefixFromAppliedConfig()) {
		hostPaths := goalstates.ResolveHostPaths(prefix)

		paths, err := goalstates.ResolvedAgentUpgradePaths(prefix)
		if err != nil {
			t.log.Warn("resolving agent binary paths for cleanup", "prefix", prefix, "error", err)
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

			removeFileIfExists(t.log, path)
		}
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
