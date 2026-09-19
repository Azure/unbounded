// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
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
	// Stop the systemd service that manages the nspawn container. This
	// properly tears down mount namespaces and cgroups so that
	// machinectl remove can succeed.
	serviceName := fmt.Sprintf("systemd-nspawn@%s.service", t.machineName)

	if err := executil.RunCmd(ctx, t.log, executil.Machinectl(), "disable", t.machineName); err != nil {
		if confirmErr := confirmNotEnabled(ctx, t.log, serviceName); confirmErr != nil {
			return fmt.Errorf("disable machine %s: %w", t.machineName, errors.Join(err, confirmErr))
		}

		t.log.Warn("machine was not enabled; continuing with stop and removal", "machine", t.machineName, "error", err)
	}

	exists, err := registeredMachineForCleanup(ctx, t.log, t.machineName)
	if err != nil {
		return err
	}

	if !exists {
		t.log.Info("machine not running, nothing to stop", "machine", t.machineName)
		return nil
	}

	t.log.Info("stopping nspawn machine", "machine", t.machineName)

	if !serviceIsActive(ctx, t.log, serviceName) {
		t.log.Info("nspawn service already inactive, skipping stop", "service", serviceName)
	} else if err := executil.RunCmd(ctx, t.log, executil.Systemctl(), "stop", serviceName); err != nil {
		t.log.Warn("failed to stop nspawn service", "service", serviceName, "error", err)
	}

	// Wait up to 30 seconds for the machine to fully stop.
	if gone, err := t.waitForGone(ctx, 30*time.Second); err != nil {
		return err
	} else if gone {
		return nil
	}

	// Force terminate if still registered.
	if exists, err := registeredMachineForCleanup(ctx, t.log, t.machineName); err != nil {
		return err
	} else if exists {
		t.log.Warn("machine did not stop gracefully, terminating", "machine", t.machineName)

		if err := executil.RunCmd(ctx, t.log, executil.Machinectl(), "terminate", t.machineName); err != nil {
			t.log.Warn("failed to terminate machine", "machine", t.machineName, "error", err)
		}

		// Wait up to 15 seconds for the terminate to take full effect.
		if gone, err := t.waitForGone(ctx, 15*time.Second); err != nil {
			return err
		} else if !gone {
			return fmt.Errorf("machine %s remains registered after termination", t.machineName)
		}
	}

	return ctx.Err()
}

// confirmNotEnabled reports whether the nspawn unit is definitely not enabled.
//
// machinectl disable fails for benign reasons, most often a machine that was
// never enabled, so the failure alone does not justify aborting reset. It is
// not safe to simply ignore either: the enablement symlink outlives the config
// and rootfs that reset deletes, so a unit left enabled makes the host try to
// start a machine that no longer exists on the next boot, while reset reported
// success. Continue only when systemd positively reports a state that cannot
// start the unit at boot, and treat a failed inspection as unconfirmed.
func confirmNotEnabled(ctx context.Context, log *slog.Logger, service string) error {
	// show exits zero whatever the state, unlike is-enabled, so the reported
	// state is the signal rather than the exit status.
	out, err := executil.OutputCmd(ctx, log, "systemctl", "show", service, "--property=UnitFileState", "--value")
	if err != nil {
		return fmt.Errorf("inspect %s enablement: %w", service, err)
	}

	// An empty state means no unit file is installed, so nothing is enabled.
	switch state := strings.TrimSpace(out); state {
	case "", "disabled", "not-found", "masked", "masked-runtime", "static", "indirect":
		return nil
	default:
		return fmt.Errorf("%s is %s", service, state)
	}
}

// waitForGone polls machineExists until the machine disappears or the timeout
// elapses. Returns true if the machine is gone.
func (t *stopMachine) waitForGone(ctx context.Context, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exists, err := registeredMachineForCleanup(ctx, t.log, t.machineName); err != nil {
			return false, err
		} else if !exists {
			return true, nil
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	exists, err := registeredMachineForCleanup(ctx, t.log, t.machineName)

	return !exists, err
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

	// Skip entirely if the machine directory doesn't exist - nothing to remove.
	if _, err := os.Stat(machineDir); errors.Is(err, os.ErrNotExist) {
		t.log.Info("machine rootfs not present, nothing to remove", "machine", t.machineName)
		return nil
	} else if err != nil {
		return err
	}

	t.log.Info("removing machine rootfs", "machine", t.machineName, "dir", machineDir)

	// Retry machinectl remove with backoff. The image may briefly remain
	// "busy" after the systemd-nspawn service stops while cgroup and mount
	// teardown completes asynchronously.
	const (
		retryTimeout  = 60 * time.Second
		retryInterval = 2 * time.Second
	)

	deadline := time.Now().Add(retryTimeout)

	for time.Now().Before(deadline) {
		err := executil.RunCmd(ctx, t.log, executil.Machinectl(), "remove", t.machineName)
		if err == nil {
			return nil // machinectl removed both image metadata and directory
		}

		if exists, err := registeredMachineForCleanup(ctx, t.log, t.machineName); err != nil {
			return err
		} else if !exists {
			// Once machined no longer knows the machine, the nspawn service is stopped
			// and the rootfs can be deleted directly. Some host configurations can still
			// make machinectl remove fail at this point. Fedora with SELinux enforcing,
			// for example, can deny systemd-machined permission to create its nspawn
			// image lock file under /run/systemd/nspawn/locks, returning "Access denied"
			// even though the machine is already gone.
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}

	// Fallback: force-remove the directory if machinectl keeps failing.
	t.log.Warn("machinectl remove did not succeed, force-removing directory", "dir", machineDir)

	if exists, err := registeredMachineForCleanup(ctx, t.log, t.machineName); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("refusing to remove registered machine %s", t.machineName)
	}

	return removeAllIfExists(t.log, machineDir)
}

// RegisteredMachine reports registration only after successful inventory.
// Bootstrap callers must not treat a missing inspection tool as a clean host.
func RegisteredMachine(ctx context.Context, log *slog.Logger, name string) (bool, error) {
	names, err := registeredMachines(ctx, log)
	if err != nil {
		return false, err
	}

	_, ok := names[name]

	return ok, nil
}

// FirstRegisteredMachine returns the name of the first node slot that is
// registered, or the empty string if neither is. It inventories once rather
// than per slot, and like RegisteredMachine it reports an uninspectable host as
// an error rather than as a clean one.
func FirstRegisteredMachine(ctx context.Context, log *slog.Logger) (string, error) {
	names, err := registeredMachines(ctx, log)
	if err != nil {
		return "", err
	}

	for _, name := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		if _, ok := names[name]; ok {
			return name, nil
		}
	}

	return "", nil
}

// registeredMachines inventories machinectl once and returns the registered
// names as a set.
func registeredMachines(ctx context.Context, log *slog.Logger) (map[string]struct{}, error) {
	out, err := executil.OutputCmd(ctx, log, "machinectl", "list", "--no-legend", "--no-pager")
	if err != nil {
		return nil, fmt.Errorf("inspect registered machines: %w", err)
	}

	names := make(map[string]struct{})

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			names[fields[0]] = struct{}{}
		}
	}

	return names, nil
}

// serviceIsActive returns true if the named systemd service is currently active.
func serviceIsActive(ctx context.Context, log *slog.Logger, service string) bool {
	err := executil.RunCmd(ctx, log, executil.Systemctl(), "is-active", "--quiet", service)
	return err == nil
}
