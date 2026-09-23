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

// TestEnsureNSpawnLifecycleHelperInstallsAtTheGivenTarget covers the task
// wrapper rather than the copy beneath it.
//
// The copy already had tests, but they call installNSpawnLifecycleHelper
// directly and so say nothing about where the task decides to put the file.
// That decision is the whole of this task's behavior, and a regression to a
// fixed path would install the helper somewhere the generated hook units do
// not name.
func TestEnsureNSpawnLifecycleHelperInstallsAtTheGivenTarget(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "bin", "unbounded-agent-nspawn-lifecycle")
	require.NoError(t, EnsureNSpawnLifecycleHelper(target).Do(t.Context()))

	info, err := os.Stat(target)
	require.NoError(t, err, "helper must be installed at the requested target")
	require.True(t, info.Mode().IsRegular())
	require.NotZero(t, info.Mode().Perm()&0o111, "helper must be executable")
}
