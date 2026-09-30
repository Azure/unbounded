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

// testLayout mirrors goalstates.HostLayout, which this package cannot import.
var testLayout = []string{
	"bin/unbounded-agent",
	"bin/unbounded-agent-blue",
	"bin/unbounded-agent-green",
	"bin/unbounded-agent-current",
	"bin/unbounded-agent-last-good",
	"bin/unbounded-agent-nspawn-lifecycle",
	"bin/unbounded-agent-daemon-recovery.sh",
	"libexec/unbounded-localdns-network",
}

// legacyHost lays out what an agent up to v0.8.0 leaves under the legacy root
// after an upgrade to green, with the root linked to it as a newer agent
// leaves it. The links name the legacy root the way that agent wrote them.
func legacyHost(t *testing.T) layout {
	t.Helper()

	l := newLayout(t)
	bin := filepath.Join(l.legacy, "bin")

	for name, content := range map[string]string{
		"unbounded-agent-blue":               "blue",
		"unbounded-agent-green":              "green",
		"unbounded-agent-nspawn-lifecycle":   "helper",
		"unbounded-agent-daemon-recovery.sh": "recover",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(content), 0o755))
	}

	require.NoError(t, os.MkdirAll(filepath.Join(l.legacy, "libexec"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(l.legacy, "libexec/unbounded-localdns-network"), []byte("dns"), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(bin, "unbounded-agent-green"), filepath.Join(bin, "unbounded-agent-current")))
	require.NoError(t, os.Symlink(filepath.Join(bin, "unbounded-agent-blue"), filepath.Join(bin, "unbounded-agent-last-good")))
	require.NoError(t, os.Symlink(filepath.Join(bin, "unbounded-agent-current"), filepath.Join(bin, "unbounded-agent")))
	// Not part of the layout; it stays where it is.
	touch(t, filepath.Join(bin, "unbounded-agent-install.sh"))

	require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers))

	return l
}

func noRelabel(context.Context, *slog.Logger, string) {}

func readLinked(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func TestState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, l layout)
		want  State
	}{
		{name: "absent", setup: func(*testing.T, layout) {}, want: StateAbsent},
		{
			name: "linked",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(l.legacy, l.root))
			},
			want: StateLinked,
		},
		{
			name: "a link someone else made",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(t.TempDir(), l.root))
			},
			want: StateOther,
		},
		{name: "a file", setup: func(t *testing.T, l layout) { touch(t, l.root) }, want: StateOther},
		{
			name:  "installed",
			setup: func(t *testing.T, l layout) { require.NoError(t, os.MkdirAll(l.root, 0o755)) },
			want:  StateInstalled,
		},
		{
			name:  "moving",
			setup: func(t *testing.T, l layout) { touch(t, filepath.Join(l.root, movingMarker)) },
			want:  StateMoving,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			tt.setup(t, l)

			got, err := state(l.root, l.legacy)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestStage checks the copy is complete and self-contained, and that nothing
// in use changes while it is made.
func TestStage(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	staging := l.root + stagingSuffix
	// Left by an earlier attempt, and not trusted.
	touch(t, filepath.Join(staging, "bin/stale"))

	require.NoError(t, stage(l.root, l.legacy, testLayout))

	final := filepath.Join(canonical(filepath.Dir(l.root)), filepath.Base(l.root))

	for name, want := range map[string]string{
		"bin/unbounded-agent-blue":             "blue",
		"bin/unbounded-agent-green":            "green",
		"bin/unbounded-agent-nspawn-lifecycle": "helper",
		"libexec/unbounded-localdns-network":   "dns",
	} {
		assert.Equal(t, want, readLinked(t, filepath.Join(staging, name)), name)

		src, err := os.Stat(filepath.Join(l.legacy, name))
		require.NoError(t, err)
		dst, err := os.Stat(filepath.Join(staging, name))
		require.NoError(t, err)
		assert.Equal(t, src.Mode().Perm(), dst.Mode().Perm(), "%s keeps its mode", name)
	}

	// The links lead to the copy under the root, as it will be named once in
	// place, not back to the legacy root.
	for link, want := range map[string]string{
		"bin/unbounded-agent-current":   filepath.Join(final, "bin/unbounded-agent-green"),
		"bin/unbounded-agent-last-good": filepath.Join(final, "bin/unbounded-agent-blue"),
		"bin/unbounded-agent":           filepath.Join(final, "bin/unbounded-agent-current"),
	} {
		target, err := os.Readlink(filepath.Join(staging, link))
		require.NoError(t, err)
		assert.Equal(t, want, target, link)
	}

	_, err := os.Stat(filepath.Join(staging, movingMarker))
	require.NoError(t, err, "a complete copy carries the marker")
	_, err = os.Stat(filepath.Join(staging, "bin/stale"))
	assert.ErrorIs(t, err, os.ErrNotExist, "an earlier attempt's copy is replaced")
	_, err = os.Lstat(filepath.Join(staging, "bin/unbounded-agent-install.sh"))
	assert.ErrorIs(t, err, os.ErrNotExist, "only the layout is copied")

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateLinked, got, "the root is untouched until the swap")
	assert.Equal(t, "green", readLinked(t, filepath.Join(l.legacy, "bin/unbounded-agent-current")), "the legacy layout is untouched")
}

func TestStageSkipsMissingFiles(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	require.NoError(t, os.Remove(filepath.Join(l.legacy, "libexec/unbounded-localdns-network")))

	require.NoError(t, stage(l.root, l.legacy, testLayout))

	_, err := os.Lstat(filepath.Join(l.root+stagingSuffix, "libexec/unbounded-localdns-network"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestStageRebasesLinksThroughALinkedLegacyRoot covers a legacy root that is
// itself a link, as /usr/local is on some images. Older agents wrote link
// targets through it and newer ones through what it resolves to, and both
// have to lead to the copy.
func TestStageRebasesLinksThroughALinkedLegacyRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := filepath.Join(dir, "var", "usrlocal")
	l := layout{root: filepath.Join(dir, "opt", "unbounded"), legacy: filepath.Join(dir, "usr", "local")}

	require.NoError(t, os.MkdirAll(filepath.Join(real, "bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(l.legacy), 0o755))
	require.NoError(t, os.Symlink(real, l.legacy))
	touch(t, filepath.Join(real, "bin/unbounded-agent-blue"))
	touch(t, filepath.Join(real, "bin/unbounded-agent-green"))
	require.NoError(t, os.Symlink(filepath.Join(l.legacy, "bin/unbounded-agent-green"), filepath.Join(real, "bin/unbounded-agent-current")))
	require.NoError(t, os.Symlink(filepath.Join(canonical(real), "bin/unbounded-agent-blue"), filepath.Join(real, "bin/unbounded-agent-last-good")))

	require.NoError(t, stage(l.root, l.legacy, testLayout))

	final := filepath.Join(canonical(filepath.Dir(l.root)), filepath.Base(l.root))

	current, err := os.Readlink(filepath.Join(l.root+stagingSuffix, "bin/unbounded-agent-current"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(final, "bin/unbounded-agent-green"), current)

	lastGood, err := os.Readlink(filepath.Join(l.root+stagingSuffix, "bin/unbounded-agent-last-good"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(final, "bin/unbounded-agent-blue"), lastGood)
}

func TestRebase(t *testing.T) {
	t.Parallel()

	prefixes := []string{"/usr/local", "/var/usrlocal"}

	for target, want := range map[string]string{
		"/usr/local/bin/unbounded-agent-blue":    "/opt/unbounded/bin/unbounded-agent-blue",
		"/var/usrlocal/bin/unbounded-agent-blue": "/opt/unbounded/bin/unbounded-agent-blue",
		"unbounded-agent-blue":                   "unbounded-agent-blue",
		"/srv/agent/unbounded-agent":             "/srv/agent/unbounded-agent",
		// A sibling that only shares the prefix as a string is not under it.
		"/usr/localother/bin/unbounded-agent": "/usr/localother/bin/unbounded-agent",
	} {
		assert.Equal(t, want, rebase(target, prefixes, "/opt/unbounded"), target)
	}
}

func TestSwap(t *testing.T) {
	t.Parallel()

	t.Run("replaces the link with the copy", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		// LocalDNS was never enabled, so there is nothing under libexec.
		require.NoError(t, os.RemoveAll(filepath.Join(l.legacy, "libexec")))
		require.NoError(t, stage(l.root, l.legacy, testLayout))

		relabeled := ""

		require.NoError(t, swap(t.Context(), discard(), l.root, l.legacy, []string{"bin", "libexec"},
			func(_ context.Context, _ *slog.Logger, root string) { relabeled = root }))

		got, err := state(l.root, l.legacy)
		require.NoError(t, err)
		assert.Equal(t, StateMoving, got)
		assert.Equal(t, l.root, relabeled, "copied files take their new parent's SELinux label until restored")
		assert.DirExists(t, filepath.Join(l.root, "libexec"), "a moved host is laid out like a fresh one")

		_, err = os.Lstat(l.root + stagingSuffix)
		assert.ErrorIs(t, err, os.ErrNotExist)

		current, err := filepath.EvalSymlinks(filepath.Join(l.root, "bin/unbounded-agent-current"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(canonical(l.root), "bin/unbounded-agent-green"), current,
			"the current link leads to the copy, and compares equal to a slot built from the resolved root")
	})

	t.Run("refuses an incomplete copy", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		require.NoError(t, stage(l.root, l.legacy, testLayout))
		require.NoError(t, os.Remove(filepath.Join(l.root+stagingSuffix, movingMarker)))

		require.ErrorContains(t, swap(t.Context(), discard(), l.root, l.legacy, nil, noRelabel), "not a complete copy")

		got, err := state(l.root, l.legacy)
		require.NoError(t, err)
		assert.Equal(t, StateLinked, got, "the link stays")
	})

	t.Run("refuses a root that is already a directory", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		require.NoError(t, os.MkdirAll(l.root, 0o755))
		touch(t, filepath.Join(l.root+stagingSuffix, movingMarker))

		require.ErrorContains(t, swap(t.Context(), discard(), l.root, l.legacy, nil, noRelabel), "not a link to")
	})
}

// TestMoveResumes interrupts the move after each step, runs what a restarted
// daemon runs, and checks the move completes. Throughout, the files the old
// units name, under the legacy root, stay in place until the move is finished,
// so a host interrupted at any point keeps a daemon unit that can start.
func TestMoveResumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		interrupt func(t *testing.T, l layout)
		// removing is set once the daemon has begun removing the legacy
		// layout, which it does only after the units name the new one.
		removing bool
	}{
		{name: "before anything", interrupt: func(*testing.T, layout) {}},
		{
			name: "after staging",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, stage(l.root, l.legacy, testLayout))
			},
		},
		{
			name: "part way through staging",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, stage(l.root, l.legacy, testLayout))
				require.NoError(t, os.Remove(filepath.Join(l.root+stagingSuffix, movingMarker)))
				require.NoError(t, os.Remove(filepath.Join(l.root+stagingSuffix, "bin/unbounded-agent-green")))
			},
		},
		{
			// The window in which the root does not exist.
			name: "after removing the link",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, stage(l.root, l.legacy, testLayout))
				require.NoError(t, os.Remove(l.root))
			},
		},
		{
			name: "after the swap",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, stage(l.root, l.legacy, testLayout))
				require.NoError(t, swap(t.Context(), discard(), l.root, l.legacy, nil, noRelabel))
			},
		},
		{
			name: "part way through removing the legacy layout",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, stage(l.root, l.legacy, testLayout))
				require.NoError(t, swap(t.Context(), discard(), l.root, l.legacy, nil, noRelabel))
				require.NoError(t, os.Remove(filepath.Join(l.legacy, "bin/unbounded-agent-blue")))
			},
			removing: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := legacyHost(t)
			tt.interrupt(t, l)

			// What a restarted daemon runs: Migrate, then the move from
			// wherever the state says it is.
			require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers))

			got, err := state(l.root, l.legacy)
			require.NoError(t, err)

			if got == StateLinked {
				require.NoError(t, discardStaging(l.root))
				require.NoError(t, stage(l.root, l.legacy, testLayout))
				require.NoError(t, swap(t.Context(), discard(), l.root, l.legacy, nil, noRelabel))

				got, err = state(l.root, l.legacy)
				require.NoError(t, err)
			}

			require.Equal(t, StateMoving, got)

			if !tt.removing {
				assert.Equal(t, "green", readLinked(t, filepath.Join(l.legacy, "bin/unbounded-agent-current")),
					"the legacy layout is still in place for the units that name it")
			}

			// The daemon rewrites the units here, then removes the legacy
			// layout and finishes.
			for _, rel := range testLayout {
				if err := os.Remove(filepath.Join(l.legacy, rel)); err != nil {
					require.ErrorIs(t, err, os.ErrNotExist)
				}
			}

			require.NoError(t, finishMove(l.root))

			got, err = state(l.root, l.legacy)
			require.NoError(t, err)
			assert.Equal(t, StateInstalled, got)
			assert.Equal(t, "green", readLinked(t, filepath.Join(l.root, "bin/unbounded-agent-current")))
			assert.Equal(t, "blue", readLinked(t, filepath.Join(l.root, "bin/unbounded-agent-last-good")))
			assert.Equal(t, "helper", readLinked(t, filepath.Join(l.root, "bin/unbounded-agent-nspawn-lifecycle")))

			_, err = os.Lstat(l.root + stagingSuffix)
			assert.ErrorIs(t, err, os.ErrNotExist)
			assert.FileExists(t, filepath.Join(l.legacy, "bin/unbounded-agent-install.sh"), "files outside the layout stay")

			require.NoError(t, migrate(discard(), l.root, l.legacy, testMarkers), "a moved host is a plain installation")
		})
	}
}

func TestDiscardStaging(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	require.NoError(t, discardStaging(l.root), "nothing to discard")

	require.NoError(t, stage(l.root, l.legacy, testLayout))
	require.NoError(t, discardStaging(l.root))

	_, err := os.Lstat(l.root + stagingSuffix)
	assert.ErrorIs(t, err, os.ErrNotExist)

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateLinked, got)
}

func TestRemoveSeed(t *testing.T) {
	t.Parallel()

	const seed = "bin/unbounded-agent"

	installed := func(t *testing.T, l layout) {
		require.NoError(t, os.MkdirAll(filepath.Join(l.root, "bin"), 0o755))
	}

	tests := []struct {
		name     string
		setup    func(t *testing.T, l layout)
		wantKept bool
	}{
		{
			name: "a seed beside a host installed under the root is removed",
			setup: func(t *testing.T, l layout) {
				installed(t, l)
				touch(t, filepath.Join(l.legacy, seed))
			},
		},
		{
			name: "a link is not a seed",
			setup: func(t *testing.T, l layout) {
				installed(t, l)
				require.NoError(t, os.Symlink("/bin/true", filepath.Join(l.legacy, seed)))
			},
			wantKept: true,
		},
		{
			name: "a legacy installation keeps its binary",
			setup: func(t *testing.T, l layout) {
				installed(t, l)
				touch(t, filepath.Join(l.legacy, seed))
				touch(t, filepath.Join(l.legacy, "bin/unbounded-agent-blue"))
			},
			wantKept: true,
		},
		{
			name: "a linked host keeps it",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.legacy, seed))
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(l.legacy, l.root))
			},
			wantKept: true,
		},
		{
			name: "an unfinished move keeps it",
			setup: func(t *testing.T, l layout) {
				touch(t, filepath.Join(l.root, movingMarker))
				touch(t, filepath.Join(l.legacy, seed))
			},
			wantKept: true,
		},
		{
			// Bootstrap has not run yet, and the agent that runs it may be
			// the one the seed is for.
			name:     "a host with no root keeps it",
			setup:    func(t *testing.T, l layout) { touch(t, filepath.Join(l.legacy, seed)) },
			wantKept: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			tt.setup(t, l)

			require.NoError(t, removeSeed(discard(), l.root, l.legacy, seed, testMarkers))

			_, err := os.Lstat(filepath.Join(l.legacy, seed))
			assert.Equal(t, tt.wantKept, err == nil, "seed kept")
		})
	}

	t.Run("no seed", func(t *testing.T) {
		t.Parallel()

		l := newLayout(t)
		installed(t, l)
		require.NoError(t, removeSeed(discard(), l.root, l.legacy, seed, testMarkers))
	})
}

// TestStageIgnoresTheUmask is not parallel because the umask belongs to the
// process. Parallel tests are paused while it runs.
func TestStageIgnoresTheUmask(t *testing.T) {
	l := legacyHost(t)

	old := syscall.Umask(0o077)

	t.Cleanup(func() { syscall.Umask(old) })

	require.NoError(t, stage(l.root, l.legacy, testLayout))

	for _, dir := range []string{l.root + stagingSuffix, filepath.Join(l.root+stagingSuffix, "bin")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), dir)
	}

	info, err := os.Stat(filepath.Join(l.root+stagingSuffix, "bin/unbounded-agent-green"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}
