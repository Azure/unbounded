// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// Runner abstracts the small set of machinectl and systemctl operations the
// backend needs, so the recovery paths can be exercised without invoking real
// binaries. NewRunner returns the implementation that shells out via executil.
type Runner interface {
	Start(ctx context.Context, name string) error
	Enable(ctx context.Context, name string) error
	Terminate(ctx context.Context, name string) error
	Exists(ctx context.Context, name string) bool
	ResetFailed(ctx context.Context, name string) error
	Running(ctx context.Context, name string) (bool, error)
}

// NewRunner returns the Runner that drives the real machinectl and systemctl.
func NewRunner(log *slog.Logger) Runner { return execRunner{log: log} }

type execRunner struct {
	log *slog.Logger
}

func (r execRunner) Running(ctx context.Context, name string) (bool, error) {
	out, err := executil.OutputCmd(ctx, r.log, "systemctl", "show", ServiceName(name), "--property=ActiveState", "--value")
	if err != nil {
		return false, err
	}

	state := strings.TrimSpace(out)

	return state == "active" || state == "activating", nil
}

func (r execRunner) Enable(ctx context.Context, name string) error {
	return executil.RunCmd(ctx, r.log, machinectl(), "enable", name)
}

func (r execRunner) Start(ctx context.Context, name string) error {
	return executil.RunCmd(ctx, r.log, machinectl(), "start", name)
}

func (r execRunner) Terminate(ctx context.Context, name string) error {
	return executil.RunCmd(ctx, r.log, machinectl(), "terminate", name)
}

func (r execRunner) Exists(ctx context.Context, name string) bool {
	return executil.RunCmd(ctx, r.log, machinectl(), "show", name) == nil
}

func (r execRunner) ResetFailed(ctx context.Context, name string) error {
	return executil.RunCmd(ctx, r.log, executil.Systemctl(), "reset-failed", ServiceName(name))
}

// machinectl returns a command factory for machinectl.
func machinectl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "machinectl")
	}
}

// ServiceName is the systemd unit that runs the machine.
func ServiceName(machine string) string { return "systemd-nspawn@" + machine + ".service" }

// Enable enables the machine unit so the host starts it at boot.
func Enable(ctx context.Context, r Runner, name string) error {
	if err := r.Enable(ctx, name); err != nil {
		return fmt.Errorf("machinectl enable %s: %w", name, err)
	}

	return nil
}

// StartWithRecovery runs `machinectl start` and, if it fails because the
// machine is already registered (for example systemd-machined was restarted out
// from under us, leaving an orphaned registration with errno 17 / "File
// exists"), terminates the stale registration and retries once. It reports
// whether the machine was already running before the call, in which case
// nothing was started.
func StartWithRecovery(ctx context.Context, log *slog.Logger, r Runner, name string) (wasRunning bool, err error) {
	running, err := r.Running(ctx, name)
	if err != nil {
		return false, fmt.Errorf("inspect nspawn service before replay: %w", err)
	}

	if running {
		return true, nil
	}

	startErr := r.Start(ctx, name)
	if startErr == nil {
		return false, nil
	}

	// Decide whether this looks like a stale-registration failure that we
	// can recover from. Two strong signals:
	//   - the machine is currently registered (machinectl show <name> ok),
	//   - or the error message contains "already exists" / "File exists".
	stale := r.Exists(ctx, name) || IsAlreadyExists(startErr)
	if !stale {
		return false, fmt.Errorf("machinectl start %s: %w", name, startErr)
	}

	log.Warn("machinectl start failed with stale registration, attempting recovery",
		"machine", name, "error", startErr)

	// Clear any prior `failed` state on the unit; otherwise systemctl will
	// refuse the next start attempt. Best-effort: ignore errors.
	if err := r.ResetFailed(ctx, name); err != nil {
		log.Debug("systemctl reset-failed returned an error (continuing)",
			"machine", name, "error", err)
	}

	if err := r.Terminate(ctx, name); err != nil {
		log.Warn("machinectl terminate during recovery failed (continuing)",
			"machine", name, "error", err)
	}

	if !waitForGone(ctx, r, name, 15*time.Second) {
		return false, fmt.Errorf("machinectl start %s: stale registration did not clear after terminate (initial error: %w)",
			name, startErr)
	}

	if err := r.Start(ctx, name); err != nil {
		return false, fmt.Errorf("machinectl start %s after recovery: %w", name, err)
	}

	log.Info("recovered stale nspawn registration and started machine", "machine", name)

	return false, nil
}

// waitForGone polls until the machine is no longer registered or the timeout
// elapses. Returns true when the machine is gone.
func waitForGone(ctx context.Context, r Runner, name string, timeout time.Duration) bool {
	const pollInterval = 500 * time.Millisecond

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !r.Exists(ctx, name) {
			return true
		}

		select {
		case <-ctx.Done():
			return !r.Exists(ctx, name)
		case <-time.After(pollInterval):
		}
	}

	return !r.Exists(ctx, name)
}

// IsAlreadyExists reports whether err looks like a machinectl "machine already
// exists" failure (errno 17 / EEXIST surfaced as text).
func IsAlreadyExists(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "already exists") || strings.Contains(msg, "file exists")
}

// WaitForMachine polls the machine until it is responsive to systemd-run
// commands. machinectl start returns before D-Bus is ready, so phases that use
// Exec would fail without this gate.
func WaitForMachine(ctx context.Context, log *slog.Logger, machine string) error {
	const (
		pollInterval = 500 * time.Millisecond
		timeout      = 30 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		if _, err := Exec(ctx, log, machine, "/bin/true"); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("machine %s not responsive after %s: %w", machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Registered reports registration only after successful inventory. Bootstrap
// callers must not treat a missing inspection tool as a clean host.
func Registered(ctx context.Context, log *slog.Logger, name string) (bool, error) {
	names, err := registeredMachines(ctx, log)
	if err != nil {
		return false, err
	}

	_, ok := names[name]

	return ok, nil
}

// RegisteredForCleanup answers Registered's question with the tolerance
// described on MissingTool. Cleanup uses this; admission must not.
func RegisteredForCleanup(ctx context.Context, log *slog.Logger, name string) (bool, error) {
	exists, err := Registered(ctx, log, name)
	if MissingTool(err) {
		log.Warn("machine inventory tool is not installed; nothing of ours can be running", "machine", name)

		return false, nil
	}

	return exists, err
}

// FirstRegistered returns the first of slots that is registered, or the empty
// string if none is. It inventories once rather than per slot, and like
// Registered it reports an uninspectable host as an error rather than as a
// clean one.
func FirstRegistered(ctx context.Context, log *slog.Logger, slots ...string) (string, error) {
	names, err := registeredMachines(ctx, log)
	if err != nil {
		return "", err
	}

	for _, name := range slots {
		if _, ok := names[name]; ok {
			return name, nil
		}
	}

	return "", nil
}

// MissingTool reports whether err is a host inspection tool that is not
// installed, as opposed to a tool that ran and failed.
func MissingTool(err error) bool { return errors.Is(err, exec.ErrNotFound) }

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

// Stop stops the machine: it disables the unit, stops the service, waits up to
// 30 seconds for the machine to unregister, and force-terminates it if
// necessary. A host without machinectl has nothing to stop.
func Stop(ctx context.Context, log *slog.Logger, name string) error {
	// Stop the systemd service that manages the nspawn container. This
	// properly tears down mount namespaces and cgroups so that
	// machinectl remove can succeed.
	serviceName := ServiceName(name)

	if err := executil.RunCmd(ctx, log, machinectl(), "disable", name); err != nil {
		if confirmErr := ConfirmNotEnabled(ctx, log, serviceName); confirmErr != nil {
			return fmt.Errorf("disable machine %s: %w", name, errors.Join(err, confirmErr))
		}

		log.Warn("machine was not enabled; continuing with stop and removal", "machine", name, "error", err)
	}

	exists, err := RegisteredForCleanup(ctx, log, name)
	if err != nil {
		return err
	}

	if !exists {
		log.Info("machine not running, nothing to stop", "machine", name)
		return nil
	}

	log.Info("stopping nspawn machine", "machine", name)

	if !serviceIsActive(ctx, log, serviceName) {
		log.Info("nspawn service already inactive, skipping stop", "service", serviceName)
	} else if err := executil.RunCmd(ctx, log, executil.Systemctl(), "stop", serviceName); err != nil {
		log.Warn("failed to stop nspawn service", "service", serviceName, "error", err)
	}

	// Wait up to 30 seconds for the machine to fully stop.
	if gone, err := waitUnregistered(ctx, log, name, 30*time.Second); err != nil {
		return err
	} else if gone {
		return nil
	}

	// Force terminate if still registered.
	if exists, err := RegisteredForCleanup(ctx, log, name); err != nil {
		return err
	} else if exists {
		log.Warn("machine did not stop gracefully, terminating", "machine", name)

		if err := executil.RunCmd(ctx, log, machinectl(), "terminate", name); err != nil {
			log.Warn("failed to terminate machine", "machine", name, "error", err)
		}

		// Wait up to 15 seconds for the terminate to take full effect.
		if gone, err := waitUnregistered(ctx, log, name, 15*time.Second); err != nil {
			return err
		} else if !gone {
			return fmt.Errorf("machine %s remains registered after termination", name)
		}
	}

	return ctx.Err()
}

// ConfirmNotEnabled reports whether the nspawn unit is definitely not enabled.
//
// machinectl disable fails for benign reasons, most often a machine that was
// never enabled, so the failure alone does not justify aborting reset. It is
// not safe to simply ignore either: the enablement symlink outlives the config
// and rootfs that reset deletes, so a unit left enabled makes the host try to
// start a machine that no longer exists on the next boot, while reset reported
// success. Continue only when systemd positively reports a state that cannot
// start the unit at boot, and treat a failed inspection as unconfirmed.
func ConfirmNotEnabled(ctx context.Context, log *slog.Logger, service string) error {
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

// waitUnregistered polls until the machine disappears from machined or the
// timeout elapses. Returns true if the machine is gone.
func waitUnregistered(ctx context.Context, log *slog.Logger, name string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exists, err := RegisteredForCleanup(ctx, log, name); err != nil {
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

	exists, err := RegisteredForCleanup(ctx, log, name)

	return !exists, err
}

// Remove removes the machine rootfs using machinectl and falls back to
// force-removing the directory once machined no longer knows the machine.
func Remove(ctx context.Context, log *slog.Logger, name string) error {
	machineDir := MachinesDir + "/" + name

	// Skip entirely if the machine directory doesn't exist - nothing to remove.
	if _, err := os.Stat(machineDir); errors.Is(err, os.ErrNotExist) {
		log.Info("machine rootfs not present, nothing to remove", "machine", name)
		return nil
	} else if err != nil {
		return err
	}

	log.Info("removing machine rootfs", "machine", name, "dir", machineDir)

	// Retry machinectl remove with backoff. The image may briefly remain
	// "busy" after the systemd-nspawn service stops while cgroup and mount
	// teardown completes asynchronously.
	const (
		retryTimeout  = 60 * time.Second
		retryInterval = 2 * time.Second
	)

	deadline := time.Now().Add(retryTimeout)

	for time.Now().Before(deadline) {
		err := executil.RunCmd(ctx, log, machinectl(), "remove", name)
		if err == nil {
			return nil // machinectl removed both image metadata and directory
		}

		if exists, err := RegisteredForCleanup(ctx, log, name); err != nil {
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
	log.Warn("machinectl remove did not succeed, force-removing directory", "dir", machineDir)

	if exists, err := RegisteredForCleanup(ctx, log, name); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("refusing to remove registered machine %s", name)
	}

	return utilio.RemoveAllIfExists(log, machineDir)
}

// serviceIsActive returns true if the named systemd service is currently active.
func serviceIsActive(ctx context.Context, log *slog.Logger, service string) bool {
	return executil.RunCmd(ctx, log, executil.Systemctl(), "is-active", "--quiet", service) == nil
}

// ConfigPaths are the host files the backend renders for one machine.
type ConfigPaths struct {
	// NSpawnFile is the .nspawn settings file.
	NSpawnFile string
	// OverrideDir is the systemd-nspawn@<machine>.service.d drop-in directory.
	OverrideDir string
	// ConfigRegenerationUnit is the config regeneration unit file.
	ConfigRegenerationUnit string
	// BPFFSMountPath is the machine's private bpffs mount point.
	BPFFSMountPath string
}

// RemoveConfig removes the nspawn settings file, the service override
// directory and the config regeneration unit.
func RemoveConfig(log *slog.Logger, p ConfigPaths) error {
	log.Info("removing nspawn configuration", "nspawn_file", p.NSpawnFile, "override_dir", p.OverrideDir, "config_regeneration_unit", p.ConfigRegenerationUnit)

	return errors.Join(utilio.RemoveFileIfExists(log, p.NSpawnFile), utilio.RemoveAllIfExists(log, p.OverrideDir), utilio.RemoveFileIfExists(log, p.ConfigRegenerationUnit))
}

// RemoveBPFFSMount unmounts and removes the machine's private bpffs mount.
func RemoveBPFFSMount(ctx context.Context, log *slog.Logger, mountPath string) error {
	if _, err := os.Stat(mountPath); errors.Is(err, os.ErrNotExist) {
		log.Info("bpffs mount path not present, nothing to remove", "path", mountPath)
		return nil
	} else if err != nil {
		return fmt.Errorf("stat bpffs mount path %s: %w", mountPath, err)
	}

	// util-linux reserves exit 32 for a path that is not a mount point.
	// Other failures do not authorize recursive removal.
	if err := executil.RunCmdAt(ctx, log, slog.LevelDebug, executil.Mountpoint(), "-q", mountPath); err == nil {
		if err := executil.RunCmd(ctx, log, executil.Umount(), mountPath); err != nil {
			return fmt.Errorf("unmount bpffs %s: %w", mountPath, err)
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 32 {
			return fmt.Errorf("inspect bpffs mount %s: %w", mountPath, err)
		}
	}

	return utilio.RemoveAllIfExists(log, mountPath)
}
