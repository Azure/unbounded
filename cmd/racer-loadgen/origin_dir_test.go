// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckOriginDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cache string)
		err   string
	}{
		{name: "existing", setup: func(t *testing.T, cache string) {
			mkdirMode(t, cache, 0o755)
			mkdirMode(t, filepath.Join(cache, "origin"), 0o700)
		}},
		{name: "missing leaf", err: "does not exist", setup: func(t *testing.T, cache string) {
			mkdirMode(t, cache, 0o755)
		}},
		{name: "missing cache", err: "does not exist", setup: func(t *testing.T, cache string) {}},
		{name: "writable leaf", err: "unsafe", setup: func(t *testing.T, cache string) {
			mkdirMode(t, cache, 0o755)
			mkdirMode(t, filepath.Join(cache, "origin"), 0o770)
		}},
		{name: "writable parent", err: "unsafe", setup: func(t *testing.T, cache string) {
			mkdirMode(t, cache, 0o757)
			mkdirMode(t, filepath.Join(cache, "origin"), 0o700)
		}},
		{name: "symlink leaf", err: "unsafe", setup: func(t *testing.T, cache string) {
			mkdirMode(t, cache, 0o755)
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(cache, "origin")))
		}},
		{name: "file leaf", err: "unsafe", setup: func(t *testing.T, cache string) {
			mkdirMode(t, cache, 0o755)
			require.NoError(t, os.WriteFile(filepath.Join(cache, "origin"), nil, 0o600))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mkdirMode(t, filepath.Join(root, "run"), 0o755)
			mkdirMode(t, filepath.Join(root, "run", "racer"), 0o755)

			cache := filepath.Join(root, "run", "racer", "c")
			tc.setup(t, cache)

			err := checkOriginDirectory(root, "c")
			if tc.err == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tc.err)

			_, statErr := os.Lstat(filepath.Join(cache, "origin"))
			if tc.err == "does not exist" {
				require.ErrorIs(t, statErr, os.ErrNotExist, "origin must not create the directory")
			}
		})
	}
}

func TestCheckOriginDirectoryRejectsForeignLeaf(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown")
	}

	root := t.TempDir()
	leaf := filepath.Join(root, "run", "racer", "c", "origin")
	require.NoError(t, os.MkdirAll(leaf, 0o700))
	require.NoError(t, os.Chown(leaf, originUID, originUID))
	require.ErrorContains(t, checkOriginDirectory(root, "c"), "not effective UID")
}

func mkdirMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Mkdir(path, mode))
	require.NoError(t, os.Chmod(path, mode))
}
