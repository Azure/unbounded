// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type ensureNSpawnLifecycleHelper struct{}

// EnsureNSpawnLifecycleHelper installs a rollback-stable lifecycle command helper.
// Agent rollback changes the daemon's current symlink but leaves this helper in
// place so already-generated nspawn hooks remain executable.
func EnsureNSpawnLifecycleHelper() phases.Task {
	return &ensureNSpawnLifecycleHelper{}
}

func (e *ensureNSpawnLifecycleHelper) Name() string { return "ensure-nspawn-lifecycle-helper" }

func (e *ensureNSpawnLifecycleHelper) Do(_ context.Context) error {
	sourcePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running agent executable: %w", err)
	}

	return installNSpawnLifecycleHelper(sourcePath, goalstates.NSpawnLifecycleBinaryPath)
}

func installNSpawnLifecycleHelper(sourcePath, targetPath string) (retErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open lifecycle helper source %s: %w", sourcePath, err)
	}

	defer func() {
		if err := source.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close lifecycle helper source %s: %w", sourcePath, err))
		}
	}()

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create lifecycle helper directory: %w", err)
	}

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("open lifecycle helper target %s: %w", targetPath, err)
	}

	defer func() {
		if err := target.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close lifecycle helper target %s: %w", targetPath, err))
		}
	}()

	if _, err := io.Copy(target, source); err != nil {
		return fmt.Errorf("copy lifecycle helper to %s: %w", targetPath, err)
	}

	if err := target.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod lifecycle helper %s: %w", targetPath, err)
	}

	return nil
}
