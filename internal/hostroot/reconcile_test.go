// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveRun records what ReconcileMove asked of the agent.
type moveRun struct {
	rewrites, restarts int
	rewriteErr         error
	// What RewriteUnits saw: the units are rewritten while the legacy files
	// are still in place and the host is marked as moving.
	legacyDuringRewrite string
	stateDuringRewrite  State
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

			return nil
		},
	}
}

// reconcile runs what the daemon of the green agent runs on the legacy host.
func reconcile(t *testing.T, l layout, opts MoveOptions) (bool, error) {
	t.Helper()

	self := func() (string, error) { return filepath.Join(l.legacy, "bin", BinaryGreenName), nil }

	return reconcileMove(t.Context(), discard(), l.root, l.legacy, opts, self, noRelabel)
}

func recordBlue(t *testing.T, l layout, opts MoveOptions) {
	t.Helper()
	require.NoError(t, recordDigest(opts.Record, filepath.Join(l.legacy, "bin", BinaryBlueName)))
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
	require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()), "a moved host is a plain installation")
}

func TestReconcileMove(t *testing.T) {
	t.Parallel()

	t.Run("an older agent in last-good keeps the host linked", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		run := &moveRun{}
		opts := moveOptions(t, l, run)

		restarted, err := reconcile(t, l, opts)
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

		restarted, err := reconcile(t, l, opts)
		require.NoError(t, err)
		assert.False(t, restarted)
		assert.Zero(t, run.rewrites)
	})

	t.Run("a host whose slots are both recorded is moved", func(t *testing.T) {
		t.Parallel()

		l := legacyHost(t)
		run := &moveRun{}
		opts := moveOptions(t, l, run)
		recordBlue(t, l, opts)

		restarted, err := reconcile(t, l, opts)
		require.NoError(t, err)
		assert.True(t, restarted)
		assert.Equal(t, 1, run.rewrites)
		assert.Equal(t, 1, run.restarts, "the running daemon's binary is gone")
		assert.Equal(t, StateMoving, run.stateDuringRewrite)
		assert.Equal(t, "green", run.legacyDuringRewrite, "the legacy files stay until the units name the new ones")
		assertMoved(t, l, opts)
	})

	t.Run("other hosts are left alone", func(t *testing.T) {
		t.Parallel()

		for name, setup := range map[string]func(t *testing.T, l layout){
			"no root":   func(*testing.T, layout) {},
			"installed": func(t *testing.T, l layout) { touch(t, filepath.Join(l.root, "bin", BinaryBlueName)) },
		} {
			l := newLayout(t)
			setup(t, l)

			run := &moveRun{}

			restarted, err := reconcile(t, l, moveOptions(t, l, run))
			require.NoError(t, err, name)
			assert.False(t, restarted, name)
			assert.Zero(t, run.rewrites, name)
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

	restarted, err := reconcile(t, l, opts)
	require.ErrorContains(t, err, "systemd is busy")
	assert.False(t, restarted)
	assert.Zero(t, run.restarts)
	assert.Equal(t, "green", readLinked(t, filepath.Join(l.legacy, "bin", BinaryCurrentName)))

	got, err := state(l.root, l.legacy)
	require.NoError(t, err)
	assert.Equal(t, StateMoving, got)

	run.rewriteErr = nil
	restarted, err = reconcile(t, l, opts)
	require.NoError(t, err)
	assert.True(t, restarted)
	assertMoved(t, l, opts)
}

// TestReconcileMoveResumes interrupts the move, runs what a restarted daemon
// runs, Migrate and then ReconcileMove, and checks the move completes.
func TestReconcileMoveResumes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		interrupt func(t *testing.T, l layout)
	}{
		{
			// The window in which the root does not exist.
			name: "after removing the link",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, stage(l.root, l.legacy, Layout()))
				require.NoError(t, os.Remove(l.root))
			},
		},
		{
			name: "after the swap",
			interrupt: func(t *testing.T, l layout) {
				require.NoError(t, move(t.Context(), discard(), l.root, l.legacy, Layout(), nil, noRelabel))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := legacyHost(t)
			run := &moveRun{}
			opts := moveOptions(t, l, run)
			recordBlue(t, l, opts)
			tt.interrupt(t, l)

			require.NoError(t, migrate(discard(), l.root, l.legacy, Markers()))

			restarted, err := reconcile(t, l, opts)
			require.NoError(t, err)
			assert.True(t, restarted)
			assertMoved(t, l, opts)
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
