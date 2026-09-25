// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package agentbinary installs and switches agent binaries from release archives.
package agentbinary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

const verifyTimeout = 30 * time.Second

const daemonBinaryMode os.FileMode = 0o755

func installFromFile(sourcePath, targetPath string, perm os.FileMode) (err error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}

	defer func() {
		if closeErr := source.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", sourcePath, closeErr)
		}
	}()

	if err := utilio.InstallFile(targetPath, source, perm); err != nil {
		return fmt.Errorf("install %s to %s: %w", sourcePath, targetPath, err)
	}

	return nil
}

// EnsureDaemonBinaryLinks initializes daemon current, last-good, and
// compatibility binary links.
func EnsureDaemonBinaryLinks(ctx context.Context, log *slog.Logger, paths goalstates.AgentUpgradePaths) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	currentTarget := paths.CurrentTargetPath
	// Resolved rather than stat'd, matching the last-good check below. Lstat
	// succeeds on a symlink whose target is gone, so a dangling current link
	// read as healthy and was left alone. VerifyDaemonInstalled resolves it and
	// fails, which made the link the one fault that verify could report and
	// repair could not fix: start returned the same stat error forever.
	if _, err := filepath.EvalSymlinks(paths.CurrentPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve current daemon binary symlink: %w", err)
		}

		target, targetErr := initialDaemonBinaryTarget(paths)
		if targetErr != nil {
			return fmt.Errorf("no executable agent binary found for daemon link initialization: %w", targetErr)
		}

		if err := utilio.UpdateSymlink(paths.CurrentPath, target); err != nil {
			return fmt.Errorf("initialize current daemon symlink: %w", err)
		}

		currentTarget = target
	}

	if _, err := filepath.EvalSymlinks(paths.LastGoodPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve last-good daemon binary symlink: %w", err)
		}

		if err := utilio.UpdateSymlink(paths.LastGoodPath, currentTarget); err != nil {
			return fmt.Errorf("initialize last-good daemon symlink: %w", err)
		}
	}

	if currentTarget != paths.BinaryPath {
		// Do not replace the compatibility path when the current symlink
		// already resolves to that path. That preserves legacy installs and
		// avoids creating a BinaryPath -> CurrentPath -> BinaryPath loop.
		if err := utilio.UpdateSymlink(paths.BinaryPath, paths.CurrentPath); err != nil {
			return fmt.Errorf("initialize daemon compatibility symlink: %w", err)
		}
	}

	log.Info("daemon binary links initialized",
		"current", paths.CurrentPath,
		"last_good", paths.LastGoodPath,
	)

	return nil
}

func initialDaemonBinaryTarget(paths goalstates.AgentUpgradePaths) (string, error) {
	target, err := paths.InitialDaemonBinaryTarget()
	if err != nil {
		return "", err
	}

	if target != paths.BinaryPath {
		return target, nil
	}

	if err := installFromFile(paths.BinaryPath, paths.BluePath, daemonBinaryMode); err != nil {
		return "", err
	}

	return paths.BluePath, nil
}

// Verify runs the installed agent binary's version command, and checks that it
// uses the same host root as this agent.
func Verify(ctx context.Context, path string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	for {
		err := exec.CommandContext(verifyCtx, path, "version").Run()
		if err == nil {
			return verifyHostRoot(verifyCtx, path, hostroot.Resolve())
		}

		if errors.Is(err, syscall.ETXTBSY) {
			select {
			case <-verifyCtx.Done():
				return fmt.Errorf("verify agent binary %s: %w", path, err)
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		return fmt.Errorf("verify agent binary %s: %w", path, err)
	}
}

// verifyHostRoot refuses an agent that would look for its files somewhere
// other than root. An agent released before the host root looks under the
// legacy root, so on a host installed under the new one it would find nothing,
// and on a writable /usr it would start a second layout there. On a host that
// still uses the legacy root there is nothing to check: every agent finds it.
func verifyHostRoot(ctx context.Context, path, root string) error {
	if root == hostroot.LegacyPath {
		return nil
	}

	out, err := exec.CommandContext(ctx, path, "host-root").Output()
	if err != nil {
		return fmt.Errorf("verify agent binary %s: it predates the host root %s and cannot run on this host: %w", path, root, err)
	}

	if got := strings.TrimSpace(string(out)); got != root {
		return fmt.Errorf("verify agent binary %s: it uses the host root %s, but this host uses %s", path, got, root)
	}

	return nil
}
