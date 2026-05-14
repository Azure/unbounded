// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
)

// Machine describes the abstract machine being reconciled and its current
// host-local nspawn placement.
type Machine[TApplied any, TDesired any, TGeneration comparable] struct {
	// MachineName is the abstract machine entity name, such as a Kubernetes Machine CR.
	MachineName string

	// NodeName is the Kubernetes Node name for this machine, when one exists.
	NodeName string

	// Generation is the observed generation of the abstract machine entity.
	Generation TGeneration

	// NSpawnMachineName is the concrete host-local nspawn machine name, such as kube1 or kube2.
	NSpawnMachineName string

	// Applied is consumer-defined applied machine state.
	Applied TApplied

	// Desired is consumer-defined desired machine state.
	Desired TDesired
}

// Operation is a typed host-local daemon operation request.
type Operation[TApplied any, TDesired any, TGeneration comparable] struct {
	// Ref contains the non-generic operation identity used for status updates.
	Ref OperationRef[TGeneration]

	// Parameters contains operation-specific inputs, such as an AgentUpgrade download URL.
	Parameters map[string]string

	// Machine contains the typed machine inputs needed by operation handlers.
	Machine Machine[TApplied, TDesired, TGeneration]
}

// OperationRef identifies an operation without carrying typed applied or desired state.
type OperationRef[TGeneration comparable] struct {
	// Name is the operation name in its source system.
	Name string

	// Kind is the requested host-local operation kind.
	Kind machinav1alpha3.OperationKind

	// MachineName is the abstract machine entity name targeted by this operation.
	MachineName string

	// ObservedMachineGeneration is the generation observed when executing the operation.
	ObservedMachineGeneration TGeneration
}

// OperationResult describes the outcome of a host-local daemon operation.
type OperationResult[TGeneration comparable] struct {
	// Result is the controller-runtime scheduling result for this operation.
	Result ctrl.Result

	// Phase is the resulting operation phase. A terminal phase should finish the
	// operation status; a non-terminal phase leaves completion to a later signal.
	Phase machinav1alpha3.OperationPhase

	// Reason is a stable, machine-readable reason for the result.
	Reason string

	// Message is a human-readable result description.
	Message string

	// ObservedMachineGeneration records the machine generation acted on by the operation.
	ObservedMachineGeneration TGeneration
}

// OperationHandler executes a host-local daemon operation.
type OperationHandler[TApplied any, TDesired any, TGeneration comparable] func(context.Context, Operation[TApplied, TDesired, TGeneration]) (OperationResult[TGeneration], error)

// Handlers maps operation kinds to their host-local daemon handlers.
type Handlers[TApplied any, TDesired any, TGeneration comparable] map[machinav1alpha3.OperationKind]OperationHandler[TApplied, TDesired, TGeneration]

// OperationStore records operation lifecycle state. It intentionally uses OperationRef
// instead of Operation so controller-side status code does not depend on typed
// applied or desired state.
type OperationStore[TGeneration comparable] interface {
	// MarkInProgress records that ref has started execution with message as the
	// human-readable in-progress status.
	MarkInProgress(context.Context, OperationRef[TGeneration], string) error

	// Finish records the terminal or non-terminal result for ref. The result
	// carries the desired phase, reason, message, observed generation, and any
	// controller-runtime requeue request.
	Finish(context.Context, OperationRef[TGeneration], OperationResult[TGeneration]) error
}

// MachineOperationRequest identifies a MachineOperation reconcile request.
type MachineOperationRequest struct {
	// Name is the MachineOperation object name.
	Name string
}

// RepaveRequest identifies a repave reconcile request for the local daemon target.
type RepaveRequest struct {
	// MachineName is the abstract machine entity name, such as a Kubernetes Machine CR.
	MachineName string

	// NodeName is the Kubernetes Node name that produced the repave signal, when applicable.
	NodeName string
}

// MachineOperationReconciler handles MachineOperation requests produced by the
// shared controller setup.
type MachineOperationReconciler interface {
	// ReconcileMachineOperation handles req, which identifies a MachineOperation
	// object queued by controller-runtime.
	ReconcileMachineOperation(context.Context, MachineOperationRequest) (ctrl.Result, error)
}

// RepaveReconciler handles repave requests produced by the shared controller setup.
type RepaveReconciler interface {
	// ReconcileRepave handles req, which identifies a local repave trigger queued
	// by controller-runtime.
	ReconcileRepave(context.Context, RepaveRequest) (ctrl.Result, error)
}

// Request is the controller-runtime queue item used by shared daemon controller
// setup. Use NewMachineOperationRequest or NewRepaveRequest to construct one.
type Request struct {
	machineOperation *MachineOperationRequest
	repave           *RepaveRequest
}

// NewMachineOperationRequest returns a controller queue request for a
// MachineOperation object.
func NewMachineOperationRequest(req MachineOperationRequest) Request {
	return Request{machineOperation: &req}
}

// NewRepaveRequest returns a controller queue request for a repave trigger.
func NewRepaveRequest(req RepaveRequest) Request {
	return Request{repave: &req}
}

// machineOperationRequest returns the MachineOperation request carried by r.
func (r Request) machineOperationRequest() (MachineOperationRequest, bool) {
	if r.machineOperation == nil {
		return MachineOperationRequest{}, false
	}

	return *r.machineOperation, true
}

// repaveRequest returns the repave request carried by r.
func (r Request) repaveRequest() (RepaveRequest, bool) {
	if r.repave == nil {
		return RepaveRequest{}, false
	}

	return *r.repave, true
}

// SetupController registers product-specific watches on builder and returns the
// updated builder. Implementations enqueue Request values.
type SetupController func(*builder.TypedBuilder[Request]) *builder.TypedBuilder[Request]
