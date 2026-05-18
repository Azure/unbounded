// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machineopscontroller "github.com/Azure/unbounded/pkg/machineops/controller"
)

// MachineOperation is a typed host-local daemon operation request.
type MachineOperation = machineopscontroller.OperationRequest

// MachineOperationResult describes the outcome of a host-local daemon operation.
type MachineOperationResult = machineopscontroller.OperationResult

// MachineOperationHandler executes a host-local daemon operation.
type MachineOperationHandler = machineopscontroller.OperationHandler

// MachineOperationHandlers maps operation kinds to host-local MachineOperation handlers.
type MachineOperationHandlers map[machinav1alpha3.OperationKind]MachineOperationHandler

// MachineOperationStore records operation lifecycle state.
type MachineOperationStore = machineopscontroller.Store

// machineOperationRequest identifies a MachineOperation reconcile request.
type machineOperationRequest struct {
	// Name is the MachineOperation object name.
	Name string
}

// repaveRequest identifies a repave reconcile request for the local daemon target.
type repaveRequest struct {
	// Source identifies what produced the repave request, such as a Node event or poll tick.
	Source string
}

// MachineOperationRequestReconciler handles MachineOperation requests produced by the
// shared controller setup.
type MachineOperationRequestReconciler interface {
	// SetupController registers MachineOperation watches on builder.
	SetupController(*builder.TypedBuilder[Request]) *builder.TypedBuilder[Request]

	// ReconcileMachineOperation handles req, which identifies a MachineOperation
	// object queued by controller-runtime.
	ReconcileMachineOperation(context.Context, string) (ctrl.Result, error)
}

// NoopMachineOperationReconciler returns a MachineOperationRequestReconciler
// that does not register watches. Use it when a daemon controller only needs
// repave requests.
func NoopMachineOperationReconciler() MachineOperationRequestReconciler {
	return noopMachineOperationReconciler{}
}

type noopMachineOperationReconciler struct{}

func (noopMachineOperationReconciler) SetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] {
	return b
}

func (noopMachineOperationReconciler) ReconcileMachineOperation(_ context.Context, name string) (ctrl.Result, error) {
	return ctrl.Result{}, fmt.Errorf("unexpected MachineOperation request %q", name)
}

// RepaveReconciler handles repave requests produced by the shared controller setup.
type RepaveReconciler interface {
	// SetupController registers repave watches on builder.
	SetupController(*builder.TypedBuilder[Request]) *builder.TypedBuilder[Request]

	// ReconcileRepave handles req, which identifies a local repave trigger queued
	// by controller-runtime.
	ReconcileRepave(context.Context, string) (ctrl.Result, error)
}

// Request is the controller-runtime queue item used by shared daemon controller
// setup. Use NewMachineOperationRequest or NewRepaveRequest to construct one.
type Request struct {
	machineOperation *machineOperationRequest
	repave           *repaveRequest
}

// NewMachineOperationRequest returns a controller queue request for a
// MachineOperation object.
func NewMachineOperationRequest(name string) Request {
	req := machineOperationRequest{Name: name}

	return Request{machineOperation: &req}
}

// NewRepaveRequest returns a controller queue request for a repave trigger.
func NewRepaveRequest(source string) Request {
	req := repaveRequest{Source: source}

	return Request{repave: &req}
}

// machineOperationRequest returns the MachineOperation request carried by r.
func (r Request) machineOperationRequest() (machineOperationRequest, bool) {
	if r.machineOperation == nil {
		return machineOperationRequest{}, false
	}

	return *r.machineOperation, true
}

// repaveRequest returns the repave request carried by r.
func (r Request) repaveRequest() (repaveRequest, bool) {
	if r.repave == nil {
		return repaveRequest{}, false
	}

	return *r.repave, true
}
