// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
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
