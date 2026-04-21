// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

// mockExecutor records calls and returns a configurable error.
type mockExecutor struct {
	softRestartCalls []string
	softRestartErr   error
}

func (m *mockExecutor) softRestart(_ context.Context, _ *slog.Logger, machineName string) error {
	m.softRestartCalls = append(m.softRestartCalls, machineName)
	return m.softRestartErr
}

// stubFindActive returns a findActive function that always returns the given
// nspawn machine name.
func stubFindActive(nspawnName string) func(*slog.Logger) (*ActiveMachine, error) {
	return func(_ *slog.Logger) (*ActiveMachine, error) {
		return &ActiveMachine{Name: nspawnName}, nil
	}
}

func operationScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

func operationClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(operationScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha3.Operation{}).
		Build().(client.WithWatch)
}

func testOperation(name string, opType v1alpha3.OperationType, phase v1alpha3.OperationPhase) *v1alpha3.Operation {
	return &v1alpha3.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha3.OperationSpec{
			MachineRef: "test-machine",
			Type:       opType,
		},
		Status: v1alpha3.OperationStatus{
			Phase: phase,
		},
	}
}

func getOperation(t *testing.T, c client.Client, name string) *v1alpha3.Operation {
	t.Helper()

	var op v1alpha3.Operation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &op))

	return &op
}

func newTestReconciler(c client.WithWatch, exec *mockExecutor) *reconciler {
	return &reconciler{
		client:      c,
		machineName: "test-machine",
		exec:        exec,
		findActive:  stubFindActive("kube1"),
	}
}

func Test_reconcileOperation_PendingSoftReboot(t *testing.T) {
	op := testOperation("op-1", v1alpha3.OperationTypeSoftReboot, v1alpha3.OperationPhasePending)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-1")
	require.NoError(t, err)

	// Executor should have been called once with the nspawn name.
	assert.Equal(t, []string{"kube1"}, exec.softRestartCalls)

	// Operation should be completed with timestamps.
	result := getOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseCompleted, result.Status.Phase)
	assert.NotNil(t, result.Status.StartedAt)
	assert.NotNil(t, result.Status.CompletedAt)
	assert.Empty(t, result.Status.Message)
}

func Test_reconcileOperation_InProgressRecovery(t *testing.T) {
	// Simulates crash recovery: operation left InProgress by a previous run.
	op := testOperation("op-2", v1alpha3.OperationTypeSoftReboot, v1alpha3.OperationPhaseInProgress)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-2")
	require.NoError(t, err)

	assert.Len(t, exec.softRestartCalls, 1)

	result := getOperation(t, c, "op-2")
	assert.Equal(t, v1alpha3.OperationPhaseCompleted, result.Status.Phase)
}

func Test_reconcileOperation_SkipsTerminal(t *testing.T) {
	op := testOperation("op-3", v1alpha3.OperationTypeSoftReboot, v1alpha3.OperationPhaseCompleted)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-3")
	require.NoError(t, err)

	// Executor should not have been called.
	assert.Empty(t, exec.softRestartCalls)
}

func Test_reconcileOperation_ExecutorError(t *testing.T) {
	op := testOperation("op-4", v1alpha3.OperationTypeSoftReboot, v1alpha3.OperationPhasePending)
	c := operationClient(op)
	exec := &mockExecutor{softRestartErr: fmt.Errorf("nspawn exploded")}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nspawn exploded")

	// Operation should be marked failed with the error message.
	result := getOperation(t, c, "op-4")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, result.Status.Phase)
	assert.Contains(t, result.Status.Message, "nspawn exploded")
	assert.NotNil(t, result.Status.CompletedAt)
}

func Test_reconcileOperation_UnknownType(t *testing.T) {
	op := &v1alpha3.Operation{
		ObjectMeta: metav1.ObjectMeta{
			Name: "op-5",
		},
		Spec: v1alpha3.OperationSpec{
			MachineRef: "test-machine",
			Type:       "Explode",
		},
		Status: v1alpha3.OperationStatus{
			Phase: v1alpha3.OperationPhasePending,
		},
	}
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-5")
	require.Error(t, err)

	// Executor should not have been called.
	assert.Empty(t, exec.softRestartCalls)

	result := getOperation(t, c, "op-5")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, result.Status.Phase)
	assert.Contains(t, result.Status.Message, "unknown operation type")
}

func Test_reconcileOperation_HardRebootNotSupported(t *testing.T) {
	op := testOperation("op-6", v1alpha3.OperationTypeHardReboot, v1alpha3.OperationPhasePending)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-6")
	require.Error(t, err)

	assert.Empty(t, exec.softRestartCalls)

	result := getOperation(t, c, "op-6")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, result.Status.Phase)
	assert.Contains(t, result.Status.Message, "machina controller")
}

func Test_reconcileOperation_SkipsFailed(t *testing.T) {
	op := testOperation("op-7", v1alpha3.OperationTypeSoftReboot, v1alpha3.OperationPhaseFailed)
	op.Status.Message = "previous failure"
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-7")
	require.NoError(t, err)

	assert.Empty(t, exec.softRestartCalls)
}
