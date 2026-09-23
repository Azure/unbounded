// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveFileIfExists(t *testing.T) {
	t.Parallel()

	log := slog.Default()

	t.Run("file exists", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test-file")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

		removeFileIfExists(log, path)

		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("file does not exist", func(t *testing.T) {
		t.Parallel()

		// Should not panic or error.
		removeFileIfExists(log, filepath.Join(t.TempDir(), "nonexistent-file"))
	})
}

func TestRemoveAllIfExists(t *testing.T) {
	t.Parallel()

	log := slog.Default()

	t.Run("directory exists", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "subdir")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o644))

		removeAllIfExists(log, dir)

		_, statErr := os.Stat(dir)
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("path does not exist", func(t *testing.T) {
		t.Parallel()

		// Should not panic or error.
		removeAllIfExists(log, filepath.Join(t.TempDir(), "nonexistent-dir"))
	})
}

// TestRemoveIfExistsSkipsAbsentPaths covers reset on a host with a read-only
// /usr. Reset sweeps the default prefix too, and unlinking a missing path under
// a read-only mount returns EROFS, not ENOENT. The remove call must not happen
// at all for an absent path. A test that only tolerated the error would pass
// against the bug, because an unwritable directory returns ENOENT instead.
func TestRemoveIfExistsSkipsAbsentPaths(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	absent := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	called := false
	remove := func(string) error {
		called = true
		return syscall.EROFS
	}

	require.NoError(t, removeIfExists(log, "/usr/local/libexec/unbounded-localdns-network", "file", absent, remove))
	assert.False(t, called, "an absent path must not be unlinked")
}

// TestRemoveIfExistsReportsFailures keeps the tolerance narrow: a path that is
// present and cannot be removed is still an error.
func TestRemoveIfExistsReportsFailures(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	present := func(string) (os.FileInfo, error) { return nil, nil } //nolint:nilnil // Only presence is read.

	err := removeIfExists(log, "/usr/local/bin/x", "file", present, func(string) error { return syscall.EROFS })
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.EROFS)
}

// TestRemoveFileIfExistsRemovesDanglingSymlink pins Lstat over Stat. A dangling
// link is still a file reset has to remove.
func TestRemoveFileIfExistsRemovesDanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing"), link))

	require.NoError(t, removeFileIfExists(slog.New(slog.DiscardHandler), link))

	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
