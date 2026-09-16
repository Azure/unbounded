// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestDaemonStartupRunsLifecycleMigrationBeforeControllerSetup(t *testing.T) {
	t.Parallel()

	op := &fakeNodeOperator{active: &ActiveMachine{
		Name:   "kube1",
		Config: &provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"},
	}}
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op, installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))})
	require.ErrorContains(t, err, "build daemon controller credentials")
	require.Equal(t, 1, op.lifecycleCalls)
}

func TestStartupLockWaitHonorsDeadlineWithoutMigration(t *testing.T) {
	t.Parallel()
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	lock, err := store.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	op := &fakeNodeOperator{}
	_, err = discoverAndMigrate(ctx, discardLogger(), store, op)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, op.lifecycleCalls)
}

func TestDaemonStartupFailsLifecycleMigrationWithoutRetry(t *testing.T) {
	t.Parallel()

	resolveErr := errors.New("resolve lifecycle configuration")
	op := &fakeNodeOperator{
		active: &ActiveMachine{
			Name:   "kube1",
			Config: &provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"},
		},
		lifecycleErrs: []error{resolveErr},
	}
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op, installation: installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))})
	require.ErrorIs(t, err, resolveErr)
	require.Equal(t, 1, op.lifecycleCalls)
}
