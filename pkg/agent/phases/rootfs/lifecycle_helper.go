// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

type ensureNSpawnLifecycleHelper struct {
	hostPaths goalstates.HostPaths
}

// EnsureNSpawnLifecycleHelper installs a rollback-stable lifecycle command helper.
// Agent rollback changes the daemon's current symlink but leaves this helper in
// place so already-generated nspawn hooks remain executable.
func EnsureNSpawnLifecycleHelper(hostPaths goalstates.HostPaths) phases.Task {
	return &ensureNSpawnLifecycleHelper{hostPaths: hostPaths}
}

func (e *ensureNSpawnLifecycleHelper) Name() string { return "ensure-nspawn-lifecycle-helper" }

func (e *ensureNSpawnLifecycleHelper) Do(_ context.Context) error {
	sourcePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running agent executable: %w", err)
	}

	return installNSpawnLifecycleHelper(sourcePath, e.hostPaths.NSpawnLifecycleBinary)
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

	targetDir := filepath.Dir(targetPath)

	temp, err := os.CreateTemp(targetDir, "."+filepath.Base(targetPath)+"-*")
	if err != nil {
		return fmt.Errorf("create lifecycle helper temporary file: %w", err)
	}

	tempPath := temp.Name()
	tempClosed := false

	defer func() {
		if !tempClosed {
			retErr = errors.Join(retErr, wrapCloseError(temp.Close(), tempPath))
		}

		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("remove lifecycle helper temporary file %s: %w", tempPath, err))
		}
	}()

	if _, err := io.Copy(temp, source); err != nil {
		return fmt.Errorf("copy lifecycle helper to temporary file: %w", err)
	}

	if err := temp.Chmod(0o755); err != nil {
		return fmt.Errorf("chmod lifecycle helper temporary file: %w", err)
	}

	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync lifecycle helper temporary file: %w", err)
	}

	closeErr := temp.Close()
	tempClosed = true

	if closeErr != nil {
		return fmt.Errorf("close lifecycle helper temporary file %s: %w", tempPath, closeErr)
	}

	if err := os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("replace lifecycle helper %s: %w", targetPath, err)
	}

	return nil
}

func wrapCloseError(err error, path string) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("close lifecycle helper temporary file %s: %w", path, err)
}
