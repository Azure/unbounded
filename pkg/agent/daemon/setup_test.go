// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
)

func TestControllerRuntimeReconcilerDispatchesOperationRequest(t *testing.T) {
	t.Parallel()

	reconciler := &recordingReconciler{operationResult: ctrl.Result{Requeue: true}}
	r := &controllerRuntimeReconciler{machineOperations: reconciler, repaves: &recordingReconciler{}}

	result, err := r.Reconcile(context.Background(), NewMachineOperationRequest("op-1"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result = %v, want requeue", result)
	}
	if reconciler.operationName != "op-1" {
		t.Fatalf("operation request = %q", reconciler.operationName)
	}
}

func TestControllerRuntimeReconcilerDispatchesRepaveRequest(t *testing.T) {
	t.Parallel()

	reconciler := &recordingReconciler{repaveResult: ctrl.Result{Requeue: true}}
	r := &controllerRuntimeReconciler{machineOperations: &recordingReconciler{}, repaves: reconciler}

	result, err := r.Reconcile(context.Background(), NewRepaveRequest("node-delete"))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result = %v, want requeue", result)
	}
	if !reconciler.repaveCalled {
		t.Fatal("repave request was not dispatched")
	}
	if reconciler.repaveSource != "node-delete" {
		t.Fatalf("repave source = %q, want node-delete", reconciler.repaveSource)
	}
}

func TestControllerRuntimeReconcilerIgnoresEmptyRequest(t *testing.T) {
	t.Parallel()

	r := &controllerRuntimeReconciler{machineOperations: &recordingReconciler{}, repaves: &recordingReconciler{}}

	result, err := r.Reconcile(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Fatalf("result = %v, want empty", result)
	}
}

func TestControllerRuntimeReconcilerReturnsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("reconcile failed")
	r := &controllerRuntimeReconciler{machineOperations: &recordingReconciler{operationErr: wantErr}, repaves: &recordingReconciler{}}

	_, err := r.Reconcile(context.Background(), NewMachineOperationRequest("op-1"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestSetupControllerValidatesInputs(t *testing.T) {
	t.Parallel()

	if err := SetupController("", nil, &recordingReconciler{}, &recordingReconciler{}); err == nil {
		t.Fatal("SetupController empty name error = nil")
	}
	if err := SetupController("controller", nil, nil, &recordingReconciler{}); err == nil {
		t.Fatal("SetupController nil machine operation reconciler error = nil")
	}
	if err := SetupController("controller", nil, &recordingReconciler{}, nil); err == nil {
		t.Fatal("SetupController nil repave reconciler error = nil")
	}
}

type recordingReconciler struct {
	operationName   string
	operationResult ctrl.Result
	operationErr    error

	repaveCalled bool
	repaveSource string
	repaveResult ctrl.Result
	repaveErr    error
}

func (r *recordingReconciler) SetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] {
	return b
}

func (r *recordingReconciler) ReconcileMachineOperation(_ context.Context, name string) (ctrl.Result, error) {
	r.operationName = name
	return r.operationResult, r.operationErr
}

func (r *recordingReconciler) ReconcileRepave(_ context.Context, source string) (ctrl.Result, error) {
	r.repaveCalled = true
	r.repaveSource = source
	return r.repaveResult, r.repaveErr
}
