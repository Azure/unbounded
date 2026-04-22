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
	softRebootCalls []string
	softRebootErr   error
}

func (m *mockExecutor) softReboot(_ context.Context, _ *slog.Logger, machineName string) error {
	m.softRebootCalls = append(m.softRebootCalls, machineName)
	return m.softRebootErr
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
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		Build().(client.WithWatch)
}

func testMachineOperation(name string, opName v1alpha3.OperationName, phase v1alpha3.OperationPhase) *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				v1alpha3.MachineOperationMachineLabelKey: "test-machine",
			},
		},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationName: opName,
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase: phase,
		},
	}
}

func getMachineOperation(t *testing.T, c client.Client, name string) *v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
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

func Test_reconcileOperation_PendingReboot(t *testing.T) {
	op := testMachineOperation("op-1", v1alpha3.OperationReboot, v1alpha3.OperationPhasePending)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-1")
	require.NoError(t, err)

	// Executor should have been called once with the nspawn name.
	assert.Equal(t, []string{"kube1"}, exec.softRebootCalls)

	// Operation should be completed with timestamps.
	result := getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseComplete, result.Status.Phase)
	assert.NotNil(t, result.Status.StartedAt)
	assert.NotNil(t, result.Status.CompletedAt)
	assert.Empty(t, result.Status.Message)
}

func Test_reconcileOperation_InProgressRecovery(t *testing.T) {
	// Simulates crash recovery: operation left InProgress by a previous run.
	op := testMachineOperation("op-2", v1alpha3.OperationReboot, v1alpha3.OperationPhaseInProgress)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-2")
	require.NoError(t, err)

	assert.Len(t, exec.softRebootCalls, 1)

	result := getMachineOperation(t, c, "op-2")
	assert.Equal(t, v1alpha3.OperationPhaseComplete, result.Status.Phase)
}

func Test_reconcileOperation_SkipsTerminal(t *testing.T) {
	op := testMachineOperation("op-3", v1alpha3.OperationReboot, v1alpha3.OperationPhaseComplete)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-3")
	require.NoError(t, err)

	// Executor should not have been called.
	assert.Empty(t, exec.softRebootCalls)
}

func Test_reconcileOperation_ExecutorError(t *testing.T) {
	op := testMachineOperation("op-4", v1alpha3.OperationReboot, v1alpha3.OperationPhasePending)
	c := operationClient(op)
	exec := &mockExecutor{softRebootErr: fmt.Errorf("nspawn exploded")}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nspawn exploded")

	// Operation should be marked failed with the error message.
	result := getMachineOperation(t, c, "op-4")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, result.Status.Phase)
	assert.Contains(t, result.Status.Message, "nspawn exploded")
	assert.NotNil(t, result.Status.CompletedAt)
}

func Test_reconcileOperation_UnknownOperationIgnored(t *testing.T) {
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: "op-5",
			Labels: map[string]string{
				v1alpha3.MachineOperationMachineLabelKey: "test-machine",
			},
		},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationName: "Explode",
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhasePending,
		},
	}
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-5")
	require.NoError(t, err)

	// Executor should not have been called.
	assert.Empty(t, exec.softRebootCalls)

	// Status should be unchanged - agent ignores operations it does not handle.
	result := getMachineOperation(t, c, "op-5")
	assert.Equal(t, v1alpha3.OperationPhasePending, result.Status.Phase)
}

func Test_reconcileOperation_HardRebootIgnored(t *testing.T) {
	op := testMachineOperation("op-6", v1alpha3.OperationHardReboot, v1alpha3.OperationPhasePending)
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-6")
	require.NoError(t, err)

	assert.Empty(t, exec.softRebootCalls)

	// Status should be unchanged - left for the machina controller.
	result := getMachineOperation(t, c, "op-6")
	assert.Equal(t, v1alpha3.OperationPhasePending, result.Status.Phase)
}

func Test_reconcileOperation_SkipsFailed(t *testing.T) {
	op := testMachineOperation("op-7", v1alpha3.OperationReboot, v1alpha3.OperationPhaseFailed)
	op.Status.Message = "previous failure"
	c := operationClient(op)
	exec := &mockExecutor{}
	r := newTestReconciler(c, exec)

	err := r.reconcileOperation(context.Background(), discardLogger(), "op-7")
	require.NoError(t, err)

	assert.Empty(t, exec.softRebootCalls)
}
