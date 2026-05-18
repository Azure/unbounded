// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machineopscontroller "github.com/Azure/unbounded/pkg/machineops/controller"
)

// MachinaMachineOperationReconciler claims local MachineOperations and
// dispatches them to kind-specific targets.
type MachinaMachineOperationReconciler struct {
	client      client.Client
	controller  *machineopscontroller.Reconciler
	machineName string
	nodeName    string
	handlers    MachineOperationHandlers
}

// NewMachinaMachineOperationReconciler returns a reusable MachineOperation request
// reconciler backed by client and kind-specific targets.
func NewMachinaMachineOperationReconciler(
	c client.Client,
	machineName string,
	nodeName string,
	handlers MachineOperationHandlers,
) (*MachinaMachineOperationReconciler, error) {
	if c == nil {
		return nil, fmt.Errorf("client is required")
	}
	if machineName == "" {
		return nil, fmt.Errorf("machine name is required")
	}
	if nodeName == "" {
		return nil, fmt.Errorf("node name is required")
	}
	if handlers == nil {
		return nil, fmt.Errorf("machine operation handlers are required")
	}
	for kind, handler := range handlers {
		if handler == nil {
			return nil, fmt.Errorf("machine operation handler %s is required", kind)
		}
	}

	reconciler := &MachinaMachineOperationReconciler{client: c, machineName: machineName, nodeName: nodeName, handlers: handlers}
	operations := make([]machineopscontroller.Operation, 0, len(handlers))
	for kind, handler := range handlers {
		operations = append(operations, machineopscontroller.Operation{Kind: kind, Handler: handler})
	}
	operationController, err := machineopscontroller.NewReconciler(machineopscontroller.Config{
		Client:                     c,
		Operations:                 operations,
		TargetResolver:             machineopscontroller.TargetResolverFunc(reconciler.resolveMachineOperationTarget),
		UnsupportedOperationPolicy: machineopscontroller.UnsupportedOperationIgnore,
	})
	if err != nil {
		return nil, err
	}
	reconciler.controller = operationController

	return reconciler, nil
}

// SetupController registers the MachineOperation watch for this reconciler.
func (r *MachinaMachineOperationReconciler) SetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] {
	return b.Watches(
		&machinav1alpha3.MachineOperation{},
		handler.TypedEnqueueRequestsFromMapFunc(machineopscontroller.NewTypedMapper(r.controller, NewMachineOperationRequest)),
	)
}

func (r *MachinaMachineOperationReconciler) mapMachineOperation(ctx context.Context, obj client.Object) []Request {
	mapper := machineopscontroller.NewTypedMapper(r.controller, NewMachineOperationRequest)
	return mapper(ctx, obj)
}

// ReconcileMachineOperation handles a queued MachineOperation request.
func (r *MachinaMachineOperationReconciler) ReconcileMachineOperation(
	ctx context.Context,
	name string,
) (ctrl.Result, error) {
	return r.controller.ReconcileName(ctx, name)
}

func (r *MachinaMachineOperationReconciler) resolveMachineOperationTarget(ctx context.Context, op *machinav1alpha3.MachineOperation) (machineopscontroller.TargetResult, error) {
	matches, err := r.matchesMachine(ctx, op)
	if err != nil {
		return machineopscontroller.TargetResult{}, err
	}
	if !matches {
		return machineopscontroller.TargetResult{Decision: machineopscontroller.TargetIgnore}, nil
	}

	return machineopscontroller.TargetResult{Decision: machineopscontroller.TargetClaim}, nil
}

func (r *MachinaMachineOperationReconciler) matchesMachine(ctx context.Context, op *machinav1alpha3.MachineOperation) (bool, error) {
	if op.Spec.MachineRef != "" {
		return op.Spec.MachineRef == r.machineName, nil
	}

	if op.Spec.MachineSelector == nil {
		return false, nil
	}

	var node corev1.Node
	if err := r.client.Get(ctx, client.ObjectKey{Name: r.nodeName}, &node); err == nil {
		return machineopscontroller.SelectorMatches(op.Spec.MachineSelector, node.Labels)
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get Node %s for selector match: %w", r.nodeName, err)
	}

	var machine machinav1alpha3.Machine
	if err := r.client.Get(ctx, client.ObjectKey{Name: r.machineName}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get Machine %s for selector match: %w", r.machineName, err)
	}

	return machineopscontroller.SelectorMatches(op.Spec.MachineSelector, machine.Labels)
}

func (r *MachinaMachineOperationReconciler) MarkInProgress(ctx context.Context, op MachineOperation, message string) error {
	return r.controller.MarkInProgress(ctx, op, message)
}

// Finish records the final status for a MachineOperation.
func (r *MachinaMachineOperationReconciler) Finish(ctx context.Context, op MachineOperation, result MachineOperationResult) error {
	return r.controller.Finish(ctx, op, result)
}

// FinishMachineOperation records the final status for a MachineOperation.
func FinishMachineOperation(ctx context.Context, c client.Client, op MachineOperation, result MachineOperationResult) error {
	return machineopscontroller.NewStatusStore(c, nil).Finish(ctx, op, result)
}

var _ MachineOperationStore = (*MachinaMachineOperationReconciler)(nil)

var _ MachineOperationRequestReconciler = (*MachinaMachineOperationReconciler)(nil)
