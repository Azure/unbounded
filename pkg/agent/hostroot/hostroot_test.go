// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testMarkers = []string{"bin/unbounded-agent", "bin/unbounded-agent-current"}

type layout struct {
	root, legacy string
}

func newLayout(t *testing.T) layout {
	t.Helper()

	dir := t.TempDir()
	l := layout{root: filepath.Join(dir, "opt", "unbounded"), legacy: filepath.Join(dir, "usr", "local")}
	require.NoError(t, os.MkdirAll(filepath.Join(l.legacy, "bin"), 0o755))

	return l
}

func touch(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o755))
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestCanonical(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	require.NoError(t, os.Symlink(real, filepath.Join(dir, "link")))

	tests := []struct {
		name, path, want string
	}{
		{name: "existing path", path: real, want: real},
		{name: "symlink is resolved", path: filepath.Join(dir, "link"), want: real},
		// Paths resolved before the directory exists must match those resolved
		// after, so the missing tail is kept under the resolved parent.
		{name: "missing tail under a symlink", path: filepath.Join(dir, "link", "unbounded", "bin"), want: filepath.Join(real, "unbounded", "bin")},
		{name: "unclean input", path: filepath.Join(dir, "link") + "/./x/", want: filepath.Join(real, "x")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, canonical(tt.path))
		})
	}
}

func TestMigrate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(t *testing.T, l layout)
		wantErr  string
		wantLink bool
		wantDir  bool
	}{
		{
			name:  "fresh host is left alone",
			setup: func(*testing.T, layout) {},
		},
		{
			name:     "legacy installation is linked",
			setup:    func(t *testing.T, l layout) { touch(t, filepath.Join(l.legacy, "bin/unbounded-agent")) },
			wantLink: true,
		},
		{
			name: "a dangling legacy link still counts as an installation",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.Symlink("missing", filepath.Join(l.legacy, "bin/unbounded-agent-current")))
			},
			wantLink: true,
		},
		{
			// The cloud-init variant writes the install script under the
			// legacy root on fresh hosts too.
			name:  "files that are not markers are not an installation",
			setup: func(t *testing.T, l layout) { touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-install.sh")) },
		},
		{
			name: "a new installation is left alone",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.root, "bin/unbounded-agent"))
			},
			wantDir: true,
		},
		{
			name: "installations under both roots are refused",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.root, "bin/unbounded-agent"))
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent"))
			},
			wantErr: "installed under both",
			wantDir: true,
		},
		{
			name: "a legacy installation beside an empty root directory is refused",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(l.root, 0o755))
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent"))
			},
			wantErr: "also exists",
			wantDir: true,
		},
		{
			name: "a migrated host stays migrated",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent"))
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(l.legacy, l.root))
			},
			wantLink: true,
		},
		{
			// An older agent's reset removes the files but not the link.
			name: "a link with no installation behind it is removed",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(l.legacy, l.root))
			},
		},
		{
			name: "a link someone else made is kept",
			setup: func(t *testing.T, l layout) {
				elsewhere := filepath.Join(filepath.Dir(l.legacy), "data")
				require.NoError(t, os.MkdirAll(elsewhere, 0o755))
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(elsewhere, l.root))
			},
			wantLink: true,
		},
		{
			name: "a file at the root is refused",
			setup: func(t *testing.T, l layout) {
				touch(t, l.root)
			},
			wantErr: "not a directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			tt.setup(t, l)

			err := migrate(discard(), l.root, l.legacy, testMarkers)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers), "migration must be idempotent")
			}

			info, err := os.Lstat(l.root)

			switch {
			case tt.wantLink:
				require.NoError(t, err)
				assert.NotZero(t, info.Mode()&os.ModeSymlink, "root must be a link")
			case tt.wantDir:
				require.NoError(t, err)
				assert.True(t, info.IsDir(), "root must stay a directory")
			case tt.wantErr == "":
				assert.ErrorIs(t, err, os.ErrNotExist, "root must not exist")
			}
		})
	}
}

// TestMigratedPathsMatchTheLegacyLayout is why the root is resolved: the
// current link a legacy agent wrote resolves to the legacy root, and a path
// built from the new root has to compare equal to it.
func TestMigratedPathsMatchTheLegacyLayout(t *testing.T) {
	t.Parallel()

	l := newLayout(t)
	blue := filepath.Join(l.legacy, "bin/unbounded-agent-blue")
	touch(t, blue)
	require.NoError(t, os.Symlink(blue, filepath.Join(l.legacy, "bin/unbounded-agent-current")))

	require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers))

	current, err := filepath.EvalSymlinks(filepath.Join(l.root, "bin/unbounded-agent-current"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonical(l.root), "bin/unbounded-agent-blue"), current)
}

func TestPlanned(t *testing.T) {
	t.Parallel()

	t.Run("fresh host", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		assert.Equal(t, canonical(l.root), planned(l.root, l.legacy, testMarkers))
		_, err := os.Lstat(l.root)
		assert.ErrorIs(t, err, os.ErrNotExist, "planning must not change the host")
	})

	t.Run("unmigrated legacy host", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		touch(t, filepath.Join(l.legacy, "bin/unbounded-agent"))
		assert.Equal(t, canonical(l.legacy), planned(l.root, l.legacy, testMarkers))
		_, err := os.Lstat(l.root)
		assert.ErrorIs(t, err, os.ErrNotExist, "planning must not change the host")
	})

	t.Run("migrated host", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		touch(t, filepath.Join(l.legacy, "bin/unbounded-agent"))
		require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers))
		assert.Equal(t, canonical(l.legacy), planned(l.root, l.legacy, testMarkers))
	})
}

// TestPrepareIgnoresTheUmask is not parallel because the umask belongs to the
// process. Parallel tests are paused while it runs.
func TestPrepareIgnoresTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)

	t.Cleanup(func() { syscall.Umask(old) })

	l := newLayout(t)
	relabeled := ""

	require.NoError(t, prepare(t.Context(), discard(), l.root, []string{"bin", "libexec"},
		func(_ context.Context, _ *slog.Logger, root string) { relabeled = root }))

	for _, dir := range []string{l.root, filepath.Join(l.root, "bin"), filepath.Join(l.root, "libexec")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), dir)
	}

	assert.Equal(t, l.root, relabeled, "new directories take their parent's SELinux label until restored")
}

func TestPrepareLeavesAMigratedHostAlone(t *testing.T) {
	t.Parallel()

	l := newLayout(t)
	touch(t, filepath.Join(l.legacy, "bin/unbounded-agent"))
	require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers))

	relabeled := false

	require.NoError(t, prepare(t.Context(), discard(), l.root, []string{"libexec"},
		func(context.Context, *slog.Logger, string) { relabeled = true }))

	_, err := os.Stat(filepath.Join(l.legacy, "libexec"))
	assert.ErrorIs(t, err, os.ErrNotExist, "the legacy root is not ours to arrange")
	assert.False(t, relabeled, "the legacy root is not ours to relabel")
}

func TestRemove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setup      func(t *testing.T, l layout)
		wantRoot   bool
		wantLegacy bool
	}{
		{name: "absent root", setup: func(*testing.T, layout) {}, wantLegacy: true},
		{
			name: "migration link is removed, and only the link",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.legacy, "bin/other-tool"))
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(l.legacy, l.root))
			},
			wantLegacy: true,
		},
		{
			name: "a link someone else made is kept",
			setup: func(t *testing.T, l layout) {
				elsewhere := filepath.Join(filepath.Dir(l.legacy), "data")
				require.NoError(t, os.MkdirAll(elsewhere, 0o755))
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(elsewhere, l.root))
			},
			wantRoot:   true,
			wantLegacy: true,
		},
		{
			name: "emptied root directory is removed",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Join(l.root, "bin"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(l.root, "libexec"), 0o755))
			},
			wantLegacy: true,
		},
		{
			name: "a root with files left in it is kept",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Join(l.root, "bin"), 0o755))
				touch(t, filepath.Join(l.root, "lib/other/keep"))
			},
			wantRoot:   true,
			wantLegacy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			tt.setup(t, l)

			require.NoError(t, remove(discard(), l.root, l.legacy))

			_, err := os.Lstat(l.root)
			assert.Equal(t, tt.wantRoot, err == nil, "root present")

			_, err = os.Stat(filepath.Join(l.legacy, "bin"))
			assert.Equal(t, tt.wantLegacy, err == nil, "legacy root untouched")
		})
	}
}

func TestRemoveKeepsNonEmptySubdirectories(t *testing.T) {
	t.Parallel()

	l := newLayout(t)
	require.NoError(t, os.MkdirAll(filepath.Join(l.root, "bin"), 0o755))
	touch(t, filepath.Join(l.root, "lib/keep"))

	require.NoError(t, remove(discard(), l.root, l.legacy))

	_, err := os.Stat(filepath.Join(l.root, "bin"))
	assert.ErrorIs(t, err, os.ErrNotExist, "an empty subdirectory is removed")
	_, err = os.Stat(filepath.Join(l.root, "lib/keep"))
	assert.NoError(t, err, "a file left in the root is kept")
}
