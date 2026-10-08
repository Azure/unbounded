// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"path/filepath"
	"testing"

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

	t.Run("runs with ownership", func(t *testing.T) {
		t.Parallel()

		op := &fakeNodeOperator{}
		reconcileHostRootUnderLock(t.Context(), discardLogger(), newStore(t), op, active)

		assert.Equal(t, 1, op.hostRootCalls)
		assert.Same(t, active, op.hostRootActive)
	})

	t.Run("skipped while an installation is unfinished", func(t *testing.T) {
		t.Parallel()

		store := newStore(t)
		record, err := installstate.NewRecord("machine-1", "fingerprint")
		require.NoError(t, err)
		require.NoError(t, store.Save(record))

		op := &fakeNodeOperator{}
		reconcileHostRootUnderLock(t.Context(), discardLogger(), store, op, active)

		assert.Zero(t, op.hostRootCalls)
	})
}
