// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// ErrActivationInProgress indicates that another host agent activation owns
// the shared activation lock.
var ErrActivationInProgress = errors.New("host agent activation is already in progress")

// ActivationOptions configures a host-driven daemon binary activation.
type ActivationOptions struct {
	Layout        Layout
	CandidatePath string
	BinaryMode    os.FileMode
	LockPath      string
}

// ServicePlan describes service configuration work needed by an activation.
type ServicePlan struct {
	UpdateRequired bool
	Description    string
}

// DaemonServiceInspector inspects host-specific service configuration without
// changing it.
type DaemonServiceInspector interface {
	Preflight(context.Context, string) (ServicePlan, error)
}

// DaemonService adapts host-specific service management to binary activation.
type DaemonService interface {
	DaemonServiceInspector
	Prepare(context.Context, string) error
	Reload(context.Context) error
	Restart(context.Context) error
	WaitHealthy(context.Context, string) error
}

// ActivationPlan describes a host activation without changing host state.
type ActivationPlan struct {
	CandidatePath    string
	ActivePath       string
	TargetPath       string
	CurrentLinkPath  string
	LastGoodLinkPath string
	RollbackPath     string
	InitializeLayout bool
	CandidateChanged bool
	RepairLastGood   bool
	Service          ServicePlan
	Actions          []string

	previousLastGoodExists bool
	previousLastGoodTarget string
}

// ActivationResult describes a completed host activation.
type ActivationResult struct {
	PreviousPath string
	CurrentPath  string
	Initialized  bool
	Changed      bool
	RolledBack   bool
}

// ActivateHostDaemon installs the candidate, switches the daemon entrypoint,
// restarts the service, verifies health, and rolls back on restart or health
// failure.
func ActivateHostDaemon(
	ctx context.Context,
	log *slog.Logger,
	opts ActivationOptions,
	service DaemonService,
) (ActivationResult, error) {
	if err := validateActivationOptions(opts); err != nil {
		return ActivationResult{}, err
	}

	lock, err := AcquireHostActivationLock(opts.LockPath)
	if err != nil {
		return ActivationResult{}, err
	}

	defer func() {
		if err := lock.Close(); err != nil {
			log.Warn("failed to release host agent activation lock", "error", err)
		}
	}()

	plan, err := PreflightHostDaemonActivation(ctx, opts, service)
	if err != nil {
		return ActivationResult{}, err
	}

	if err := Verify(ctx, plan.CandidatePath); err != nil {
		return ActivationResult{}, fmt.Errorf("verify candidate agent binary: %w", err)
	}

	mode := opts.BinaryMode
	if mode == 0 {
		mode = daemonBinaryMode
	}

	if plan.InitializeLayout {
		if err := installFromFile(plan.ActivePath, opts.Layout.BluePath, mode); err != nil {
			return ActivationResult{}, fmt.Errorf("preserve active agent binary: %w", err)
		}
	}

	lastGoodProtected := false

	if plan.CandidateChanged {
		protected, protectErr := symlinkReferencesPath(opts.Layout.LastGoodPath, plan.TargetPath)
		if protectErr != nil {
			return ActivationResult{}, fmt.Errorf("resolve last-good agent binary: %w", protectErr)
		}

		lastGoodProtected = protected
		if protected {
			if err := utilio.UpdateSymlink(opts.Layout.LastGoodPath, plan.RollbackPath); err != nil {
				return ActivationResult{}, fmt.Errorf("protect active agent as last-good: %w", err)
			}
		}

		if err := installFromFile(plan.CandidatePath, plan.TargetPath, mode); err != nil {
			return ActivationResult{}, fmt.Errorf("install candidate agent binary: %w", err)
		}

		if err := Verify(ctx, plan.TargetPath); err != nil {
			return ActivationResult{}, fmt.Errorf("verify installed candidate agent binary: %w", err)
		}
	}

	if err := service.Prepare(ctx, opts.Layout.CurrentPath); err != nil {
		return ActivationResult{}, fmt.Errorf("prepare daemon service: %w", err)
	}

	if err := switchActivationLinks(opts.Layout, plan); err != nil {
		currentRollbackErr := utilio.UpdateSymlink(opts.Layout.CurrentPath, plan.RollbackPath)
		if currentRollbackErr != nil {
			currentRollbackErr = fmt.Errorf("restore current agent symlink after switch failure: %w", currentRollbackErr)
		}

		var lastGoodRollbackErr error
		if !lastGoodProtected {
			lastGoodRollbackErr = restoreLastGoodSymlink(opts.Layout.LastGoodPath, plan)
		}

		return ActivationResult{}, errors.Join(err, currentRollbackErr, lastGoodRollbackErr)
	}

	result := ActivationResult{
		PreviousPath: plan.ActivePath,
		CurrentPath:  plan.TargetPath,
		Initialized:  plan.InitializeLayout,
		Changed:      plan.CandidateChanged,
	}

	if err := service.Reload(ctx); err != nil {
		result.RolledBack = true
		rollbackErr := rollbackHostActivation(ctx, opts.Layout, plan, service)

		return result, errors.Join(fmt.Errorf("reload daemon service: %w", err), rollbackErr)
	}

	if err := service.Restart(ctx); err != nil {
		result.RolledBack = true
		rollbackErr := rollbackHostActivation(ctx, opts.Layout, plan, service)

		return result, errors.Join(fmt.Errorf("restart daemon service: %w", err), rollbackErr)
	}

	if err := service.WaitHealthy(ctx, plan.TargetPath); err != nil {
		result.RolledBack = true
		rollbackErr := rollbackHostActivation(ctx, opts.Layout, plan, service)

		return result, errors.Join(fmt.Errorf("wait for activated daemon health: %w", err), rollbackErr)
	}

	log.Info("activated host agent daemon binary",
		"previous", plan.ActivePath,
		"current", plan.TargetPath,
		"initialized", plan.InitializeLayout,
		"changed", plan.CandidateChanged,
	)

	return result, nil
}

func switchActivationLinks(layout Layout, plan ActivationPlan) error {
	if plan.InitializeLayout || plan.CandidateChanged || plan.RepairLastGood {
		if err := utilio.UpdateSymlink(layout.LastGoodPath, plan.RollbackPath); err != nil {
			return fmt.Errorf("update last-good agent symlink: %w", err)
		}
	}

	if plan.InitializeLayout || plan.CandidateChanged {
		if err := utilio.UpdateSymlink(layout.CurrentPath, plan.TargetPath); err != nil {
			return fmt.Errorf("update current agent symlink: %w", err)
		}
	}

	if layout.BinaryPath != "" {
		if err := utilio.UpdateSymlink(layout.BinaryPath, layout.CurrentPath); err != nil {
			return fmt.Errorf("update compatibility agent symlink: %w", err)
		}
	}

	return nil
}

func restoreLastGoodSymlink(path string, plan ActivationPlan) error {
	if plan.previousLastGoodExists {
		if err := utilio.UpdateSymlink(path, plan.previousLastGoodTarget); err != nil {
			return fmt.Errorf("restore last-good agent symlink: %w", err)
		}

		return nil
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove newly created last-good agent symlink: %w", err)
	}

	return nil
}

func rollbackHostActivation(ctx context.Context, layout Layout, plan ActivationPlan, service DaemonService) error {
	if err := utilio.UpdateSymlink(layout.CurrentPath, plan.RollbackPath); err != nil {
		return fmt.Errorf("roll back current agent symlink: %w", err)
	}

	if err := service.Restart(ctx); err != nil {
		return fmt.Errorf("restart rolled-back daemon service: %w", err)
	}

	if err := service.WaitHealthy(ctx, plan.RollbackPath); err != nil {
		return fmt.Errorf("wait for rolled-back daemon health: %w", err)
	}

	return nil
}

// HostActivationLock is an exclusive lock shared by host-driven and
// MachineOperation-driven agent activation paths.
type HostActivationLock struct {
	file *os.File
}

// AcquireHostActivationLock acquires the shared, nonblocking host activation
// lock. The caller must close the returned lock.
func AcquireHostActivationLock(path string) (*HostActivationLock, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("invalid agent activation lock path %q", path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create agent activation lock directory: %w", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open agent activation lock: %w", err)
	}

	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := file.Close()
		lockErr := errors.Join(err, closeErr)

		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: %w", ErrActivationInProgress, lockErr)
		}

		return nil, fmt.Errorf("acquire host agent activation lock: %w", lockErr)
	}

	return &HostActivationLock{file: file}, nil
}

// Close releases the host activation lock.
func (l *HostActivationLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil

	return errors.Join(unlockErr, closeErr)
}
