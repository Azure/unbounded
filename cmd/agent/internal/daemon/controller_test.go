// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type fakeNodeOperator struct {
	active  *ActiveMachine
	findErr error

	restartCalled bool
	restartActive *ActiveMachine
	restartErr    error

	resetCalled bool
	resetErr    error

	stopCalled bool
	stopErr    error
}

func (op *fakeNodeOperator) FindActiveMachine(*slog.Logger) (*ActiveMachine, error) {
	if op.findErr != nil {
		return nil, op.findErr
	}

	return op.active, nil
}

func (op *fakeNodeOperator) RestartNode(_ context.Context, _ *slog.Logger, active *ActiveMachine) error {
	op.restartCalled = true
	op.restartActive = active

	return op.restartErr
}

func (op *fakeNodeOperator) ResetAgent(context.Context, *slog.Logger) error {
	op.resetCalled = true

	return op.resetErr
}

func (op *fakeNodeOperator) StopDaemon(context.Context, *slog.Logger) error {
	op.stopCalled = true

	return op.stopErr
}

func fakeStatusClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(objs...).
		Build()
}

func TestReconcileNodeReboot_Complete(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	active := &ActiveMachine{Name: "kube1"}
	opImpl := &fakeNodeOperator{active: active}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, op),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: opImpl,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, opImpl.restartCalled)
	assert.Same(t, active, opImpl.restartActive)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "NodeReboot completed", updated.Status.Message)
	assert.Equal(t, int64(7), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileNodeReboot_Failed(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	opImpl := &fakeNodeOperator{restartErr: errors.New("restart failed")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, op),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: opImpl,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "restart failed", updated.Status.Message)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileAgentReset_Complete(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentReset,
		},
	}

	opImpl := &fakeNodeOperator{}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, op),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: opImpl,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, opImpl.resetCalled)
	assert.True(t, opImpl.stopCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "AgentReset completed", updated.Status.Message)
	assert.Equal(t, int64(7), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileAgentReset_Failed(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentReset,
		},
	}

	opImpl := &fakeNodeOperator{resetErr: errors.New("reset failed")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, op),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: opImpl,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "reset failed", updated.Status.Message)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestShouldEnqueueMachineOperation_AgentReset(t *testing.T) {
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentReset,
		},
	}

	matches := shouldEnqueueMachineOperation(context.Background(), fakeStatusClient(op), discardLogger(), "test-machine", "test-node", op)
	assert.True(t, matches)
}
