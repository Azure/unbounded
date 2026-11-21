// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// MachineOperationHandlers maps operation kinds to host-local MachineOperation handlers.
type MachineOperationHandlers map[machinav1alpha3.OperationKind]MachineOperationHandler[int64]

// MachinaMachineOperationReconciler fetches MachineOperation objects, skips terminal
// operations, and dispatches the non-terminal operations whose machineRef names this
// machine to kind-specific targets.
type MachinaMachineOperationReconciler struct {
	client      client.Client
	reader      client.Reader
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
	return NewMachinaMachineOperationReconcilerWithReader(c, c, machineName, nodeName, handlers)
}

// NewMachinaMachineOperationReconcilerWithReader configures an uncached reader
// for terminal-state checks while retaining the client for status writes.
func NewMachinaMachineOperationReconcilerWithReader(
	c client.Client,
	reader client.Reader,
	machineName string,
	nodeName string,
	handlers MachineOperationHandlers,
) (*MachinaMachineOperationReconciler, error) {
	if c == nil {
		return nil, fmt.Errorf("client is required")
	}

	if reader == nil {
		return nil, fmt.Errorf("reader is required")
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

	return &MachinaMachineOperationReconciler{client: c, reader: reader, machineName: machineName, nodeName: nodeName, handlers: handlers}, nil
}

// SetupController registers the MachineOperation watch for this reconciler.
func (r *MachinaMachineOperationReconciler) SetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] {
	return b.Watches(
		&machinav1alpha3.MachineOperation{},
		handler.TypedEnqueueRequestsFromMapFunc(r.mapMachineOperation),
	)
}

// ReconcileMachineOperation handles a queued MachineOperation request, ignoring
// terminal operations, allocation operations, and operations that do not name
// this machine.
func (r *MachinaMachineOperationReconciler) ReconcileMachineOperation(
	ctx context.Context,
	name string,
) (ctrl.Result, error) {
	var op machinav1alpha3.MachineOperation
	if err := r.reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		return ctrl.Result{}, nil
	}

	// HostAllocate and HostRelease act on a MachineAllocation, not on this
	// host. They are never dispatched, even when a handler is registered.
	if op.Spec.OperationKind.IsAllocationOperation() {
		log.FromContext(ctx).V(1).Info("ignoring allocation MachineOperation", "operation", op.Name, "operationKind", op.Spec.OperationKind)
		return ctrl.Result{}, nil
	}

	handler, ok := r.handlers[op.Spec.OperationKind]
	if !ok {
		log.FromContext(ctx).V(1).Info("ignoring MachineOperation with no local handler", "operation", op.Name, "operationKind", op.Spec.OperationKind)
		return ctrl.Result{}, nil
	}

	if !r.matchesMachine(&op) {
		log.FromContext(ctx).V(1).Info("ignoring MachineOperation for another machine", "operation", op.Name, "machineRef", op.Spec.MachineRef)
		return ctrl.Result{}, nil
	}

	// Say that an agent has the operation before the handler can decide to
	// wait, for a lock or for this daemon to be restarted. Whoever created
	// the operation can then tell a busy agent from a machine nothing serves.
	if err := r.acknowledge(ctx, &op); err != nil {
		return ctrl.Result{}, err
	}

	operation := MachineOperation{
		Name: op.Name,
		Kind: op.Spec.OperationKind,
	}

	operation.Parameters = op.Spec.Parameters

	result, err := handler(ctx, r, operation)
	if errors.Is(err, ErrMachineOperationFinished) {
		log.FromContext(ctx).Info("not running MachineOperation: it finished before this agent started it", "operation", op.Name)
		return ctrl.Result{}, nil
	}

	return result, err
}

// ErrMachineOperationFinished is returned by MarkInProgress when the
// operation reached a terminal phase before the agent started it, for
// example because its creator gave up waiting. The handler must not run it.
var ErrMachineOperationFinished = errors.New("machine operation is already finished")

const acknowledgedMessage = "accepted by the agent; waiting to start"

// acknowledge marks an operation with no phase as Pending.
func (r *MachinaMachineOperationReconciler) acknowledge(ctx context.Context, op *machinav1alpha3.MachineOperation) error {
	if op.Status.Phase != "" {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := r.client.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		if latest.Status.Phase != "" {
			return nil
		}

		latest.Status.Phase = machinav1alpha3.OperationPhasePending
		latest.Status.Message = acknowledgedMessage

		return r.client.Status().Update(ctx, &latest)
	})
}

func (r *MachinaMachineOperationReconciler) mapMachineOperation(_ context.Context, obj client.Object) []Request {
	op, ok := obj.(*machinav1alpha3.MachineOperation)
	if !ok || !r.shouldEnqueue(op) {
		return nil
	}

	return []Request{NewMachineOperationRequest(op.Name)}
}

func (r *MachinaMachineOperationReconciler) shouldEnqueue(op *machinav1alpha3.MachineOperation) bool {
	if op.Status.Phase != "" && op.Status.Phase != machinav1alpha3.OperationPhasePending {
		return false
	}

	if op.Spec.OperationKind.IsAllocationOperation() {
		return false
	}

	if _, ok := r.handlers[op.Spec.OperationKind]; !ok {
		return false
	}

	return r.matchesMachine(op)
}

// matchesMachine reports whether op is for this agent to run now: it names
// this machine and is not held. An operation without a machineRef is never a
// match, whatever its selector says: a central controller resolves selectors
// into one machineRef operation per machine, and releases a held one by
// clearing spec.suspend, which is an update the agent sees.
func (r *MachinaMachineOperationReconciler) matchesMachine(op *machinav1alpha3.MachineOperation) bool {
	return op.Spec.MachineRef == r.machineName && !op.IsHeld()
}

func (r *MachinaMachineOperationReconciler) MarkInProgress(ctx context.Context, op MachineOperation, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := r.client.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return err
		}

		if latest.Status.IsTerminal() {
			return ErrMachineOperationFinished
		}

		now := metav1.Now()
		latest.Status.Phase = machinav1alpha3.OperationPhaseInProgress

		latest.Status.Message = message
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            message,
			ObservedGeneration: latest.Generation,
		})

		return r.client.Status().Update(ctx, &latest)
	})
}

// Finish records the final status for a MachineOperation.
func (r *MachinaMachineOperationReconciler) Finish(ctx context.Context, op MachineOperation, result MachineOperationResult[int64]) error {
	return FinishMachineOperation(ctx, r.client, op, result)
}

// FinishMachineOperation records the final status for a MachineOperation. A
// Failed operation is left unchanged. A Complete one is changed only to
// Failed: an agent upgrade that was reported complete and then rolled back
// reports its failure that way.
func FinishMachineOperation(ctx context.Context, c client.Client, op MachineOperation, result MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		if latest.Status.Phase == machinav1alpha3.OperationPhaseFailed {
			return nil
		}

		if latest.Status.Phase == machinav1alpha3.OperationPhaseComplete && result.Phase != machinav1alpha3.OperationPhaseFailed {
			return nil
		}

		SetMachineOperationResult(&latest, result, metav1.Now())

		return c.Status().Update(ctx, &latest)
	})
}

// SetMachineOperationResult applies result to the status of op, as
// FinishMachineOperation does, without writing it.
func SetMachineOperationResult(op *machinav1alpha3.MachineOperation, result MachineOperationResult[int64], now metav1.Time) {
	if op.Status.StartedAt == nil {
		op.Status.StartedAt = &now
	}

	op.Status.Phase = result.Phase
	op.Status.Message = result.Message

	op.Status.CompletedAt = &now
	if result.ObservedMachineGeneration > 0 {
		op.Status.ObservedMachineGeneration = result.ObservedMachineGeneration
	}

	conditionStatus := metav1.ConditionTrue
	if result.Phase == machinav1alpha3.OperationPhaseFailed {
		conditionStatus = metav1.ConditionFalse
	}

	apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
		Type:               "Completed",
		Status:             conditionStatus,
		Reason:             result.Reason,
		Message:            result.Message,
		ObservedGeneration: op.Generation,
	})
}

var _ MachineOperationStore[int64] = (*MachinaMachineOperationReconciler)(nil)

var _ MachineOperationRequestReconciler = (*MachinaMachineOperationReconciler)(nil)
