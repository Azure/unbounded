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

	result, err := r.Reconcile(context.Background(), NewMachineOperationRequest(MachineOperationRequest{Name: "op-1"}))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result = %v, want requeue", result)
	}
	if reconciler.operationReq.Name != "op-1" {
		t.Fatalf("operation request = %#v", reconciler.operationReq)
	}
}

func TestControllerRuntimeReconcilerDispatchesRepaveRequest(t *testing.T) {
	t.Parallel()

	reconciler := &recordingReconciler{repaveResult: ctrl.Result{Requeue: true}}
	r := &controllerRuntimeReconciler{machineOperations: &recordingReconciler{}, repaves: reconciler}

	result, err := r.Reconcile(context.Background(), NewRepaveRequest(RepaveRequest{MachineName: "machine-1", NodeName: "node-1"}))
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("result = %v, want requeue", result)
	}
	if reconciler.repaveReq.MachineName != "machine-1" || reconciler.repaveReq.NodeName != "node-1" {
		t.Fatalf("repave request = %#v", reconciler.repaveReq)
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

	_, err := r.Reconcile(context.Background(), NewMachineOperationRequest(MachineOperationRequest{Name: "op-1"}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestSetupWithManagerValidatesInputs(t *testing.T) {
	t.Parallel()

	if err := SetupWithManager("", nil, &recordingReconciler{}, &recordingReconciler{}, nonNilSetupController); err == nil {
		t.Fatal("SetupWithManager empty name error = nil")
	}
	if err := SetupWithManager("controller", nil, nil, &recordingReconciler{}, nonNilSetupController); err == nil {
		t.Fatal("SetupWithManager nil machine operation reconciler error = nil")
	}
	if err := SetupWithManager("controller", nil, &recordingReconciler{}, nil, nonNilSetupController); err == nil {
		t.Fatal("SetupWithManager nil repave reconciler error = nil")
	}
	if err := SetupWithManager("controller", nil, &recordingReconciler{}, &recordingReconciler{}); err == nil {
		t.Fatal("SetupWithManager empty callbacks error = nil")
	}
	if err := SetupWithManager("controller", nil, &recordingReconciler{}, &recordingReconciler{}, nil); err == nil {
		t.Fatal("SetupWithManager nil callback error = nil")
	}
}

func nonNilSetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] { return b }

type recordingReconciler struct {
	operationReq    MachineOperationRequest
	operationResult ctrl.Result
	operationErr    error

	repaveReq    RepaveRequest
	repaveResult ctrl.Result
	repaveErr    error
}

func (r *recordingReconciler) ReconcileMachineOperation(_ context.Context, req MachineOperationRequest) (ctrl.Result, error) {
	r.operationReq = req
	return r.operationResult, r.operationErr
}

func (r *recordingReconciler) ReconcileRepave(_ context.Context, req RepaveRequest) (ctrl.Result, error) {
	r.repaveReq = req
	return r.repaveResult, r.repaveErr
}
