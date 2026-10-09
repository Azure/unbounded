// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyHost lays out what an agent up to v0.10.0 leaves under the legacy root
// after an upgrade to green, with the root linked to it as a newer agent
// leaves it. The links name the legacy root the way that agent wrote them. The
// root's parent holds files staged for the agent, which a move and a reset
// must leave alone; see assertArtifactsKept.
func legacyHost(t *testing.T) layout {
	t.Helper()

	l := newLayout(t)
	bin := filepath.Join(l.legacy, "bin")

	stageArtifacts(t, l)

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

	require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))

	return l
}

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

			// Only a finished installation under the root counts: on every
			// other host the legacy files may still be in use.
			done, err := installed(l.root, l.legacy)
			require.NoError(t, err)
			assert.Equal(t, tt.want == StateInstalled, done, "installed")
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

	require.NoError(t, stage(l.root, l.legacy, Layout()))

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

// TestStageRebasesLinksThroughALinkedLegacyRoot covers a legacy root that is
// itself a link, as /usr/local is on some images. Older agents wrote link
// targets through it and newer ones through what it resolves to, and both
// have to lead to the copy.
func TestStageRebasesLinksThroughALinkedLegacyRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := filepath.Join(dir, "var", "usrlocal")
	l := layout{root: filepath.Join(dir, "opt", "unbounded", "agent"), legacy: filepath.Join(dir, "usr", "local")}

	require.NoError(t, os.MkdirAll(filepath.Join(real, "bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(l.legacy), 0o755))
	require.NoError(t, os.Symlink(real, l.legacy))
	touch(t, filepath.Join(real, "bin/unbounded-agent-blue"))
	touch(t, filepath.Join(real, "bin/unbounded-agent-green"))
	require.NoError(t, os.Symlink(filepath.Join(l.legacy, "bin/unbounded-agent-green"), filepath.Join(real, "bin/unbounded-agent-current")))
	require.NoError(t, os.Symlink(filepath.Join(canonical(real), "bin/unbounded-agent-blue"), filepath.Join(real, "bin/unbounded-agent-last-good")))

	require.NoError(t, stage(l.root, l.legacy, Layout()))

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
		"/usr/local/bin/unbounded-agent-blue":    "/opt/unbounded/agent/bin/unbounded-agent-blue",
		"/var/usrlocal/bin/unbounded-agent-blue": "/opt/unbounded/agent/bin/unbounded-agent-blue",
		"unbounded-agent-blue":                   "unbounded-agent-blue",
		"/srv/agent/unbounded-agent":             "/srv/agent/unbounded-agent",
		// A sibling that only shares the prefix as a string is not under it.
		"/usr/localother/bin/unbounded-agent": "/usr/localother/bin/unbounded-agent",
	} {
		assert.Equal(t, want, rebase(target, prefixes, "/opt/unbounded/agent"), target)
	}
}

// TestMove covers the copy and the swap. completeMove lays the copy out and
// labels it; see TestReconcileMoveResumes.
func TestMove(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	// LocalDNS was never enabled, so there is nothing under libexec.
	require.NoError(t, os.RemoveAll(filepath.Join(l.legacy, "libexec")))

	require.NoError(t, move(discard(), l.root, l.legacy, Layout()))

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateMoving, got)
	assert.NoDirExists(t, filepath.Join(l.root, "libexec"), "a missing file is skipped, and nothing is made for it")

	_, err = os.Lstat(l.root + stagingSuffix)
	assert.ErrorIs(t, err, os.ErrNotExist)

	current, err := filepath.EvalSymlinks(filepath.Join(l.root, "bin/unbounded-agent-current"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonical(l.root), "bin/unbounded-agent-green"), current,
		"the current link leads to the copy, and compares equal to a slot built from the resolved root")
	assert.Equal(t, "green", readLinked(t, filepath.Join(l.legacy, "bin/unbounded-agent-current")), "the legacy layout is untouched")
	assertArtifactsKept(t, l)
}

func TestUnder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	real := filepath.Join(dir, "var", "usrlocal")
	legacy := filepath.Join(dir, "usr", "local")

	require.NoError(t, os.MkdirAll(real, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	require.NoError(t, os.Symlink(real, legacy))

	for path, want := range map[string]bool{
		filepath.Join(legacy, "bin/unbounded-agent-green"): true,
		// /proc/self/exe names the resolved path.
		filepath.Join(real, "bin/unbounded-agent-green"):                    true,
		filepath.Join(dir, "opt/unbounded/agent/bin/unbounded-agent-green"): false,
		legacy + "other/bin/unbounded-agent-green":                          false,
	} {
		assert.Equal(t, want, under(path, legacy), path)
	}
}

func TestRemoveSeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(t *testing.T, l layout)
		wantKept bool
	}{
		{name: "a seeded binary is removed", setup: func(t *testing.T, l layout) { touch(t, filepath.Join(l.legacy, SeedFile)) }},
		{
			name: "a link is not a seed",
			setup: func(t *testing.T, l layout) {
				require.NoError(t, os.Symlink("/bin/true", filepath.Join(l.legacy, SeedFile)))
			},
			wantKept: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newLayout(t)
			tt.setup(t, l)

			require.NoError(t, removeSeed(discard(), l.legacy, SeedFile))

			_, err := os.Lstat(filepath.Join(l.legacy, SeedFile))
			assert.Equal(t, tt.wantKept, err == nil, "seed kept")
		})
	}

	require.NoError(t, removeSeed(discard(), newLayout(t).legacy, SeedFile), "no seed")
}
