// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteNSpawnHostFileAtomicallyReplacesExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "systemd", "kube1.nspawn")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, writeNSpawnHostFile(path, []byte("new"), 0o644))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, os.SameFile(before, after))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("new"), data)
}

func TestReplaceNSpawnHostFilePreservesExistingFileOnWriteFailure(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "systemd", "kube1.nspawn")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("working"), 0o644))

	wantErr := errors.New("disk full")
	err := replaceNSpawnHostFile(path, 0o644, func(file *os.File) error {
		_, writeErr := file.Write([]byte("partial"))
		require.NoError(t, writeErr)

		return wantErr
	})
	require.ErrorIs(t, err, wantErr)

	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, []byte("working"), data)

	temps, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), ".kube1.nspawn-*"))
	require.NoError(t, globErr)
	require.Empty(t, temps)
}
