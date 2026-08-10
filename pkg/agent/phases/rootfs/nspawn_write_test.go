// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteNSpawnHostFilePreservesExistingInode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "systemd", "kube1.nspawn")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, writeNSpawnHostFile(path, []byte("new"), 0o644))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("new"), data)
}
