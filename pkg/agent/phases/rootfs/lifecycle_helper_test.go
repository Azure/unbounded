// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
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

func TestEnsureNSpawnLifecycleHelperFor(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		lifecycle   *config.NSpawnLifecycleConfig
		wantInstall bool
	}{
		"hooks run the helper": {wantInstall: true},
		"hooks run configured commands": {
			lifecycle: &config.NSpawnLifecycleConfig{
				PreStartCommand:  "/usr/bin/systemctl start app-pre-start@%i.service",
				PostStartCommand: "/usr/bin/systemctl start app-post-start@%i.service",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			task, ok := EnsureNSpawnLifecycleHelperFor(&goalstates.RootFS{NSpawnLifecycle: tc.lifecycle}).(*ensureNSpawnLifecycleHelper)
			require.True(t, ok)

			var targets []string

			task.install = func(_, targetPath string) error {
				targets = append(targets, targetPath)

				return nil
			}
			require.NoError(t, task.Do(context.Background()))

			if tc.wantInstall {
				require.Equal(t, []string{goalstates.ResolveHostPaths().NSpawnLifecycleBinary}, targets)
			} else {
				require.Empty(t, targets)
			}
		})
	}
}

func TestRepairNSpawnLifecycleHelper(t *testing.T) {
	t.Parallel()

	configured := &config.NSpawnLifecycleConfig{
		PreStartCommand:  "/usr/bin/systemctl start app-pre-start@%i.service",
		PostStartCommand: "/usr/bin/systemctl start app-post-start@%i.service",
	}

	tests := map[string]struct {
		lifecycle   *config.NSpawnLifecycleConfig
		helperThere bool
		wantInstall bool
	}{
		"hooks run configured commands":                  {lifecycle: configured},
		"hooks run the helper, which is there":           {helperThere: true},
		"hooks run the helper, which is missing":         {wantInstall: true},
		"hooks run configured commands, helper is there": {lifecycle: configured, helperThere: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			target := filepath.Join(t.TempDir(), "bin", "unbounded-agent-nspawn-lifecycle")
			if tc.helperThere {
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.WriteFile(target, []byte("helper"), 0o755))
			}

			task, ok := RepairNSpawnLifecycleHelper(&goalstates.RootFS{NSpawnLifecycle: tc.lifecycle}).(*ensureNSpawnLifecycleHelper)
			require.True(t, ok)
			require.Equal(t, "repair-nspawn-lifecycle-helper", task.Name())

			var targets []string

			task.target = func() string { return target }
			task.install = func(_, targetPath string) error {
				targets = append(targets, targetPath)

				return nil
			}
			require.NoError(t, task.Do(context.Background()))

			if tc.wantInstall {
				require.Equal(t, []string{target}, targets)
			} else {
				require.Empty(t, targets)
			}
		})
	}
}
