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

	called := false
	reconciler := &daemonReconciler{
		Client:      fakeStatusClient(machine, op),
		log:         discardLogger(),
		machineName: "test-machine",
		restartActiveNode: func(context.Context, *slog.Logger) error {
			called = true
			return nil
		},
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, called)

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

	reconciler := &daemonReconciler{
		Client:      fakeStatusClient(machine, op),
		log:         discardLogger(),
		machineName: "test-machine",
		restartActiveNode: func(context.Context, *slog.Logger) error {
			return errors.New("restart failed")
		},
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
