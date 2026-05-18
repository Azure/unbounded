// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestReconcilerDispatchesClaimedOperation(t *testing.T) {
	t.Parallel()

	machine := &machinav1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 7}}
	op := newTestOperation("op-1", machinav1alpha3.OperationNodeReboot)
	handler := &recordingHandler{result: ctrl.Result{RequeueAfter: time.Second}}
	c := newTestClient(machine, op)
	reconciler := newTestReconciler(t, c, []Operation{{Kind: machinav1alpha3.OperationNodeReboot, Handler: handler.handle}}, func(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error) {
		return TargetResult{Decision: TargetClaim, Machine: machine}, nil
	})

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, time.Second, result.RequeueAfter)
	require.True(t, handler.called)
	require.Equal(t, "op-1", handler.request.Name)
	require.Equal(t, machinav1alpha3.OperationNodeReboot, handler.request.Kind)
	require.Equal(t, machine, handler.request.Machine)
}

func TestReconcilerTargetFailMarksOperationFailed(t *testing.T) {
	t.Parallel()

	op := newTestOperation("op-1", machinav1alpha3.OperationHostReboot)
	c := newTestClient(op)
	reconciler := newTestReconciler(t, c, []Operation{{Kind: machinav1alpha3.OperationHostReboot, Handler: (&recordingHandler{}).handle}}, func(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error) {
		return TargetResult{Decision: TargetFail, Reason: "InvalidSpec", Message: "spec.machineRef is required"}, nil
	})

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated machinav1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, machinav1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Equal(t, "spec.machineRef is required", updated.Status.Message)

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "InvalidSpec", cond.Reason)
}

func TestReconcilerTargetFailRequeuesTerminalCleanup(t *testing.T) {
	t.Parallel()

	ttl := int32(30)
	op := newTestOperation("op-1", machinav1alpha3.OperationHostReboot)
	op.Spec.TTLSecondsAfterFinished = &ttl
	reconciler := newTestReconciler(t, newTestClient(op), []Operation{{Kind: machinav1alpha3.OperationHostReboot, Handler: (&recordingHandler{}).handle}}, func(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error) {
		return TargetResult{Decision: TargetFail, Reason: "InvalidSpec", Message: "spec.machineRef is required"}, nil
	})

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, result.RequeueAfter)
}

func TestReconcilerSkipsInProgressByDefault(t *testing.T) {
	t.Parallel()

	op := newTestOperation("op-1", machinav1alpha3.OperationHostReboot)
	op.Status.Phase = machinav1alpha3.OperationPhaseInProgress
	handler := &recordingHandler{}
	reconciler := newTestReconciler(t, newTestClient(op), []Operation{{Kind: machinav1alpha3.OperationHostReboot, Handler: handler.handle}}, handleAll)

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
	require.False(t, handler.called)
}

func TestReconcilerReexecutesConfiguredInProgressOperation(t *testing.T) {
	t.Parallel()

	op := newTestOperation("op-1", machinav1alpha3.OperationHostReplace)
	op.Status.Phase = machinav1alpha3.OperationPhaseInProgress
	handler := &recordingHandler{}
	c := newTestClient(op)
	reconciler, err := NewReconciler(Config{
		Client:         c,
		Operations:     []Operation{{Kind: machinav1alpha3.OperationHostReplace, Handler: handler.handle, ReexecuteInProgress: true}},
		TargetResolver: TargetResolverFunc(handleAll),
		Now:            fixedNow,
	})
	require.NoError(t, err)

	_, err = reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.True(t, handler.called)
}

func TestStoreMarkInProgressAndFinish(t *testing.T) {
	t.Parallel()

	op := newTestOperation("op-1", machinav1alpha3.OperationNodeReboot)
	c := newTestClient(op)
	reconciler := newTestReconciler(t, c, []Operation{{
		Kind: machinav1alpha3.OperationNodeReboot,
		Handler: func(ctx context.Context, store Store, request OperationRequest) (ctrl.Result, error) {
			require.NoError(t, store.MarkInProgress(ctx, request, "restarting node"))
			return ctrl.Result{}, store.Finish(ctx, request, OperationResult{
				Phase:                     machinav1alpha3.OperationPhaseComplete,
				Reason:                    "Succeeded",
				Message:                   "NodeReboot completed",
				ObservedMachineGeneration: 12,
			})
		},
	}}, handleAll)

	_, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)

	var updated machinav1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, machinav1alpha3.OperationPhaseComplete, updated.Status.Phase)
	require.Equal(t, int64(12), updated.Status.ObservedMachineGeneration)
	require.True(t, updated.Status.StartedAt.Time.Equal(fixedNow().Time))
	require.True(t, updated.Status.CompletedAt.Time.Equal(fixedNow().Time))

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Succeeded", cond.Reason)
}

func TestReconcilerDeletesExpiredTerminalOperation(t *testing.T) {
	t.Parallel()

	completedAt := metav1.NewTime(time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC))
	ttl := int32(30)
	op := newTestOperation("op-1", machinav1alpha3.OperationHostReboot)
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = machinav1alpha3.OperationPhaseComplete
	op.Status.CompletedAt = &completedAt
	c := newTestClient(op)
	reconciler := newTestReconcilerWithNow(t, c, []Operation{{Kind: machinav1alpha3.OperationHostReboot, Handler: (&recordingHandler{}).handle}}, handleAll, func() metav1.Time {
		return metav1.NewTime(completedAt.Add(31 * time.Second))
	})

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated machinav1alpha3.MachineOperation
	require.Error(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
}

func TestReconcilerIgnoresUnclaimedTerminalOperation(t *testing.T) {
	t.Parallel()

	completedAt := metav1.NewTime(time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC))
	ttl := int32(30)
	op := newTestOperation("op-1", machinav1alpha3.OperationHostReboot)
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = machinav1alpha3.OperationPhaseComplete
	op.Status.CompletedAt = &completedAt
	c := newTestClient(op)
	reconciler := newTestReconcilerWithNow(t, c, []Operation{{Kind: machinav1alpha3.OperationHostReboot, Handler: (&recordingHandler{}).handle}}, func(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error) {
		return TargetResult{Decision: TargetIgnore}, nil
	}, func() metav1.Time {
		return metav1.NewTime(completedAt.Add(31 * time.Second))
	})

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	var updated machinav1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	require.Equal(t, machinav1alpha3.OperationPhaseComplete, updated.Status.Phase)
}

func TestReconcilerRequeuesUnexpiredTerminalOperation(t *testing.T) {
	t.Parallel()

	completedAt := metav1.NewTime(time.Date(2026, 4, 28, 10, 0, 0, 0, time.UTC))
	ttl := int32(30)
	op := newTestOperation("op-1", machinav1alpha3.OperationHostReboot)
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = machinav1alpha3.OperationPhaseComplete
	op.Status.CompletedAt = &completedAt
	reconciler := newTestReconcilerWithNow(t, newTestClient(op), []Operation{{Kind: machinav1alpha3.OperationHostReboot, Handler: (&recordingHandler{}).handle}}, handleAll, func() metav1.Time {
		return metav1.NewTime(completedAt.Add(10 * time.Second))
	})

	result, err := reconciler.ReconcileName(context.Background(), "op-1")
	require.NoError(t, err)
	require.Equal(t, 20*time.Second, result.RequeueAfter)
}

func TestShouldReconcileOperation(t *testing.T) {
	t.Parallel()

	completedAt := metav1.Now()
	ttl := int32(30)

	tests := []struct {
		name string
		op   *machinav1alpha3.MachineOperation
		want bool
	}{
		{
			name: "non terminal",
			op:   newTestOperation("op-1", machinav1alpha3.OperationHostReboot),
			want: true,
		},
		{
			name: "terminal without ttl",
			op: &machinav1alpha3.MachineOperation{
				Status: machinav1alpha3.MachineOperationStatus{Phase: machinav1alpha3.OperationPhaseComplete},
			},
			want: false,
		},
		{
			name: "terminal with ttl and completion time",
			op: &machinav1alpha3.MachineOperation{
				Spec: machinav1alpha3.MachineOperationSpec{TTLSecondsAfterFinished: &ttl},
				Status: machinav1alpha3.MachineOperationStatus{
					Phase:       machinav1alpha3.OperationPhaseComplete,
					CompletedAt: &completedAt,
				},
			},
			want: true,
		},
		{
			name: "terminal with ttl but no completion time",
			op: &machinav1alpha3.MachineOperation{
				Spec:   machinav1alpha3.MachineOperationSpec{TTLSecondsAfterFinished: &ttl},
				Status: machinav1alpha3.MachineOperationStatus{Phase: machinav1alpha3.OperationPhaseComplete},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, ShouldReconcileOperation(tt.op))
		})
	}
}

func TestSelectorMatches(t *testing.T) {
	t.Parallel()

	matches, err := SelectorMatches(
		&metav1.LabelSelector{MatchLabels: map[string]string{"pool": "edge"}},
		map[string]string{"pool": "edge", "zone": "a"},
	)
	require.NoError(t, err)
	require.True(t, matches)

	matches, err = SelectorMatches(
		&metav1.LabelSelector{MatchLabels: map[string]string{"pool": "cloud"}},
		map[string]string{"pool": "edge"},
	)
	require.NoError(t, err)
	require.False(t, matches)
}

func handleAll(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error) {
	return TargetResult{Decision: TargetClaim}, nil
}

type recordingHandler struct {
	called  bool
	request OperationRequest
	result  ctrl.Result
	err     error
}

func (h *recordingHandler) handle(_ context.Context, _ Store, request OperationRequest) (ctrl.Result, error) {
	h.called = true
	h.request = request
	return h.result, h.err
}

func newTestReconciler(
	t *testing.T,
	c client.Client,
	operations []Operation,
	resolver TargetResolverFunc,
) *Reconciler {
	t.Helper()

	return newTestReconcilerWithNow(t, c, operations, resolver, fixedNow)
}

func newTestReconcilerWithNow(
	t *testing.T,
	c client.Client,
	operations []Operation,
	resolver TargetResolverFunc,
	now func() metav1.Time,
) *Reconciler {
	t.Helper()

	reconciler, err := NewReconciler(Config{
		Client:         c,
		Operations:     operations,
		TargetResolver: resolver,
		Now:            now,
	})
	require.NoError(t, err)

	return reconciler
}

func newTestOperation(name string, operation machinav1alpha3.OperationKind) *machinav1alpha3.MachineOperation {
	return &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec: machinav1alpha3.MachineOperationSpec{
			OperationKind: operation,
		},
	}
}

func newTestClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = machinav1alpha3.AddToScheme(scheme)

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(objs...).
		Build()
}

func fixedNow() metav1.Time {
	return metav1.NewTime(time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
}
