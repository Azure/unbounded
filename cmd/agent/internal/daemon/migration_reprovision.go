// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/renameio/v2"
	"k8s.io/client-go/rest"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Reprovision resets the host and bootstraps it again in the target mode. The
// reset removes the daemon and the agent binary, so the daemon cannot run the
// bootstrap itself. Instead, before anything is reset, it stages a handoff in
// ReprovisionDir: a copy of the binary, the target configuration (the applied
// configuration with Isolation set) and a journal. It then installs and starts
// the unit ReprovisionUnit, whose command (the hidden isolation-reprovision
// subcommand) runs the existing reset, then the bootstrap stage plan for the
// target mode, and is retried by systemd (and run again at boot) until it
// succeeds. Two limits apply and are different: the command itself gives up
// after ReprovisionMaxAttempts failed runs (5) and records why in the journal,
// and the unit's StartLimitBurst (10 in an hour) is only systemd's backstop,
// kept higher so the command's own record is what an operator finds. The daemon
// that bootstrap installs finds the journal at startup, sees the applied config
// in the target mode and reports the outcome in the Machine status.
//
// The unit and ReprovisionDir are not touched by the reset, so a crash or a
// reboot at any point leaves the handoff to continue. When the runs are used
// up, the handoff records StepFailed in the journal, removes the unit and the
// staged binary and configuration, and stops. When the bootstrap was what kept
// failing, the host is reset and has no daemon, so the change is reported
// Failed by the first daemon that starts on the host afterward (see
// settleIsolationChanges), for example after the operator runs the install
// script again. When the reset itself never completed, the old node may still
// be partly in place, and the journal says so.

// Seams for the host-mutating parts, replaced in tests.
var (
	// runHandoffCommand runs the staged binary, for its --check.
	runHandoffCommand = func(ctx context.Context, bin string, args ...string) error {
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput() //nolint:gosec // the binary is the agent's own staged copy
		if err != nil {
			return fmt.Errorf("%s %v: %w: %s", bin, args, err, out)
		}

		return nil
	}

	// systemctlForMigration runs systemctl.
	systemctlForMigration = func(ctx context.Context, log *slog.Logger, args ...string) error {
		return executil.RunCmd(ctx, log, executil.Systemctl(), args...)
	}

	// agentExecutable is the running agent binary, copied into the handoff.
	agentExecutable = os.Executable
)

func reprovisionUnitPath() string { return filepath.Join(reprovisionUnitDir(), ReprovisionUnit) }

func renderReprovisionUnit(dir string) string {
	return fmt.Sprintf(`# Written by unbounded-agent for an isolation mode change that reprovisions the
# host at a repave. It resets this
# host and bootstraps it again in the new isolation mode, and is removed by the
# daemon that reports the outcome.
[Unit]
Description=unbounded-agent isolation reprovision
After=network-online.target
Wants=network-online.target
ConditionPathExists=%[1]s/%[2]s
StartLimitIntervalSec=3600
StartLimitBurst=10

[Service]
Type=simple
ExecStart=%[1]s/%[3]s isolation-reprovision --handoff-dir %[1]s
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
`, dir, ReprovisionJournalName, ReprovisionBinaryName)
}

// migrateReprovision stages the handoff and starts the unit.
// Nothing is reset here. It returns ErrMigrationHandedOff on success. The
// handoff bootstraps the desired configuration, so a new Kubernetes version in
// the same repave is applied with the new mode.
func migrateReprovision(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig) error {
	target, err := desired.IsolationMode()
	if err != nil {
		return err
	}

	targetCfg := desired.DeepCopy()

	// The toolkit location describes the host install that only mode none uses.
	if target != isolation.ModeNone {
		targetCfg.NvidiaToolkit = nil
	}

	if err := targetCfg.Validate(); err != nil {
		return fmt.Errorf("configuration for isolation mode %s is invalid: %w", target, err)
	}

	// Before any mutation.
	if err := preflightMigrationTarget(ctx, log, target); err != nil {
		return err
	}

	// The bootstrap that follows the reset needs a credential. An attested host
	// attests again; any other host reuses the recorded token, which must still
	// be accepted, or the node would be reset and could not rejoin.
	attest, err := readAttestRecord()
	if err != nil {
		return err
	}

	if attest == nil {
		if err := checkBootstrapCredential(ctx, log, targetCfg); err != nil {
			return err
		}
	}

	dir := reprovisionDir()
	store := reprovisionJournalFor()

	j := migrate.Journal{Strategy: migrate.Reprovision, From: active.Mode, To: target}

	cleanup := func(cause error) error {
		// Nothing was reset, so the handoff is thrown away.
		removeReprovisionFiles(log)

		return cause
	}

	if err := stageHandoff(log, dir, targetCfg, attest); err != nil {
		return cleanup(err)
	}

	// The staged binary validates the configuration and that a bootstrap plan
	// exists for the mode, so an unsupported target fails before anything is reset.
	if err := runHandoffCommand(ctx, filepath.Join(dir, ReprovisionBinaryName), "isolation-reprovision", "--check", "--handoff-dir", dir); err != nil {
		return cleanup(fmt.Errorf("the target configuration cannot be bootstrapped: %w", err))
	}

	// From here the node is going away. The journal says so before the unit
	// can reset anything.
	j.Step = migrate.StepReprovisioning

	if err := store.Save(j); err != nil {
		return cleanup(err)
	}

	if err := installReprovisionUnit(ctx, log, dir); err != nil {
		return cleanup(err)
	}

	log.Info("reprovision handed off; the host resets and bootstraps in the new mode", "unit", ReprovisionUnit, "to", target)

	return ErrMigrationHandedOff
}

// ErrBootstrapTokenRejected is returned, before anything is reset, when the
// bootstrap token recorded for the node is no longer accepted.
var ErrBootstrapTokenRejected = errors.New("the bootstrap token recorded for this node is no longer accepted by the API server")

// probeBootstrapToken asks the API server whether it accepts the bootstrap
// token. It reports nil when the token is accepted, ErrBootstrapTokenRejected
// when the server answers 401, and any other error when the question could not
// be answered.
var probeBootstrapToken = defaultProbeBootstrapToken

func defaultProbeBootstrapToken(ctx context.Context, cfg *rest.Config) error {
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(cfg.Host, "/")+"/version", nil)
	if err != nil {
		return err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close() //nolint:errcheck // the status is all that is read

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrBootstrapTokenRejected
	}

	return nil
}

// checkBootstrapCredential refuses a reprovision that could not bootstrap again
// because the recorded token has expired or been revoked. A token cannot be
// renewed from the agent: it is issued by the cluster, so the operator renews
// it and retries. Hosts that authenticate with an exec credential have no token
// to check. When the server cannot be asked, the reprovision goes ahead, and
// the bounded retry of the handoff reports the failure if it is real.
func checkBootstrapCredential(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) error {
	if cfg.Kubelet.Auth.BootstrapToken == "" {
		return nil
	}

	restCfg, err := buildBootstrapRESTConfig(cfg)
	if err != nil {
		log.Warn("could not check the bootstrap token before the reprovision", "error", err)

		return nil
	}

	err = probeBootstrapToken(ctx, restCfg)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrBootstrapTokenRejected):
		return fmt.Errorf("%w; a reprovision would reset the node and it could not rejoin the cluster. "+
			"Renew the token, or bootstrap the host with attestation, and the change is tried again at the next repave", err)
	default:
		log.Warn("could not check the bootstrap token before the reprovision; continuing", "error", err)

		return nil
	}
}

// stageHandoff writes the binary copy and target configuration.
func stageHandoff(log *slog.Logger, dir string, cfg *provision.AgentConfig, attest *provision.AgentAttestConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	self, err := agentExecutable()
	if err != nil {
		return fmt.Errorf("find the running agent binary: %w", err)
	}

	if err := fsutil.InstallFile(self, filepath.Join(dir, ReprovisionBinaryName), 0o755); err != nil {
		return fmt.Errorf("copy the agent binary into %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(provision.UnboundedAgentConfig{AgentConfig: *cfg, Attest: attest}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal target configuration: %w", err)
	}

	path := filepath.Join(dir, ReprovisionConfigName)
	if err := renameio.WriteFile(path, data, 0o600, renameio.WithTempDir(dir)); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	log.Info("reprovision handoff staged", "dir", dir)

	return nil
}

// installReprovisionUnit writes the unit, reloads systemd and starts the unit
// without waiting: the unit resets the host, which includes stopping this
// daemon.
func installReprovisionUnit(ctx context.Context, log *slog.Logger, dir string) error {
	path := reprovisionUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	if err := renameio.WriteFile(path, []byte(renderReprovisionUnit(dir)), 0o644, renameio.WithTempDir(filepath.Dir(path))); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", ReprovisionUnit},
		{"start", "--no-block", ReprovisionUnit},
	} {
		if err := systemctlForMigration(ctx, log, args...); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}

	return nil
}

// removeReprovisionFiles removes the unit and the handoff directory.
func removeReprovisionFiles(log *slog.Logger) {
	removeReprovisionUnit(context.Background(), log)

	if err := os.RemoveAll(reprovisionDir()); err != nil {
		log.Warn("could not remove the reprovision directory", "dir", reprovisionDir(), "error", err)
	}
}

// AbandonReprovision ends a handoff that gave up: it disables and removes the
// unit and removes the staged binary and configuration, which holds the
// bootstrap token. The journal stays in dir, so the next daemon to start on the
// host reports the failure in the Machine status and then removes it.
func AbandonReprovision(ctx context.Context, log *slog.Logger, dir string) error {
	removeReprovisionUnit(ctx, log)

	var errs []error

	for _, name := range []string{ReprovisionBinaryName, ReprovisionConfigName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// RemoveReprovisionHandoff is the part of an agent reset that removes what a
// reprovision left behind: the unit, the staged binary and configuration, and
// the journal. Without it, a reset after a handoff that gave up would leave a
// unit and a copy of the bootstrap token on the host. The handoff's own reset
// does not include it, because it runs from that directory.
func RemoveReprovisionHandoff(log *slog.Logger) phases.Task {
	return hostnode.Func("remove-reprovision-handoff", func(ctx context.Context) error {
		if _, err := os.Lstat(reprovisionUnitPath()); err == nil {
			// A handoff that is still running must not outlive its files.
			if err := systemctlForMigration(ctx, log, "stop", "--no-block", ReprovisionUnit); err != nil {
				log.Warn("could not stop the reprovision unit", "error", err)
			}
		}

		removeReprovisionFiles(log)

		return nil
	})
}

// removeReprovisionUnit disables and removes the unit and reloads systemd.
func removeReprovisionUnit(ctx context.Context, log *slog.Logger) {
	if _, err := os.Lstat(reprovisionUnitPath()); err != nil {
		return
	}

	if err := systemctlForMigration(ctx, log, "disable", ReprovisionUnit); err != nil {
		log.Warn("could not disable the reprovision unit", "error", err)
	}

	if err := os.Remove(reprovisionUnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("could not remove the reprovision unit", "error", err)
	}

	if err := systemctlForMigration(ctx, log, "daemon-reload"); err != nil {
		log.Warn("could not reload systemd after removing the reprovision unit", "error", err)
	}
}

// resolveReprovision is resolveJournal for a reprovision. The
// handoff is in charge: the outcome is success once the applied config records
// the target mode, and still in progress until then.
func resolveReprovision(_ context.Context, log *slog.Logger, j migrate.Journal) error {
	switch j.Step {
	case migrate.StepReprovisioning, migrate.StepBootstrapping:
		if !appliedInMode(log, j.To) {
			return ErrMigrationHandedOff
		}

		j.Step = migrate.StepBootstrapped
		if err := reprovisionJournalFor().Save(j); err != nil {
			log.Warn("could not record that the reprovision finished", "error", err)
		}

		return nil
	default:
		return fmt.Errorf("unexpected reprovision step %q", j.Step)
	}
}

// appliedInMode reports whether the host has an applied config recorded in
// mode, which is how a finished bootstrap shows.
func appliedInMode(log *slog.Logger, mode isolation.Mode) bool {
	for _, slot := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		active, err := readMigrationConfig(log, slot)
		if err == nil && active.Mode == mode {
			return true
		}
	}

	return false
}
