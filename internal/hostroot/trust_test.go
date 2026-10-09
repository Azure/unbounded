// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckPath walks paths under the test's temporary directory, which the
// user running the test owns, so it trusts that user as well as root.
// TestCheckOwner covers a directory owned by someone untrusted.
func TestCheckPath(t *testing.T) {
	t.Parallel()

	trusted := []uint32{0, uint32(os.Getuid())} //nolint:gosec // A uid fits in uint32.

	tests := []struct {
		name string
		// setup lays out dir and returns the path to check.
		setup   func(t *testing.T, dir string) string
		wantErr string
	}{
		{
			name: "a missing path under trusted directories",
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "opt/unbounded/agent")
			},
		},
		{
			name: "an existing path under trusted directories",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "opt/unbounded/agent"), 0o755))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
		},
		{
			name: "a parent the group can write to",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "opt/unbounded"), 0o775)

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "opt/unbounded can be written to by group or others (mode 0775)",
		},
		{
			name: "an ancestor others can write to",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "opt"), 0o757)

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "opt can be written to by group or others",
		},
		{
			name: "the path itself others can write to",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "opt/unbounded/agent"), 0o777)

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "agent can be written to",
		},
		{
			name: "a sticky directory with a trusted entry in it",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "shared"), 0o777|os.ModeSticky)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "shared/mine/agent"), 0o755))

				return filepath.Join(dir, "shared/mine/agent")
			},
		},
		{
			// Anyone could make it first.
			name: "a sticky directory without the entry",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "shared"), 0o777|os.ModeSticky)

				return filepath.Join(dir, "shared/missing/agent")
			},
			wantErr: "shared can be written to",
		},
		{
			name: "a link to a trusted directory",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "data/agent"), 0o755))
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "opt/unbounded"), 0o755))
				require.NoError(t, os.Symlink(filepath.Join(dir, "data/agent"), filepath.Join(dir, "opt/unbounded/agent")))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
		},
		{
			name: "a link to a directory others can write to",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "data/agent"), 0o777)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "opt/unbounded"), 0o755))
				require.NoError(t, os.Symlink(filepath.Join(dir, "data/agent"), filepath.Join(dir, "opt/unbounded/agent")))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "data/agent can be written to",
		},
		{
			name: "a relative link along the way into a directory the group can write to",
			setup: func(t *testing.T, dir string) string {
				mkdirChmod(t, filepath.Join(dir, "data"), 0o775)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "opt"), 0o755))
				require.NoError(t, os.Symlink("../data", filepath.Join(dir, "opt/unbounded")))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "data can be written to",
		},
		{
			// The directory the link is in decides who can replace the link.
			name: "a link in a directory others can write to",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "data/agent"), 0o755))
				mkdirChmod(t, filepath.Join(dir, "opt/unbounded"), 0o777)
				require.NoError(t, os.Symlink(filepath.Join(dir, "data/agent"), filepath.Join(dir, "opt/unbounded/agent")))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "opt/unbounded can be written to",
		},
		{
			name: "a link loop",
			setup: func(t *testing.T, dir string) string {
				require.NoError(t, os.Symlink(filepath.Join(dir, "b"), filepath.Join(dir, "a")))
				require.NoError(t, os.Symlink(filepath.Join(dir, "a"), filepath.Join(dir, "b")))

				return filepath.Join(dir, "a/agent")
			},
			wantErr: "too many levels of symbolic links",
		},
		{
			name: "a file along the way",
			setup: func(t *testing.T, dir string) string {
				touch(t, filepath.Join(dir, "opt"))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
			wantErr: "is not a directory",
		},
		{
			name: "a file at the end",
			setup: func(t *testing.T, dir string) string {
				touch(t, filepath.Join(dir, "opt/unbounded/agent"))

				return filepath.Join(dir, "opt/unbounded/agent")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := tt.setup(t, dir)

			err := checkPath(path, trusted)
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.wantErr)

			if tt.wantErr != "too many levels of symbolic links" && tt.wantErr != "is not a directory" {
				require.ErrorIs(t, err, errUntrusted)
			}
		})
	}

	require.ErrorContains(t, checkPath("relative/agent", trusted), "not an absolute path")
}

// TestCheckOwner covers a directory none of the trusted users owns: whatever
// its mode, its owner can change it.
func TestCheckOwner(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	info, err := os.Lstat(dir)
	require.NoError(t, err)

	me := uint32(os.Getuid()) //nolint:gosec // A uid fits in uint32.
	require.NoError(t, checkOwner(dir, info, []uint32{me}))

	err = checkOwner(dir, info, []uint32{me + 1})
	require.ErrorIs(t, err, errUntrusted)
	require.ErrorContains(t, err, "is owned by uid")
}

func TestCheckRoot(t *testing.T) {
	t.Parallel()

	t.Run("a root linked to a legacy root the group can write to", func(t *testing.T) {
		t.Parallel()

		// As /usr/local is root:staff 2775 on some distributions.
		l := newLayout(t)
		require.NoError(t, os.Chmod(l.legacy, 0o775))
		require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
		require.NoError(t, os.Symlink(l.legacy, l.root))

		require.NoError(t, checkRoot(l.root, l.legacy))
	})

	t.Run("a root linked to the legacy root in a parent others can write to", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		mkdirChmod(t, filepath.Dir(l.root), 0o777)
		require.NoError(t, os.Symlink(l.legacy, l.root))

		require.ErrorIs(t, checkRoot(l.root, l.legacy), errUntrusted)
	})

	t.Run("a link an operator made into a directory the group can write to", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		elsewhere := filepath.Join(filepath.Dir(l.legacy), "data")
		mkdirChmod(t, elsewhere, 0o775)
		require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
		require.NoError(t, os.Symlink(elsewhere, l.root))

		require.ErrorIs(t, checkRoot(l.root, l.legacy), errUntrusted)
	})
}

// TestMigrateRefusesARootOthersCanReplace covers the host shapes Migrate meets
// with a directory on the way to the root that others can write to. It refuses
// each, fresh hosts included, and changes nothing.
func TestMigrateRefusesARootOthersCanReplace(t *testing.T) {
	t.Parallel()

	for name, setup := range map[string]func(t *testing.T, l layout){
		"a fresh host": func(t *testing.T, l layout) {
			mkdirChmod(t, filepath.Dir(l.root), 0o777)
		},
		"a legacy installation": func(t *testing.T, l layout) {
			mkdirChmod(t, filepath.Dir(l.root), 0o775)
			touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
		},
		"an installation under the root": func(t *testing.T, l layout) {
			mkdirChmod(t, l.root, 0o777)
			touch(t, filepath.Join(l.root, "bin/unbounded-agent-blue"))
		},
		"a grandparent others can write to": func(t *testing.T, l layout) {
			mkdirChmod(t, filepath.Dir(filepath.Dir(l.root)), 0o777)
			touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			setup(t, l)

			before, beforeErr := os.Lstat(l.root)

			require.ErrorIs(t, migrate(discard(), l.root, l.legacy, Markers()), errUntrusted)

			after, afterErr := os.Lstat(l.root)
			require.Equal(t, beforeErr == nil, afterErr == nil, "the root was created or removed")

			if beforeErr == nil {
				assert.Equal(t, before.Mode(), after.Mode(), "the root was changed")
			}
		})
	}
}

func TestPrepareRefusesARootOthersCanReplace(t *testing.T) {
	t.Parallel()

	l := newLayout(t)
	mkdirChmod(t, filepath.Dir(l.root), 0o777)

	relabeled := false
	err := prepare(t.Context(), discard(), l.root, l.legacy, []string{"bin"},
		func(context.Context, *slog.Logger, string) { relabeled = true })

	require.ErrorIs(t, err, errUntrusted)
	assert.NoDirExists(t, l.root)
	assert.False(t, relabeled)
}

// mkdirChmod creates dir and its parents, and gives dir mode exactly, whatever
// the umask.
func mkdirChmod(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Chmod(dir, mode))
}
