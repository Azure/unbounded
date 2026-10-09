// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveRun records what ReconcileMove asked of the agent.
type moveRun struct {
	rewrites, restarts     int
	rewriteErr, restartErr error
	// What RewriteUnits saw: the units are rewritten while the legacy files
	// are still in place and the host is marked as moving.
	legacyDuringRewrite string
	stateDuringRewrite  State
	// The root whose SELinux labels were restored, if any.
	relabeled string
}

func moveOptions(t *testing.T, l layout, run *moveRun) MoveOptions {
	t.Helper()

	dir := t.TempDir()

	return MoveOptions{
		Files:        Layout(),
		Subdirs:      []string{"bin", "libexec"},
		Record:       filepath.Join(dir, "agents"),
		SignalPath:   filepath.Join(dir, "signal"),
		CurrentPath:  filepath.Join(l.root, "bin", BinaryCurrentName),
		LastGoodPath: filepath.Join(l.root, "bin", BinaryLastGoodName),
		RewriteUnits: func(context.Context) error {
			run.rewrites++
			data, _ := os.ReadFile(filepath.Join(l.legacy, "bin", BinaryCurrentName)) //nolint:errcheck // Absence is what is being recorded.
			run.legacyDuringRewrite = string(data)
			run.stateDuringRewrite, _ = state(l.root, l.legacy) //nolint:errcheck // Compared by the caller.

			return run.rewriteErr
		},
		Restart: func(context.Context) error {
			run.restarts++

			return run.restartErr
		},
	}
}

// fromLegacy and fromRoot are where the green daemon runs from: the legacy
// root until the move restarts it, and the root after.
func fromLegacy(l layout) string { return filepath.Join(l.legacy, "bin", BinaryGreenName) }

func fromRoot(l layout) string { return filepath.Join(l.root, "bin", BinaryGreenName) }

// reconcile runs what the green daemon runs on the legacy host, as the daemon
// running from self.
func reconcile(t *testing.T, l layout, opts MoveOptions, run *moveRun, self string) (bool, error) {
	t.Helper()

	executable := func() (string, error) { return self, nil }
	relabel := func(_ context.Context, _ *slog.Logger, root string) { run.relabeled = root }

	return reconcileMove(t.Context(), discard(), l.root, l.legacy, opts, executable, relabel)
}

func recordBlue(t *testing.T, l layout, opts MoveOptions) {
	t.Helper()
	require.NoError(t, recordDigest(opts.Record, filepath.Join(l.legacy, "bin", BinaryBlueName)))
}

// assertRestarting checks the first pass of a move left the host moving, laid
// out and labeled, with the daemon restarted and the legacy files all in
// place for the units the restart may not have reached.
func assertRestarting(t *testing.T, l layout, opts MoveOptions, run *moveRun, restarted bool, err error) {
	t.Helper()

	require.NoError(t, err)
	assert.True(t, restarted, "the daemon runs from the legacy files until it is restarted")
	assert.Equal(t, 1, run.restarts)

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateMoving, got)
	assert.Equal(t, l.root, run.relabeled, "copied files take their new parent's SELinux label until restored")
	assert.DirExists(t, filepath.Join(l.root, "libexec"), "a moved host is laid out like a fresh one")
	assert.Equal(t, "green", readLinked(t, filepath.Join(l.root, "bin", BinaryCurrentName)))

	for _, path := range LayoutUnder(l.legacy) {
		_, err := os.Lstat(path)
		assert.NoError(t, err, "%s is removed before the daemon restarts from the root", path)
	}

	assert.FileExists(t, opts.Record, "the record stays until the move is finished")
}

// assertMoved checks the host ended up a plain installation under the root,
// with the legacy layout and the move's own bookkeeping gone.
func assertMoved(t *testing.T, l layout, opts MoveOptions) {
	t.Helper()

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateInstalled, got)
	assert.Equal(t, "green", readLinked(t, filepath.Join(l.root, "bin", BinaryCurrentName)))
	assert.Equal(t, "blue", readLinked(t, filepath.Join(l.root, "bin", BinaryLastGoodName)))
	assert.Equal(t, "helper", readLinked(t, filepath.Join(l.root, "bin", NSpawnLifecycleName)))

	for _, path := range LayoutUnder(l.legacy) {
		_, err := os.Lstat(path)
		assert.ErrorIs(t, err, os.ErrNotExist, "%s is left under the legacy root", path)
	}

	assert.FileExists(t, filepath.Join(l.legacy, "bin/unbounded-agent-install.sh"), "files outside the layout stay")
	assert.NoFileExists(t, opts.Record, "only a linked host keeps the record")
	assertArtifactsKept(t, l)
	require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()), "a moved host is a plain installation")
}

// finish runs the daemon restarted from the root and checks it finishes the
// move without restarting again.
func finish(t *testing.T, l layout, opts MoveOptions, run *moveRun) {
	t.Helper()

	restarts := run.restarts

	restarted, err := reconcile(t, l, opts, run, fromRoot(l))
	require.NoError(t, err)
	assert.False(t, restarted, "a daemon running from the root has nothing to restart for")
	assert.Equal(t, restarts, run.restarts)
	assertMoved(t, l, opts)
}

func TestReconcileMove(t *testing.T) {
	t.Parallel()

	t.Run("an older agent in last-good keeps the host linked", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		run := &moveRun{}
		opts := moveOptions(t, l, run)

		restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
		require.NoError(t, err)
		assert.False(t, restarted)
		assert.Zero(t, run.rewrites)

		got, err := state(l.root, l.legacy)
		require.NoError(t, err)
		assert.Equal(t, StateLinked, got)

		green, err := fileDigest(filepath.Join(l.legacy, "bin", BinaryGreenName))
		require.NoError(t, err)
		known, err := loadDigests(opts.Record)
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{green: true}, known, "the running binary records itself")
	})

	t.Run("an unreported upgrade keeps the host linked", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		run := &moveRun{}
		opts := moveOptions(t, l, run)
		recordBlue(t, l, opts)
		require.NoError(t, os.WriteFile(opts.SignalPath, []byte("{}"), 0o600))

		restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
		require.NoError(t, err)
		assert.False(t, restarted)
		assert.Zero(t, run.rewrites)
	})

	t.Run("a host whose slots are both recorded is moved over two starts", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		run := &moveRun{}
		opts := moveOptions(t, l, run)
		recordBlue(t, l, opts)

		restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
		assertRestarting(t, l, opts, run, restarted, err)
		assert.Equal(t, 1, run.rewrites)
		assert.Equal(t, StateMoving, run.stateDuringRewrite)
		assert.Equal(t, "green", run.legacyDuringRewrite, "the legacy files stay until the units name the new ones")

		finish(t, l, opts, run)
		assert.Equal(t, 2, run.rewrites, "the units are rewritten again before the legacy files go")
	})

	t.Run("other hosts are left alone", func(t *testing.T) {
		t.Parallel()

		for name, setup := range map[string]func(t *testing.T, l layout){
			"no root":   func(*testing.T, layout) {},
			"installed": func(t *testing.T, l layout) { touch(t, filepath.Join(l.root, "bin", BinaryBlueName)) },
			"a link someone else made": func(t *testing.T, l layout) {
				require.NoError(t, os.MkdirAll(filepath.Dir(l.root), 0o755))
				require.NoError(t, os.Symlink(t.TempDir(), l.root))
			},
		} {
			t.Run(name, func(t *testing.T) {
				l := newLayout(t)
				setup(t, l)

				run := &moveRun{}
				opts := moveOptions(t, l, run)

				restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
				require.NoError(t, err)
				assert.False(t, restarted)
				assert.Zero(t, run.rewrites)
				assert.Empty(t, run.relabeled)
				assert.NoFileExists(t, opts.Record, "only a linked host records its binary")
			})
		}
	})
}

// TestReconcileMoveKeepsTheLegacyFilesUntilTheUnitsAreRewritten covers a
// failure to rewrite the units: the files they still name stay, the host stays
// marked as moving, and the next run finishes the move.
func TestReconcileMoveKeepsTheLegacyFilesUntilTheUnitsAreRewritten(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	run := &moveRun{rewriteErr: errors.New("systemd is busy")}
	opts := moveOptions(t, l, run)
	recordBlue(t, l, opts)

	restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
	require.ErrorContains(t, err, "systemd is busy")
	assert.False(t, restarted)
	assert.Zero(t, run.restarts)
	assert.Equal(t, "green", readLinked(t, filepath.Join(l.legacy, "bin", BinaryCurrentName)))

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateMoving, got)

	run.rewriteErr = nil
	restarted, err = reconcile(t, l, opts, run, fromLegacy(l))
	assertRestarting(t, l, opts, run, restarted, err)
	finish(t, l, opts, run)
}

// TestReconcileMoveRetriesAFailedRestart covers a restart that fails: nothing
// has been removed, so the daemon keeps running from files that are still
// there, and the next start restarts it again.
func TestReconcileMoveRetriesAFailedRestart(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	run := &moveRun{restartErr: errors.New("restart refused")}
	opts := moveOptions(t, l, run)
	recordBlue(t, l, opts)

	restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
	require.ErrorContains(t, err, "restart refused")
	assert.False(t, restarted)

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateMoving, got, "the marker stays, so the next start finishes the move")

	for _, path := range LayoutUnder(l.legacy) {
		_, err := os.Lstat(path)
		assert.NoError(t, err, "%s is removed although the daemon still runs from the legacy root", path)
	}

	run.restartErr = nil
	run.restarts = 0
	restarted, err = reconcile(t, l, opts, run, fromLegacy(l))
	assertRestarting(t, l, opts, run, restarted, err)
	finish(t, l, opts, run)
}

// TestReconcileMoveRemovesTheAgentsDirectories checks the directories that hold
// only the agent's files go with them, and only once the files are gone.
func TestReconcileMoveRemovesTheAgentsDirectories(t *testing.T) {
	t.Parallel()

	l := legacyHost(t)
	run := &moveRun{}
	opts := moveOptions(t, l, run)
	recordBlue(t, l, opts)

	// The parent is listed before the directory nested in it.
	opts.Dirs = []string{"lib/agent", "lib/agent/nested", "libexec", "lib/kept", "lib/linked", "lib/missing"}

	require.NoError(t, os.MkdirAll(filepath.Join(l.legacy, "lib/agent/nested"), 0o755))
	touch(t, filepath.Join(l.legacy, "lib/kept/operator.conf"))

	elsewhere := t.TempDir()
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(l.legacy, "lib/linked")))

	restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
	assertRestarting(t, l, opts, run, restarted, err)
	assert.DirExists(t, filepath.Join(l.legacy, "libexec"), "the directories stay while the files in them do")
	assert.DirExists(t, filepath.Join(l.legacy, "lib/agent/nested"))

	finish(t, l, opts, run)

	for _, rel := range []string{"lib/agent", "libexec"} {
		assert.NoDirExists(t, filepath.Join(l.legacy, rel))
	}

	assert.FileExists(t, filepath.Join(l.legacy, "lib/kept/operator.conf"), "a directory with other files in it stays")

	info, err := os.Lstat(filepath.Join(l.legacy, "lib/linked"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "a link is not the agent's directory")
	assert.DirExists(t, elsewhere)
	assert.DirExists(t, filepath.Join(l.legacy, "bin"), "a directory that is not listed stays")
}

// TestReconcileMoveResumes interrupts the move, runs what a restarted daemon
// runs, Migrate and then ReconcileMove, and checks the move completes.
func TestReconcileMoveResumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		interrupt func(t *testing.T, l layout, opts MoveOptions)
		// The interrupted daemon was already running from the root.
		fromRoot bool
	}{
		{
			// The window in which the root does not exist.
			name: "after removing the link",
			interrupt: func(t *testing.T, l layout, _ MoveOptions) {
				require.NoError(t, stage(l.root, l.legacy, Layout()))
				require.NoError(t, os.Remove(l.root))
			},
		},
		{
			// The copy is in place but not yet laid out or labeled.
			name: "after the rename",
			interrupt: func(t *testing.T, l layout, _ MoveOptions) {
				require.NoError(t, move(discard(), l.root, l.legacy, Layout()))
			},
		},
		{
			// The restarted daemon died while it removed the legacy files.
			name: "partway through removing the legacy files",
			interrupt: func(t *testing.T, l layout, _ MoveOptions) {
				require.NoError(t, move(discard(), l.root, l.legacy, Layout()))
				require.NoError(t, os.Remove(filepath.Join(l.legacy, "bin", BinaryBlueName)))
				require.NoError(t, os.Remove(filepath.Join(l.legacy, "bin", BinaryCurrentName)))
			},
			fromRoot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := legacyHost(t)
			run := &moveRun{}
			opts := moveOptions(t, l, run)
			recordBlue(t, l, opts)
			tt.interrupt(t, l, opts)

			require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))

			if !tt.fromRoot {
				restarted, err := reconcile(t, l, opts, run, fromLegacy(l))
				assertRestarting(t, l, opts, run, restarted, err)
			}

			finish(t, l, opts, run)
			assert.Equal(t, l.root, run.relabeled, "a resumed move restores the labels too")
		})
	}
}

func TestRecordDigest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	record := filepath.Join(dir, "agents")
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	require.NoError(t, os.WriteFile(first, []byte("first"), 0o755))
	require.NoError(t, os.WriteFile(second, []byte("second"), 0o755))

	for _, path := range []string{first, first, second} {
		require.NoError(t, recordDigest(record, path))
	}

	data, err := os.ReadFile(record)
	require.NoError(t, err)
	assert.Len(t, strings.Fields(string(data)), 2, "a digest is recorded once")

	info, err := os.Stat(record)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
