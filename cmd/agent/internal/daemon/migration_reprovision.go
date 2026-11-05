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
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Reprovision resets the host and bootstraps it again in the target mode. The
// reset removes the daemon and the agent binary, so the daemon cannot run the
// bootstrap itself. Instead, before anything is reset, it stages a handoff in
// ReprovisionDir: a copy of the binary, the target configuration (the desired
// configuration with Isolation set, and the recorded attestation settings)
// and a journal. It then installs and starts the unit ReprovisionUnit, whose
// command (the hidden isolation-reprovision subcommand) runs the reset, then
// the bootstrap stage plan for the target mode, and is retried by systemd (and
// run again at boot) until it succeeds or ReprovisionGiveUpAfter has passed
// since the handoff started. The command waits longer between runs each time,
// up to ReprovisionMaxBackoff. The unit's start limit is only systemd's
// backstop, kept above what the command's own pacing allows, so the command's
// own record is what an operator finds. The daemon that bootstrap installs
// finds the journal at startup, sees the applied config in the target mode and
// reports the outcome in the Machine status.
//
// The bootstrap in mode none starts with the none backend's ownership check
// (nonePlan.EnsureHostClean), which refuses a host that still runs a kubelet
// or containerd the agent does not own. It cannot run before the reset, while
// the node being changed is such a kubelet; after the reset it is the first
// thing the bootstrap does, before anything is installed.
//
// The unit and ReprovisionDir are not touched by the reset, so a crash or a
// reboot at any point leaves the handoff to continue. When the handoff gives
// up, it records StepFailed in the journal, reports the failure in the Machine
// status when the staged credential still allows it, removes the unit and the
// staged binary and configuration, and stops. When the bootstrap was what kept
// failing, the host is reset and has no daemon, so the change is also reported
// Failed by the first daemon that starts on the host afterward (see
// settleIsolationChanges), for example after the operator runs the install
// again. When the reset itself never completed, the old node may still be
// partly in place, and the journal says so.

func (e *migrationEnv) reprovisionUnitPath() string {
	return filepath.Join(e.unitDir(), ReprovisionUnit)
}

// reprovisionStartLimitBurst is the unit's StartLimitBurst within an hour. The
// command's own pacing (ReprovisionRestartDelay plus a delay that doubles up to
// ReprovisionMaxBackoff) starts it far fewer times than this in any hour, so
// systemd's limit only stops a command that exits at once without pacing.
const reprovisionStartLimitBurst = 30

func renderReprovisionUnit(dir string) string {
	return fmt.Sprintf(`# Written by unbounded-agent for an isolation mode change that reprovisions the
# host at a repave. It resets this host and bootstraps it again in the new
# isolation mode, and is removed by the daemon that reports the outcome.
[Unit]
Description=unbounded-agent isolation reprovision
After=network-online.target
Wants=network-online.target
ConditionPathExists=%[1]s/%[2]s
StartLimitIntervalSec=3600
StartLimitBurst=%[4]d

[Service]
Type=simple
ExecStart=%[1]s/%[3]s isolation-reprovision --handoff-dir %[1]s
Restart=on-failure
RestartSec=%[5]d

[Install]
WantedBy=multi-user.target
`, dir, ReprovisionJournalName, ReprovisionBinaryName, reprovisionStartLimitBurst, int(ReprovisionRestartDelay/time.Second))
}

// migrateReprovision stages the handoff and starts the unit. Nothing is reset
// here. It returns ErrMigrationHandedOff on success. The handoff bootstraps
// the desired configuration, so a new Kubernetes version in the same repave is
// applied with the new mode.
func (e *migrationEnv) migrateReprovision(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig) error {
	target, err := desired.IsolationMode()
	if err != nil {
		return err
	}

	// The download sources travel with the configuration: the bootstrap after
	// the reset fetches from the same mirrors a repave would.
	targetCfg := &provision.UnboundedAgentConfig{AgentConfig: *desired.DeepCopy(), Downloads: desired.Downloads}

	if err := targetCfg.Validate(); err != nil {
		return fmt.Errorf("configuration for isolation mode %s is invalid: %w", target, err)
	}

	// Before any mutation.
	if err := e.preflight(ctx, log, target); err != nil {
		return err
	}

	// The bootstrap that follows the reset needs a credential. An attested host
	// attests again; any other host reuses the recorded token, which must still
	// be accepted, or the node would be reset and could not rejoin.
	attest, err := e.readAttest()
	if err != nil {
		return err
	}

	if attest == nil {
		if err := e.checkBootstrapCredential(ctx, log, &targetCfg.AgentConfig); err != nil {
			return err
		}
	}

	dir := e.reprovisionDir()

	cleanup := func(cause error) error {
		// Nothing was reset, so the handoff is thrown away.
		e.removeReprovisionFiles(context.WithoutCancel(ctx), log)

		return cause
	}

	if err := e.stageHandoff(log, dir, targetCfg, attest); err != nil {
		return cleanup(err)
	}

	// The staged binary validates the configuration, builds the bootstrap plan
	// for the mode and resolves its inputs, so an unsupported target or an
	// unusable configuration fails before anything is reset.
	if err := e.runHandoff(ctx, filepath.Join(dir, ReprovisionBinaryName), "isolation-reprovision", "--check", "--handoff-dir", dir); err != nil {
		return cleanup(fmt.Errorf("the target configuration cannot be bootstrapped: %w", err))
	}

	// From here the node is going away. The journal says so before the unit
	// can reset anything. Until the unit is installed and enabled, nothing has
	// been reset: resolveReprovision tells that case apart.
	j := migrate.Journal{Strategy: migrate.Reprovision, From: activeModeOf(active), To: target, Step: migrate.StepReprovisioning}

	if err := e.reprovisionJournal().Save(j); err != nil {
		return cleanup(err)
	}

	if err := e.installReprovisionUnit(ctx, log, dir); err != nil {
		return cleanup(err)
	}

	log.Info("reprovision handed off; the host resets and bootstraps in the new mode", "unit", ReprovisionUnit, "to", target)

	return ErrMigrationHandedOff
}

// ErrBootstrapTokenRejected is returned, before anything is reset, when the
// API server answers that it no longer accepts the bootstrap token recorded
// for the node.
var ErrBootstrapTokenRejected = errors.New("the bootstrap token recorded for this node is no longer accepted by the API server")

// defaultProbeBootstrapToken asks the API server whether it accepts the
// bootstrap token. It reports nil when the token is accepted,
// ErrBootstrapTokenRejected when the server answers 401, and any other error
// when the question could not be answered.
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
// because the recorded token has expired or been revoked. The check is made
// against the API server, so an expired token is caught when the API server
// can be reached. A token cannot be renewed from the agent: it is issued by the
// cluster, so the operator renews it and the change is tried again. Hosts that
// authenticate with an exec credential have no token to check. When the server
// cannot be asked, the reprovision goes ahead, and the handoff's bounded retry
// reports the failure if it is real.
func (e *migrationEnv) checkBootstrapCredential(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) error {
	if cfg.Kubelet.Auth.BootstrapToken == "" {
		return nil
	}

	restCfg, err := buildBootstrapRESTConfig(cfg)
	if err != nil {
		log.Warn("could not check the bootstrap token before the reprovision", "error", err)

		return nil
	}

	err = e.probeToken(ctx, restCfg)

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
func (e *migrationEnv) stageHandoff(log *slog.Logger, dir string, cfg *provision.UnboundedAgentConfig, attest *provision.AgentAttestConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	self, err := e.executable()
	if err != nil {
		return fmt.Errorf("find the running agent binary: %w", err)
	}

	if err := fsutil.InstallFile(self, filepath.Join(dir, ReprovisionBinaryName), 0o755); err != nil {
		return fmt.Errorf("copy the agent binary into %s: %w", dir, err)
	}

	staged := *cfg
	staged.Attest = attest

	data, err := json.MarshalIndent(staged, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal target configuration: %w", err)
	}

	path := filepath.Join(dir, ReprovisionConfigName)
	if err := fsutil.WriteFileDurable(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	log.Info("reprovision handoff staged", "dir", dir)

	return nil
}

// installReprovisionUnit writes the unit, reloads systemd and starts the unit
// without waiting: the unit resets the host, which includes stopping this
// daemon.
func (e *migrationEnv) installReprovisionUnit(ctx context.Context, log *slog.Logger, dir string) error {
	path := e.reprovisionUnitPath()
	if err := fsutil.WriteFileDurable(path, []byte(renderReprovisionUnit(dir)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", ReprovisionUnit},
		{"start", "--no-block", ReprovisionUnit},
	} {
		if err := e.systemctl(ctx, log, args...); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}

	return nil
}

// reprovisionUnitInstalled reports whether the unit file exists and systemd
// has it enabled, which is what a handoff that started looks like.
func (e *migrationEnv) reprovisionUnitInstalled(ctx context.Context, log *slog.Logger) bool {
	if _, err := os.Stat(e.reprovisionUnitPath()); err != nil {
		return false
	}

	return e.systemctl(ctx, log, "is-enabled", "--quiet", ReprovisionUnit) == nil
}

// removeReprovisionFiles removes the unit and the handoff directory.
func (e *migrationEnv) removeReprovisionFiles(ctx context.Context, log *slog.Logger) {
	e.removeReprovisionUnit(ctx, log)

	if err := os.RemoveAll(e.reprovisionDir()); err != nil {
		log.Warn("could not remove the reprovision directory", "dir", e.reprovisionDir(), "error", err)
	}
}

// AbandonReprovision ends a handoff that gave up: it disables and removes the
// unit and removes the staged binary and configuration, which holds the
// bootstrap token. The journal stays in dir, so the next daemon to start on the
// host reports the failure in the Machine status and then removes it.
func AbandonReprovision(ctx context.Context, log *slog.Logger, dir string) error {
	e := &migrationEnv{ReprovisionDir: dir}

	return e.abandonReprovision(ctx, log)
}

func (e *migrationEnv) abandonReprovision(ctx context.Context, log *slog.Logger) error {
	e.removeReprovisionUnit(ctx, log)

	var errs []error

	for _, name := range []string{ReprovisionBinaryName, ReprovisionConfigName} {
		if err := os.Remove(filepath.Join(e.reprovisionDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	return (&migrationEnv{}).removeReprovisionHandoff(log)
}

func (e *migrationEnv) removeReprovisionHandoff(log *slog.Logger) phases.Task {
	return hostnode.Func("remove-reprovision-handoff", func(ctx context.Context) error {
		if _, err := os.Lstat(e.reprovisionUnitPath()); err == nil {
			// A handoff that is still running must not outlive its files.
			if err := e.systemctl(ctx, log, "stop", "--no-block", ReprovisionUnit); err != nil {
				log.Warn("could not stop the reprovision unit", "error", err)
			}
		}

		e.removeReprovisionFiles(ctx, log)

		return nil
	})
}

// removeReprovisionUnit disables and removes the unit and reloads systemd.
func (e *migrationEnv) removeReprovisionUnit(ctx context.Context, log *slog.Logger) {
	if _, err := os.Lstat(e.reprovisionUnitPath()); err != nil {
		return
	}

	if err := e.systemctl(ctx, log, "disable", ReprovisionUnit); err != nil {
		log.Warn("could not disable the reprovision unit", "error", err)
	}

	if err := os.Remove(e.reprovisionUnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("could not remove the reprovision unit", "error", err)
	}

	if err := e.systemctl(ctx, log, "daemon-reload"); err != nil {
		log.Warn("could not reload systemd after removing the reprovision unit", "error", err)
	}
}

// errHandoffNeverStarted is the outcome of a reprovision whose journal was
// written but whose unit was never installed: nothing was reset.
var errHandoffNeverStarted = errors.New("the reprovision handoff never started")

// resolveReprovision is resolveJournal for a reprovision. The handoff is in
// charge: the outcome is success once the applied config records the target
// mode, and still in progress until then, as long as the unit that carries the
// handoff is installed. A journal whose unit is missing records a handoff that
// stopped where nothing can carry it on, and is concluded as a failure.
func (e *migrationEnv) resolveReprovision(ctx context.Context, log *slog.Logger, j migrate.Journal) error {
	switch j.Step {
	case migrate.StepReprovisioning, migrate.StepBootstrapping:
	default:
		return fmt.Errorf("unexpected reprovision step %q", j.Step)
	}

	store := e.reprovisionJournal()

	if e.appliedInMode(log, j.To) {
		j.Step = migrate.StepBootstrapped
		j.Message = ""

		if err := store.Save(j); err != nil {
			log.Warn("could not record that the reprovision finished", "error", err)
		}

		return nil
	}

	if e.reprovisionUnitInstalled(ctx, log) {
		return ErrMigrationHandedOff
	}

	var outcome error

	if j.Step == migrate.StepReprovisioning {
		// The daemon stopped between writing the journal and installing the
		// unit. The unit is what resets the host, so nothing was reset and the
		// node runs as before. The staged files are thrown away.
		outcome = fmt.Errorf("%w: the daemon stopped before it installed %s, so nothing was reset and the node still runs in isolation mode %s",
			errHandoffNeverStarted, ReprovisionUnit, j.From)

		e.removeReprovisionUnit(ctx, log)

		for _, name := range []string{ReprovisionBinaryName, ReprovisionConfigName} {
			if err := os.Remove(filepath.Join(e.reprovisionDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("could not remove a staged reprovision file", "file", name, "error", err)
			}
		}
	} else {
		outcome = fmt.Errorf("the reprovision to isolation mode %s reset the host, but %s is gone and the bootstrap did not finish; the node needs attention",
			j.To, ReprovisionUnit)
	}

	j.Step = migrate.StepFailed
	j.Message = outcome.Error()

	if err := store.Save(j); err != nil {
		log.Warn("could not record the failed reprovision", "error", err)
	}

	return outcome
}

// appliedInMode reports whether the host has an applied config recorded in
// mode, which is how a finished bootstrap shows.
func (e *migrationEnv) appliedInMode(log *slog.Logger, mode isolation.Mode) bool {
	for _, slot := range reprovisionSlots {
		active, err := e.readConfig(log, mode, slot)
		if err == nil && activeModeOf(active) == mode {
			return true
		}
	}

	return false
}

// reprovisionSlots are the slots a bootstrap can record a node in.
var reprovisionSlots = []string{"kube1", "kube2"}

// ReprovisionGaveUpStatus is what a handoff that gave up reports in the Machine
// status.
func ReprovisionGaveUpStatus(j migrate.Journal) *v1alpha3.IsolationChangeStatus {
	return &v1alpha3.IsolationChangeStatus{
		TargetMode:         v1alpha3.IsolationMode(j.To),
		Strategy:           v1alpha3.IsolationChangeReprovision,
		Phase:              v1alpha3.IsolationChangeFailed,
		Message:            j.Message,
		LastTransitionTime: metav1.Now(),
	}
}

// ReportReprovisionGaveUp reports a handoff that gave up in the Machine status,
// with the bootstrap token of the staged configuration cfg. The host is reset
// and has no daemon to report it, and the staged configuration, which holds
// the token, is about to be removed. A configuration without a token (an
// attested host) cannot report: the change is then reported by the first
// daemon that starts on the host, or shows as stale in machina.
func ReportReprovisionGaveUp(ctx context.Context, cfg *provision.AgentConfig, j migrate.Journal) error {
	if strings.TrimSpace(cfg.Kubelet.Auth.BootstrapToken) == "" {
		return errors.New("the staged configuration has no bootstrap token to report with")
	}

	restCfg, err := buildBootstrapRESTConfig(cfg)
	if err != nil {
		return err
	}

	c, err := client.New(restCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return patchIsolationStatus(ctx, c, cfg.MachineName, isolationStatus{Change: ReprovisionGaveUpStatus(j)})
}
