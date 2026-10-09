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

type layout struct {
	root, legacy string
}

// newLayout mirrors the host: the root is nested in a parent that is not the
// agent's, and the legacy root exists.
func newLayout(t *testing.T) layout {
	t.Helper()

	dir := t.TempDir()
	l := layout{root: filepath.Join(dir, "opt", "unbounded", "agent"), legacy: filepath.Join(dir, "usr", "local")}
	require.NoError(t, os.MkdirAll(filepath.Join(l.legacy, "bin"), 0o755))

	return l
}

// stageArtifacts puts files under the root's parent, as a host does when it
// stages offline artifacts there, and gives the parent a mode the agent would
// not choose; see assertArtifactsKept.
func stageArtifacts(t *testing.T, l layout) {
	t.Helper()

	artifact := artifactPath(l)
	require.NoError(t, os.MkdirAll(filepath.Dir(artifact), 0o755))
	require.NoError(t, os.WriteFile(artifact, []byte("manifest"), 0o644))
	require.NoError(t, os.Chmod(filepath.Dir(l.root), 0o750))
}

func artifactPath(l layout) string {
	return filepath.Join(filepath.Dir(l.root), "artifacts", "v1.34.2", "manifest.json")
}

// assertArtifactsKept checks what stageArtifacts made is untouched.
func assertArtifactsKept(t *testing.T, l layout) {
	t.Helper()

	data, err := os.ReadFile(artifactPath(l))
	require.NoError(t, err, "files beside the root are not the agent's")
	assert.Equal(t, "manifest", string(data))

	info, err := os.Stat(filepath.Dir(l.root))
	require.NoError(t, err, "the root's parent is never removed")
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm(), "the root's parent keeps its mode")
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
		// The setup staged artifacts beside the root, which must survive.
		artifacts bool
	}{
		{
			name:  "fresh host is left alone",
			setup: func(*testing.T, layout) {},
		},
		{
			name:     "legacy installation is linked",
			setup:    func(t *testing.T, l layout) { touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue")) },
			wantLink: true,
		},
		{
			// Hosts stage offline artifacts under /opt/unbounded, as the agent
			// docs suggest. The link goes in beside them.
			name: "legacy installation is linked inside a parent that holds other files",
			setup: func(t *testing.T, l layout) {
				stageArtifacts(t, l)
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
			},
			wantLink:  true,
			artifacts: true,
		},
		{
			name: "a fresh host with files beside the root is left alone",
			setup: func(t *testing.T, l layout) {
				stageArtifacts(t, l)
			},
			artifacts: true,
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
			// Install scripts seed the plain binary for agents up to v0.10.0
			// on every host that allows it.
			name:  "a seeded binary on its own is not an installation",
			setup: func(t *testing.T, l layout) { touch(t, filepath.Join(l.legacy, "bin/unbounded-agent")) },
		},
		{
			name: "a move that has not finished is left to finish",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.root, "bin/unbounded-agent-blue"))
				touch(t, filepath.Join(l.root, movingMarker))
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
			},
			wantDir: true,
		},
		{
			name: "a new installation is left alone",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.root, "bin/unbounded-agent-blue"))
			},
			wantDir: true,
		},
		{
			name: "installations under both roots are refused",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.root, "bin/unbounded-agent-blue"))
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
			},
			wantErr: "installed under both",
			wantDir: true,
		},
		{
			name: "a legacy installation beside an empty root directory is refused",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(l.root, 0o755))
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
			},
			wantErr: "also exists",
			wantDir: true,
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

			err := migrate(discard(), l.root, l.legacy, Markers())
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()), "migration must be idempotent")
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

			if tt.artifacts {
				assertArtifactsKept(t, l)
			}
		})
	}
}

// TestMigrateCreatesTheParentIgnoringTheUmask is not parallel because the umask
// belongs to the process. Parallel tests are paused while it runs.
func TestMigrateCreatesTheParentIgnoringTheUmask(t *testing.T) {
	old := syscall.Umask(0o077)

	t.Cleanup(func() { syscall.Umask(old) })

	l := newLayout(t)
	touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))

	require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))

	info, err := os.Stat(filepath.Dir(l.root))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the root's parent must be traversable")
}

func TestPlanned(t *testing.T) {
	t.Parallel()

	t.Run("fresh host", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		assert.Equal(t, canonical(l.root), planned(l.root, l.legacy, Markers()))
		_, err := os.Lstat(l.root)
		assert.ErrorIs(t, err, os.ErrNotExist, "planning must not change the host")
	})

	t.Run("unmigrated legacy host", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
		assert.Equal(t, canonical(l.legacy), planned(l.root, l.legacy, Markers()))
		_, err := os.Lstat(l.root)
		assert.ErrorIs(t, err, os.ErrNotExist, "planning must not change the host")
	})

	t.Run("migrated host", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
		require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))
		assert.Equal(t, canonical(l.legacy), planned(l.root, l.legacy, Markers()))
	})

	// An older release's reset removed the files but not the link, which
	// Migrate then removes.
	t.Run("a link with no installation behind it", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
		require.NoError(t, os.Symlink(l.legacy, l.root))

		got := planned(l.root, l.legacy, Markers())
		info, err := os.Lstat(l.root)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink, "planning must not change the host")

		require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))
		assert.Equal(t, canonical(l.root), got, "planned where the root will be once migrated")
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

	for _, dir := range []string{filepath.Dir(l.root), l.root, filepath.Join(l.root, "bin"), filepath.Join(l.root, "libexec")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), dir)
	}

	assert.Equal(t, l.root, relabeled, "new directories take their parent's SELinux label until restored")
}

// TestPrepareLeavesTheParentAlone covers a fresh host that already has the
// root's parent, holding files staged for the agent.
func TestPrepareLeavesTheParentAlone(t *testing.T) {
	t.Parallel()

	l := newLayout(t)
	stageArtifacts(t, l)

	relabeled := ""

	require.NoError(t, prepare(t.Context(), discard(), l.root, []string{"bin"},
		func(_ context.Context, _ *slog.Logger, root string) { relabeled = root }))

	assert.DirExists(t, filepath.Join(l.root, "bin"))
	assert.Equal(t, l.root, relabeled, "only the root is relabeled")
	assertArtifactsKept(t, l)
}

func TestPrepareLeavesAMigratedHostAlone(t *testing.T) {
	t.Parallel()

	l := newLayout(t)
	touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
	require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))

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
		// The setup staged artifacts beside the root, which must survive.
		artifacts bool
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
		{
			name: "an unfinished move's marker does not keep the root",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Join(l.root, "bin"), 0o755))
				touch(t, filepath.Join(l.root, movingMarker))
			},
			wantLegacy: true,
		},
		{
			name: "a staging copy is removed with the link",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(l.legacy, l.root))
				touch(t, filepath.Join(l.root+stagingSuffix, "bin/unbounded-agent-blue"))
			},
			wantLegacy: true,
		},
		{
			name: "a link beside files staged for the agent is removed, and only the link",
			setup: func(t *testing.T, l layout) {
				stageArtifacts(t, l)
				require.NoError(t, os.Symlink(l.legacy, l.root))
			},
			wantLegacy: true,
			artifacts:  true,
		},
		{
			name: "a root beside files staged for the agent is removed, and only the root",
			setup: func(t *testing.T, l layout) {
				stageArtifacts(t, l)
				require.NoError(t, os.MkdirAll(filepath.Join(l.root, "bin"), 0o755))
			},
			wantLegacy: true,
			artifacts:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			tt.setup(t, l)

			_, parentErr := os.Stat(filepath.Dir(l.root))

			require.NoError(t, remove(discard(), l.root, l.legacy))

			if parentErr == nil {
				assert.DirExists(t, filepath.Dir(l.root), "the root's parent is never removed, even when empty")
			}

			if tt.artifacts {
				assertArtifactsKept(t, l)
			}

			_, err := os.Lstat(l.root)
			assert.Equal(t, tt.wantRoot, err == nil, "root present")

			_, err = os.Lstat(l.root + stagingSuffix)
			assert.ErrorIs(t, err, os.ErrNotExist, "a staging copy never survives reset")

			_, err = os.Lstat(filepath.Join(l.root, "bin"))
			assert.ErrorIs(t, err, os.ErrNotExist, "an empty subdirectory never survives reset")

			_, err = os.Stat(filepath.Join(l.legacy, "bin"))
			assert.Equal(t, tt.wantLegacy, err == nil, "legacy root untouched")
		})
	}
}
