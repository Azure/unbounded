// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Operation declares one operation kind handled by a MachineOperation controller.
type Operation struct {
	// Kind is the MachineOperation kind handled by Handler.
	Kind machinav1alpha3.OperationKind

	// Handler executes Kind.
	Handler OperationHandler

	// ReexecuteInProgress allows Handler to run when the operation is already
	// InProgress. The default is to execute only empty or Pending operations.
	ReexecuteInProgress bool
}

// OperationRequest is the generic controller-facing view of a MachineOperation.
// Object and Machine are read-only context for handlers; status writes should
// go through Store.
type OperationRequest struct {
	// Object is the raw MachineOperation object fetched for this reconcile.
	Object *machinav1alpha3.MachineOperation

	// Machine is the resolved target Machine when the controller's target
	// resolver resolved one.
	Machine *machinav1alpha3.Machine

	// Name is the MachineOperation object name.
	Name string

	// UID is the MachineOperation object UID.
	UID types.UID

	// Kind is the requested operation kind.
	Kind machinav1alpha3.OperationKind

	// Parameters contains operation-specific inputs.
	Parameters map[string]string
}

// OperationResult describes the MachineOperation status to record.
type OperationResult struct {
	// Phase is the resulting operation phase.
	Phase machinav1alpha3.OperationPhase

	// Reason is a stable, machine-readable reason for the result.
	Reason string

	// Message is a human-readable result description.
	Message string

	// ObservedMachineGeneration records the Machine generation acted on by the operation.
	ObservedMachineGeneration int64
}

// OperationHandler executes a MachineOperation.
type OperationHandler func(context.Context, Store, OperationRequest) (ctrl.Result, error)

// Store records operation lifecycle state.
type Store interface {
	// MarkInProgress records that op has started execution with message as the
	// human-readable in-progress status.
	MarkInProgress(context.Context, OperationRequest, string) error

	// Finish records the terminal or non-terminal result for op.
	Finish(context.Context, OperationRequest, OperationResult) error
}

// TargetDecision describes whether a controller owns a MachineOperation target.
type TargetDecision string

const (
	// TargetIgnore leaves the operation untouched so another controller can own it.
	TargetIgnore TargetDecision = "Ignore"

	// TargetClaim means this controller owns the target and should dispatch to
	// a registered operation handler.
	TargetClaim TargetDecision = "Claim"

	// TargetFail marks the operation Failed with TargetResult's reason and message.
	TargetFail TargetDecision = "Fail"
)

// TargetResult is returned by a TargetResolver.
type TargetResult struct {
	Decision TargetDecision
	Machine  *machinav1alpha3.Machine
	Reason   string
	Message  string
}

// TargetResolver decides whether this controller owns an operation's target. It
// may resolve and return a Machine for handlers that need one.
type TargetResolver interface {
	ResolveTarget(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error)
}

// TargetResolverFunc adapts a function into a TargetResolver.
type TargetResolverFunc func(context.Context, *machinav1alpha3.MachineOperation) (TargetResult, error)

// ResolveTarget calls f.
func (f TargetResolverFunc) ResolveTarget(ctx context.Context, op *machinav1alpha3.MachineOperation) (TargetResult, error) {
	return f(ctx, op)
}

// UnsupportedOperationPolicy controls what happens when this controller owns a
// target but has no handler for the requested operation kind.
type UnsupportedOperationPolicy string

const (
	// UnsupportedOperationIgnore leaves unsupported claimed operations untouched.
	UnsupportedOperationIgnore UnsupportedOperationPolicy = "Ignore"

	// UnsupportedOperationFail marks unsupported claimed operations Failed.
	UnsupportedOperationFail UnsupportedOperationPolicy = "Fail"
)

type operationRegistry map[machinav1alpha3.OperationKind]Operation

func newOperationRegistry(operations []Operation) (operationRegistry, error) {
	if len(operations) == 0 {
		return nil, fmt.Errorf("machine operations are required")
	}
	registry := make(operationRegistry, len(operations))
	for _, operation := range operations {
		if operation.Kind == "" {
			return nil, fmt.Errorf("machine operation kind is required")
		}
		if operation.Handler == nil {
			return nil, fmt.Errorf("machine operation handler %s is required", operation.Kind)
		}
		if _, exists := registry[operation.Kind]; exists {
			return nil, fmt.Errorf("duplicate machine operation %s", operation.Kind)
		}
		registry[operation.Kind] = operation
	}

	return registry, nil
}
