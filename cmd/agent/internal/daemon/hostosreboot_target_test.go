// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type nativeBootFailureStore struct {
	operation daemon.MachineOperation
	result    daemon.MachineOperationResult[int64]
}

func (*nativeBootFailureStore) MarkInProgress(context.Context, daemon.MachineOperation, string) error {
	panic("unexpected MarkInProgress")
}

func (s *nativeBootFailureStore) Finish(_ context.Context, op daemon.MachineOperation, result daemon.MachineOperationResult[int64]) error {
	s.operation = op
	s.result = result

	return nil
}

func TestNativeHostSoftRebootForwardsCallerStore(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"))
	host := &fakeNodeOperator{bootIDErr: errors.New("boot failed")}
	target := &machineOperationTarget{
		installation: installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
		Client:       c,
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: host,
	}
	store := &nativeBootFailureStore{}
	op := daemon.MachineOperation{Name: "op-1", Kind: v1alpha3.OperationHostSoftReboot}

	_, err := target.reconcileHostSoftReboot(t.Context(), store, op)
	require.NoError(t, err)
	require.Equal(t, op, store.operation)
	require.Equal(t, v1alpha3.OperationPhaseFailed, store.result.Phase)
	require.Equal(t, "ExecutionFailed", store.result.Reason)
	require.Equal(t, "boot failed", store.result.Message)
	require.Empty(t, getMachineOperation(t, c, "op-1").Status.Phase)
}

func TestHostSoftRebootRequiresNativeMachineBeforeBootRead(t *testing.T) {
	op := &fakeNodeOperator{bootIDErr: assert.AnError}
	c := fakeStatusClient(hostSoftRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(t.Context(), "op-1")
	require.Error(t, err)
	assert.Zero(t, op.rebootCalls)
	assert.Empty(t, getMachineOperation(t, c, "op-1").Status.Phase)
}
