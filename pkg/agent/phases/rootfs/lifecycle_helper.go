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
	name          string
	skip          bool
	onlyIfMissing bool
	target        func() string
	install       func(sourcePath, targetPath string) error
}

func newEnsureNSpawnLifecycleHelper(name string) *ensureNSpawnLifecycleHelper {
	return &ensureNSpawnLifecycleHelper{
		name:    name,
		target:  func() string { return goalstates.ResolveHostPaths().NSpawnLifecycleBinary },
		install: installNSpawnLifecycleHelper,
	}
}

// EnsureNSpawnLifecycleHelper installs a rollback-stable lifecycle command helper.
// Agent rollback changes the daemon's current symlink but leaves this helper in
// place so already-generated nspawn hooks remain executable. For a machine whose
// config may supply the hooks' commands, use EnsureNSpawnLifecycleHelperFor.
func EnsureNSpawnLifecycleHelper() phases.Task {
	return newEnsureNSpawnLifecycleHelper("ensure-nspawn-lifecycle-helper")
}

// EnsureNSpawnLifecycleHelperFor is EnsureNSpawnLifecycleHelper for a machine.
// It installs nothing when the machine's hooks run configured commands instead
// of the helper.
func EnsureNSpawnLifecycleHelperFor(goalState *goalstates.RootFS) phases.Task {
	task := newEnsureNSpawnLifecycleHelper("ensure-nspawn-lifecycle-helper")
	task.skip = !goalState.UsesLifecycleHelper()

	return task
}

// RepairNSpawnLifecycleHelper installs the lifecycle helper if the machine's
// hooks run it and it is missing. EnsureNSpawnConfig runs it before writing the
// hooks, so a host whose config stops supplying the hooks' commands, which then
// installs no helper, gets the helper back rather than hooks that cannot run.
func RepairNSpawnLifecycleHelper(goalState *goalstates.RootFS) phases.Task {
	task := newEnsureNSpawnLifecycleHelper("repair-nspawn-lifecycle-helper")
	task.skip = !goalState.UsesLifecycleHelper()
	task.onlyIfMissing = true

	return task
}

func (e *ensureNSpawnLifecycleHelper) Name() string { return e.name }

func (e *ensureNSpawnLifecycleHelper) Do(_ context.Context) error {
	if e.skip {
		return nil
	}

	targetPath := e.target()

	if e.onlyIfMissing {
		_, err := os.Stat(targetPath)
		if err == nil {
			return nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check lifecycle helper %s: %w", targetPath, err)
		}
	}

	sourcePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running agent executable: %w", err)
	}

	return e.install(sourcePath, targetPath)
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
