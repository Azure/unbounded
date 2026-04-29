// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilexec"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// machinectlRunner abstracts the small set of machinectl/systemctl operations
// we need so tests can exercise the recovery path without invoking real
// binaries. The default implementation, defaultMachinectlRunner, shells out
// via utilexec.
type machinectlRunner interface {
	Start(ctx context.Context, name string) error
	Enable(ctx context.Context, name string) error
	Terminate(ctx context.Context, name string) error
	Exists(ctx context.Context, name string) bool
	ResetFailed(ctx context.Context, name string) error
}

type defaultMachinectlRunner struct {
	log *slog.Logger
}

func (r defaultMachinectlRunner) Enable(ctx context.Context, name string) error {
	return utilexec.RunCmd(ctx, r.log, utilexec.Machinectl(), "enable", name)
}

func (r defaultMachinectlRunner) Start(ctx context.Context, name string) error {
	return utilexec.RunCmd(ctx, r.log, utilexec.Machinectl(), "start", name)
}

func (r defaultMachinectlRunner) Terminate(ctx context.Context, name string) error {
	return utilexec.RunCmd(ctx, r.log, utilexec.Machinectl(), "terminate", name)
}

func (r defaultMachinectlRunner) Exists(ctx context.Context, name string) bool {
	return utilexec.RunCmd(ctx, r.log, utilexec.Machinectl(), "show", name) == nil
}

func (r defaultMachinectlRunner) ResetFailed(ctx context.Context, name string) error {
	service := fmt.Sprintf("systemd-nspawn@%s.service", name)
	return utilexec.RunCmd(ctx, r.log, utilexec.Systemctl(), "reset-failed", service)
}

type startNSpawnMachine struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart

	// runner is the machinectl/systemctl driver. Tests inject a fake.
	runner machinectlRunner
}

// StartNSpawnMachine returns a task that starts the systemd-nspawn machine using machinectl and
// waits until D-Bus is responsive inside the machine so that subsequent phases
// can safely use utilexec.MachineRun().
func StartNSpawnMachine(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &startNSpawnMachine{
		log:       log,
		goalState: goalState,
		runner:    defaultMachinectlRunner{log: log},
	}
}

func (s *startNSpawnMachine) Name() string { return "start-nspawn-machine" }

func (s *startNSpawnMachine) Do(ctx context.Context) error {
	name := s.goalState.MachineName

	if err := s.runner.Enable(ctx, name); err != nil {
		return fmt.Errorf("machinectl enable %s: %w", name, err)
	}

	if err := s.startWithRecovery(ctx, name); err != nil {
		return err
	}

	if err := waitForMachine(ctx, s.log, name); err != nil {
		return fmt.Errorf("wait for machine %s: %w", name, err)
	}

	return nil
}

// startWithRecovery runs `machinectl start` and, if it fails because the
// machine is already registered (e.g. systemd-machined was restarted out from
// under us, leaving an orphaned registration with errno 17 / "File exists"),
// terminates the stale registration and retries once.
func (s *startNSpawnMachine) startWithRecovery(ctx context.Context, name string) error {
	startErr := s.runner.Start(ctx, name)
	if startErr == nil {
		return nil
	}

	// Decide whether this looks like a stale-registration failure that we
	// can recover from. Two strong signals:
	//   - the machine is currently registered (machinectl show <name> ok),
	//   - or the error message contains "already exists" / "File exists".
	stale := s.runner.Exists(ctx, name) || isAlreadyExistsErr(startErr)
	if !stale {
		return fmt.Errorf("machinectl start %s: %w", name, startErr)
	}

	s.log.Warn("machinectl start failed with stale registration, attempting recovery",
		"machine", name, "error", startErr)

	// Clear any prior `failed` state on the unit; otherwise systemctl will
	// refuse the next start attempt. Best-effort: ignore errors.
	if err := s.runner.ResetFailed(ctx, name); err != nil {
		s.log.Debug("systemctl reset-failed returned an error (continuing)",
			"machine", name, "error", err)
	}

	if err := s.runner.Terminate(ctx, name); err != nil {
		s.log.Warn("machinectl terminate during recovery failed (continuing)",
			"machine", name, "error", err)
	}

	if !s.waitForGone(ctx, name, 15*time.Second) {
		return fmt.Errorf("machinectl start %s: stale registration did not clear after terminate (initial error: %w)",
			name, startErr)
	}

	if err := s.runner.Start(ctx, name); err != nil {
		return fmt.Errorf("machinectl start %s after recovery: %w", name, err)
	}

	s.log.Info("recovered stale nspawn registration and started machine", "machine", name)

	return nil
}

// waitForGone polls until the machine is no longer registered or the timeout
// elapses. Returns true when the machine is gone.
func (s *startNSpawnMachine) waitForGone(ctx context.Context, name string, timeout time.Duration) bool {
	const pollInterval = 500 * time.Millisecond

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !s.runner.Exists(ctx, name) {
			return true
		}

		select {
		case <-ctx.Done():
			return !s.runner.Exists(ctx, name)
		case <-time.After(pollInterval):
		}
	}

	return !s.runner.Exists(ctx, name)
}

// isAlreadyExistsErr reports whether err looks like a machinectl "machine
// already exists" failure (errno 17 / EEXIST surfaced as text).
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "already exists") || strings.Contains(msg, "file exists")
}

// waitForMachine polls the machine until it is responsive to systemd-run
// commands. machinectl start returns before D-Bus is ready, so phases that use
// utilexec.MachineRun() would fail without this gate.
func waitForMachine(ctx context.Context, log *slog.Logger, machine string) error {
	const (
		pollInterval = 500 * time.Millisecond
		timeout      = 30 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		if _, err := utilexec.MachineRun(ctx, log, machine, "/bin/true"); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			// TODO: dump machine state (e.g. machinectl status, journalctl) to aid debugging when the machine fails to start.
			return fmt.Errorf("machine %s not responsive after %s: %w", machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
