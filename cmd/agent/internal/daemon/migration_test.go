// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
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
	err := run(context.Background(), discardLogger(), runOptions{NodeOperator: op})
	require.ErrorIs(t, err, resolveErr)
	require.Equal(t, 1, op.lifecycleCalls)
}
