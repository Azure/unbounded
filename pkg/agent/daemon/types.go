// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// MachineOperation is a typed host-local daemon operation request.
type MachineOperation struct {
	// Name is the operation name in its source system.
	Name string

	// Kind is the requested host-local operation kind.
	Kind machinav1alpha3.OperationKind

	// Parameters contains operation-specific inputs, such as an AgentUpgrade download URL.
	Parameters map[string]string
}

// MachineOperationResult describes the outcome of a host-local daemon operation.
type MachineOperationResult[TGeneration comparable] struct {
	// Phase is the resulting operation phase.
	Phase machinav1alpha3.OperationPhase

	// Reason is a stable, machine-readable reason for the result.
	Reason string

	// Message is a human-readable result description.
	Message string

	// ObservedMachineGeneration records the machine generation acted on by the operation.
	ObservedMachineGeneration TGeneration
}

// MachineOperationHandler executes a host-local daemon operation.
type MachineOperationHandler[TGeneration comparable] func(context.Context, MachineOperationStore[TGeneration], MachineOperation) (ctrl.Result, error)

// MachineOperationStore records operation lifecycle state.
type MachineOperationStore[TGeneration comparable] interface {
	// MarkInProgress records that op has started execution with message as the
	// human-readable in-progress status.
	MarkInProgress(context.Context, MachineOperation, string) error

	// Finish records the terminal or non-terminal result for op. The result
	// carries the desired phase, reason, message, observed generation, and any
	// controller-runtime requeue request.
	Finish(context.Context, MachineOperation, MachineOperationResult[TGeneration]) error
}

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
// that does not register watches and ignores MachineOperation requests. Use it
// when a daemon controller only needs repave requests.
func NoopMachineOperationReconciler() MachineOperationRequestReconciler {
	return noopMachineOperationReconciler{}
}

type noopMachineOperationReconciler struct{}

func (noopMachineOperationReconciler) SetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] {
	return b
}

func (noopMachineOperationReconciler) ReconcileMachineOperation(context.Context, string) (ctrl.Result, error) {
	return ctrl.Result{}, nil
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
