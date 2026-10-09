// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// The move itself is tested in internal/hostroot.
func TestReconcileHostRootUnderLock(t *testing.T) {
	t.Parallel()

	active := &ActiveMachine{Name: "kube1", Config: &provision.AgentConfig{MachineName: "machine-1"}}
	newStore := func(t *testing.T) *installstate.Store {
		t.Helper()

		return installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	}

	// requireReleased checks nothing holds the installation lock any more.
	requireReleased := func(t *testing.T, store *installstate.Store) {
		t.Helper()

		lock, err := store.AcquireMutationLock()
		require.NoError(t, err, "the lock must be released")
		require.NoError(t, lock.Release())
	}

	t.Run("runs with ownership", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		heldDuring := false
		op := &fakeNodeOperator{hostRootDuring: func() {
			_, err := store.AcquireMutationLock()
			heldDuring = errors.Is(err, installstate.ErrLockHeld)
		}}

		assert.False(t, reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active))
		assert.Equal(t, 1, op.hostRootCalls)
		assert.Same(t, active, op.hostRootActive)
		assert.True(t, heldDuring, "an upgrade or a reset must not change the layout during a move")
		requireReleased(t, store)
	})

	t.Run("reports a queued restart and releases the lock", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		op := &fakeNodeOperator{hostRootRestarted: true}

		assert.True(t, reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active))
		requireReleased(t, store)
	})

	t.Run("a failure is logged, not returned", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		op := &fakeNodeOperator{hostRootErr: errors.New("systemd is busy")}

		assert.False(t, reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active),
			"a daemon whose move failed carries on")
		requireReleased(t, store)
	})

	t.Run("skipped while an installation is unfinished", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		record, err := installstate.NewRecord("machine-1", "fingerprint")
		require.NoError(t, err)
		require.NoError(t, store.Save(record))

		op := &fakeNodeOperator{}
		assert.False(t, reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active))

		assert.Zero(t, op.hostRootCalls)
	})
}

// TestVerifyMovedDaemon runs the current daemon link in a copy of the layout,
// which is what a move checks before any unit names the copy.
func TestVerifyMovedDaemon(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))

	require.ErrorContains(t, verifyMovedDaemon(t.Context(), root), hostroot.BinaryCurrentName, "a missing binary cannot run")

	green := filepath.Join(bin, hostroot.BinaryGreenName)
	require.NoError(t, os.WriteFile(green, []byte("#!/bin/sh\n[ \"$1\" = version ]\n"), 0o755))
	require.NoError(t, os.Symlink(green, filepath.Join(bin, hostroot.BinaryCurrentName)))
	require.NoError(t, verifyMovedDaemon(t.Context(), root))

	require.NoError(t, os.Chmod(green, 0o644))
	require.Error(t, verifyMovedDaemon(t.Context(), root), "a binary that cannot run fails")
}

// TestAwaitReplacement: a daemon that queued its own restart does nothing until
// systemd stops it, and fails if that does not come, so Restart= starts it from
// the rewritten units.
func TestAwaitReplacement(t *testing.T) {
	t.Parallel()

	t.Run("fails when the restart does not come", func(t *testing.T) {
		t.Parallel()

		err := awaitReplacement(t.Context(), discardLogger(), time.Millisecond)
		require.ErrorContains(t, err, "not restarted from the host root")
	})

	t.Run("stops with its context", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := awaitReplacement(ctx, discardLogger(), time.Hour)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// TestRestartFromHostRoot: the move's planned restart must not be refused for
// the unit's start limit, so the limit is cleared first, on this unit alone,
// and a denied reset does not stop the restart.
func TestRestartFromHostRoot(t *testing.T) {
	t.Parallel()

	// fakeSystemctl records its arguments, one per line, and exits with code.
	fakeSystemctl := func(t *testing.T, code int) (func(context.Context) *exec.Cmd, string) {
		t.Helper()

		dir := t.TempDir()
		calls := filepath.Join(dir, "calls")
		script := filepath.Join(dir, "systemctl")
		require.NoError(t, os.WriteFile(script,
			fmt.Appendf(nil, "#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %q\nexit %d\n", calls, code), 0o755))

		return func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, script) }, calls
	}

	t.Run("clears this unit's start limit before restarting", func(t *testing.T) {
		t.Parallel()

		systemctl, calls := fakeSystemctl(t, 0)

		var atRestart []byte

		err := restartFromHostRoot(t.Context(), discardLogger(), systemctl, func(context.Context) error {
			var err error

			atRestart, err = os.ReadFile(calls)

			return err
		})
		require.NoError(t, err)
		// Named, so systemd resets this unit and not every unit on the host.
		assert.Equal(t, "reset-failed\n"+goalstates.DaemonUnit+"\n", string(atRestart))
	})

	t.Run("restarts when the reset is denied", func(t *testing.T) {
		t.Parallel()

		systemctl, calls := fakeSystemctl(t, 1)
		restarted := false

		err := restartFromHostRoot(t.Context(), discardLogger(), systemctl, func(context.Context) error {
			restarted = true

			return nil
		})
		require.NoError(t, err)
		assert.True(t, restarted)
		assert.FileExists(t, calls)
	})

	t.Run("reports a failed restart", func(t *testing.T) {
		t.Parallel()

		systemctl, _ := fakeSystemctl(t, 0)
		restartErr := errors.New("restart failed")

		err := restartFromHostRoot(t.Context(), discardLogger(), systemctl, func(context.Context) error {
			return restartErr
		})
		require.ErrorIs(t, err, restartErr)
	})
}
