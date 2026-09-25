// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallNSpawnLifecycleHelperPreservesExistingTargetOnCopyFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "bin", "nspawn-lifecycle-helper")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("working-agent"), 0o755))

	// Opening a directory succeeds, but copying from it fails. This exercises
	// cleanup after temporary-file creation without replacing the working target.
	err := installNSpawnLifecycleHelper(dir, target)
	require.Error(t, err)

	data, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, []byte("working-agent"), data)

	temps, globErr := filepath.Glob(filepath.Join(filepath.Dir(target), ".nspawn-lifecycle-helper-*"))
	require.NoError(t, globErr)
	require.Empty(t, temps)
}

func TestInstallNSpawnLifecycleHelper(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "agent-current")
	target := filepath.Join(dir, "lib", "nspawn-lifecycle-helper")

	require.NoError(t, os.WriteFile(source, []byte("new-agent"), 0o755))

	require.NoError(t, installNSpawnLifecycleHelper(source, target))
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, []byte("new-agent"), data)

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	// Updating the daemon compatibility symlink or source later does not alter
	// the already-installed rollback-compatible helper.
	require.NoError(t, os.WriteFile(source, []byte("rolled-back-agent"), 0o755))

	data, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, []byte("new-agent"), data)
}
