// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

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
