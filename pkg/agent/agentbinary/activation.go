// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
	Mode          os.FileMode
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
	Service          ServicePlan
	Actions          []string
}

// ActivationResult describes a completed host activation.
type ActivationResult struct {
	PreviousPath string
	CurrentPath  string
	Initialized  bool
	Changed      bool
	RolledBack   bool
}

// PreflightHostDaemonActivation validates and plans a host-driven activation
// without changing files, links, services, or locks.
func PreflightHostDaemonActivation(
	ctx context.Context,
	opts ActivationOptions,
	service DaemonServiceInspector,
) (ActivationPlan, error) {
	if service == nil {
		return ActivationPlan{}, fmt.Errorf("daemon service is required")
	}

	if err := validateActivationOptions(opts); err != nil {
		return ActivationPlan{}, err
	}

	candidatePath, err := executablePath(opts.CandidatePath)
	if err != nil {
		return ActivationPlan{}, fmt.Errorf("resolve candidate agent binary: %w", err)
	}

	currentPath, initialized, err := activationCurrentPath(opts.Layout)
	if err != nil {
		return ActivationPlan{}, err
	}

	if !initialized && candidatePath == currentPath {
		return ActivationPlan{}, fmt.Errorf("candidate must be staged separately from the existing single-path agent binary")
	}

	changed, err := filesDiffer(candidatePath, currentPath)
	if err != nil {
		return ActivationPlan{}, fmt.Errorf("compare candidate and active agent binaries: %w", err)
	}

	plan := ActivationPlan{
		CandidatePath:    candidatePath,
		ActivePath:       currentPath,
		CurrentLinkPath:  opts.Layout.CurrentPath,
		LastGoodLinkPath: opts.Layout.LastGoodPath,
		RollbackPath:     currentPath,
		InitializeLayout: !initialized,
		CandidateChanged: changed,
	}

	if !initialized {
		plan.RollbackPath = opts.Layout.BluePath

		plan.TargetPath = opts.Layout.BluePath
		if changed {
			plan.TargetPath = opts.Layout.GreenPath
		}
	} else {
		plan.TargetPath = currentPath
		if changed {
			currentIsBlue, resolveErr := pathResolvesTo(opts.Layout.BluePath, currentPath)
			if resolveErr != nil {
				return ActivationPlan{}, fmt.Errorf("resolve blue agent binary: %w", resolveErr)
			}

			plan.TargetPath = opts.Layout.BluePath
			if currentIsBlue {
				plan.TargetPath = opts.Layout.GreenPath
			}
		}
	}

	servicePlan, err := service.Preflight(ctx, opts.Layout.CurrentPath)
	if err != nil {
		return ActivationPlan{}, fmt.Errorf("preflight daemon service: %w", err)
	}

	plan.Service = servicePlan
	plan.Actions = activationActions(plan, opts.Layout)

	return plan, nil
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
	if log == nil {
		log = slog.Default()
	}

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

	mode := opts.Mode
	if mode == 0 {
		mode = daemonBinaryMode
	}

	if plan.InitializeLayout {
		if err := installFromFile(plan.ActivePath, opts.Layout.BluePath, mode); err != nil {
			return ActivationResult{}, fmt.Errorf("preserve active agent binary: %w", err)
		}
	}

	if plan.CandidateChanged {
		protected, protectErr := symlinkReferencesPath(opts.Layout.LastGoodPath, plan.TargetPath)
		if protectErr != nil {
			return ActivationResult{}, fmt.Errorf("resolve last-good agent binary: %w", protectErr)
		}

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
		return ActivationResult{}, err
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

func validateActivationOptions(opts ActivationOptions) error {
	if err := validateLayout(opts.Layout); err != nil {
		return err
	}

	if opts.CandidatePath == "" || !filepath.IsAbs(opts.CandidatePath) || filepath.Clean(opts.CandidatePath) != opts.CandidatePath {
		return fmt.Errorf("invalid candidate agent binary path %q", opts.CandidatePath)
	}

	if opts.LockPath == "" || !filepath.IsAbs(opts.LockPath) || filepath.Clean(opts.LockPath) != opts.LockPath {
		return fmt.Errorf("invalid agent activation lock path %q", opts.LockPath)
	}

	if opts.Mode != 0 && opts.Mode.Perm() != opts.Mode {
		return fmt.Errorf("agent binary mode must contain permission bits only")
	}

	return nil
}

func activationCurrentPath(layout Layout) (string, bool, error) {
	currentPath, err := executablePath(layout.CurrentPath)
	if err == nil {
		currentIsBlue, blueErr := pathResolvesTo(layout.BluePath, currentPath)
		if blueErr != nil {
			return "", false, fmt.Errorf("resolve blue agent binary: %w", blueErr)
		}

		currentIsGreen, greenErr := pathResolvesTo(layout.GreenPath, currentPath)
		if greenErr != nil {
			return "", false, fmt.Errorf("resolve green agent binary: %w", greenErr)
		}

		return currentPath, currentIsBlue || currentIsGreen, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("resolve current agent binary: %w", err)
	}

	legacyPath, legacyErr := executablePath(layout.BinaryPath)
	if legacyErr != nil {
		return "", false, fmt.Errorf("resolve existing agent binary: %w", legacyErr)
	}

	return legacyPath, false, nil
}

func activationActions(plan ActivationPlan, layout Layout) []string {
	actions := make([]string, 0, 9)
	actions = append(actions, "verify candidate binary")

	if plan.InitializeLayout {
		actions = append(actions, "initialize managed binary layout")
	}

	if plan.CandidateChanged {
		actions = append(actions,
			"install candidate into "+plan.TargetPath,
			"preserve "+plan.RollbackPath+" as last-good",
			"switch "+layout.CurrentPath+" to "+plan.TargetPath,
		)
	}

	if plan.Service.UpdateRequired {
		actions = append(actions, "update daemon service configuration")
	}

	return append(actions,
		"reload daemon service manager",
		"restart daemon service",
		"wait for daemon health",
		"roll back to "+plan.RollbackPath+" if activation fails",
	)
}

func switchActivationLinks(layout Layout, plan ActivationPlan) error {
	if err := utilio.UpdateSymlink(layout.LastGoodPath, plan.RollbackPath); err != nil {
		return fmt.Errorf("update last-good agent symlink: %w", err)
	}

	if err := utilio.UpdateSymlink(layout.CurrentPath, plan.TargetPath); err != nil {
		return fmt.Errorf("update current agent symlink: %w", err)
	}

	if err := utilio.UpdateSymlink(layout.BinaryPath, layout.CurrentPath); err != nil {
		return fmt.Errorf("update compatibility agent symlink: %w", err)
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

func filesDiffer(firstPath, secondPath string) (bool, error) {
	first, err := fileSHA256(firstPath)
	if err != nil {
		return false, err
	}

	second, err := fileSHA256(secondPath)
	if err != nil {
		return false, err
	}

	return first != second, nil
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte

	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer file.Close() //nolint:errcheck // read error is authoritative

	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return digest, err
	}

	copy(digest[:], hasher.Sum(nil))

	return digest, nil
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
		file.Close() //nolint:errcheck // preserve lock acquisition error

		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: %w", ErrActivationInProgress, err)
		}

		return nil, fmt.Errorf("acquire host agent activation lock: %w", err)
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
