// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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

	lastGoodExists, lastGoodTarget, err := readSymlinkState(opts.Layout.LastGoodPath)
	if err != nil {
		return ActivationPlan{}, fmt.Errorf("inspect last-good agent symlink: %w", err)
	}

	plan.previousLastGoodExists = lastGoodExists
	plan.previousLastGoodTarget = lastGoodTarget

	if !plan.InitializeLayout && !plan.CandidateChanged {
		if _, err := executablePath(opts.Layout.LastGoodPath); errors.Is(err, os.ErrNotExist) {
			plan.RepairLastGood = true
		} else if err != nil {
			return ActivationPlan{}, fmt.Errorf("resolve last-good agent binary: %w", err)
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

	if opts.BinaryMode != 0 && opts.BinaryMode.Perm() != opts.BinaryMode {
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

	for _, fallbackPath := range []string{
		layout.LastGoodPath,
		layout.BluePath,
		layout.GreenPath,
		layout.BinaryPath,
	} {
		fallback, fallbackErr := executablePath(fallbackPath)
		if fallbackErr == nil {
			return fallback, false, nil
		}

		if !errors.Is(fallbackErr, os.ErrNotExist) {
			return "", false, fmt.Errorf("resolve fallback agent binary %s: %w", fallbackPath, fallbackErr)
		}
	}

	return "", false, fmt.Errorf("no executable existing agent binary found")
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

	if plan.RepairLastGood {
		actions = append(actions, "repair last-good agent symlink")
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

func readSymlinkState(path string) (bool, string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}

	if err != nil {
		return false, "", err
	}

	if info.Mode()&os.ModeSymlink == 0 {
		return false, "", fmt.Errorf("%s is not a symlink", path)
	}

	target, err := os.Readlink(path)
	if err != nil {
		return false, "", err
	}

	return true, target, nil
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
