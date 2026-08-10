// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestDaemonStartupRunsLifecycleMigrationBeforeControllerSetup(t *testing.T) {
	t.Parallel()

	op := &fakeNodeOperator{active: &ActiveMachine{
		Name:   "kube1",
		Config: &provision.AgentConfig{MachineName: "machine-1", NodeName: "node-1"},
	}}
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op})
	require.ErrorContains(t, err, "build daemon controller credentials")
	require.Equal(t, 1, op.lifecycleCalls)
}

func TestEnsureLifecycleMigrationRetriesUnavailableNVIDIA(t *testing.T) {
	t.Parallel()

	op := &fakeNodeOperator{lifecycleErrs: []error{
		fmt.Errorf("discover GPU: %w", goalstates.ErrNVIDIAStateUnavailable),
		nil,
	}}
	err := ensureLifecycleMigration(
		context.Background(),
		discardLogger(),
		op,
		&ActiveMachine{Name: "kube1"},
		time.Millisecond,
	)
	require.NoError(t, err)
	require.Equal(t, 2, op.lifecycleCalls)
}

func TestEnsureLifecycleMigrationFailsNonRetryableError(t *testing.T) {
	t.Parallel()

	resolveErr := errors.New("resolve lifecycle configuration")
	op := &fakeNodeOperator{lifecycleErrs: []error{resolveErr}}
	err := ensureLifecycleMigration(
		context.Background(),
		discardLogger(),
		op,
		&ActiveMachine{Name: "kube1"},
		time.Millisecond,
	)
	require.ErrorIs(t, err, resolveErr)
	require.Equal(t, 1, op.lifecycleCalls)
}
