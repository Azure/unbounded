package reset

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilexec"
)

type stopMachine struct {
	log         *slog.Logger
	machineName string
}

// StopMachine returns a task that stops the nspawn machine, waits for it to
// fully stop (up to 30 seconds), and force-terminates it if necessary.
func StopMachine(log *slog.Logger, machineName string) phases.Task {
	return &stopMachine{log: log, machineName: machineName}
}

func (t *stopMachine) Name() string { return "stop-machine" }

func (t *stopMachine) Do(ctx context.Context) error {
	if !machineExists(ctx, t.log, t.machineName) {
		t.log.Info("machine not running, nothing to stop", "machine", t.machineName)
		return nil
	}

	t.log.Info("stopping nspawn machine", "machine", t.machineName)

	// Attempt graceful stop.
	_ = utilexec.RunCmd(ctx, t.log, utilexec.Machinectl(), "stop", t.machineName)

	// Wait up to 30 seconds for the machine to fully stop.
	if t.waitForGone(ctx, 30*time.Second) {
		return nil
	}

	// Force terminate if still running.
	if machineExists(ctx, t.log, t.machineName) {
		t.log.Warn("machine did not stop gracefully, terminating", "machine", t.machineName)
		_ = utilexec.RunCmd(ctx, t.log, utilexec.Machinectl(), "terminate", t.machineName)

		// Wait up to 15 seconds for the terminate to take full effect.
		t.waitForGone(ctx, 15*time.Second)
	}

	return ctx.Err()
}

// waitForGone polls machineExists until the machine disappears or the timeout
// elapses. Returns true if the machine is gone.
func (t *stopMachine) waitForGone(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !machineExists(ctx, t.log, t.machineName) {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}

	return !machineExists(ctx, t.log, t.machineName)
}

type removeMachine struct {
	log         *slog.Logger
	machineName string
}

// RemoveMachine returns a task that removes the machine rootfs using
// machinectl and then force-removes the directory.
func RemoveMachine(log *slog.Logger, machineName string) phases.Task {
	return &removeMachine{log: log, machineName: machineName}
}

func (t *removeMachine) Name() string { return "remove-machine" }

func (t *removeMachine) Do(ctx context.Context) error {
	machineDir := fmt.Sprintf("/var/lib/machines/%s", t.machineName)

	t.log.Info("removing machine rootfs", "machine", t.machineName, "dir", machineDir)

	// Wait for the machine to be fully gone before attempting removal.
	// machinectl remove will fail with "Device or resource busy" if the
	// machine's processes haven't fully terminated.
	const waitTimeout = 30 * time.Second

	deadline := time.Now().Add(waitTimeout)
	for machineExists(ctx, t.log, t.machineName) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	// Try machinectl remove first.
	_ = utilexec.RunCmd(ctx, t.log, utilexec.Machinectl(), "remove", t.machineName)

	return removeAllIfExists(machineDir)
}

// machineExists checks whether the named nspawn machine is known to machinectl.
func machineExists(ctx context.Context, log *slog.Logger, name string) bool {
	err := utilexec.RunCmd(ctx, log, utilexec.Machinectl(), "show", name)
	return err == nil
}
