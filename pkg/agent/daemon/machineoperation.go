// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
// operations, and dispatches non-terminal operations to kind-specific targets.
type MachinaMachineOperationReconciler struct {
	client      client.Client
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

	return &MachinaMachineOperationReconciler{client: c, machineName: machineName, nodeName: nodeName, handlers: handlers}, nil
}

// SetupController registers the MachineOperation watch for this reconciler.
func (r *MachinaMachineOperationReconciler) SetupController(b *builder.TypedBuilder[Request]) *builder.TypedBuilder[Request] {
	return b.Watches(
		&machinav1alpha3.MachineOperation{},
		handler.TypedEnqueueRequestsFromMapFunc(r.mapMachineOperation),
	)
}

// ReconcileMachineOperation handles a queued MachineOperation request.
func (r *MachinaMachineOperationReconciler) ReconcileMachineOperation(
	ctx context.Context,
	name string,
) (ctrl.Result, error) {
	var op machinav1alpha3.MachineOperation
	if err := r.client.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		return ctrl.Result{}, nil
	}

	operation := MachineOperation{
		Name: op.Name,
		Kind: op.Spec.OperationKind,
	}

	handler, ok := r.handlers[op.Spec.OperationKind]
	if !ok {
		log.FromContext(ctx).V(1).Info("ignoring MachineOperation with no local handler", "operation", op.Name, "operationKind", op.Spec.OperationKind)
		return ctrl.Result{}, nil
	}

	operation.Parameters = op.Spec.Parameters

	return handler(ctx, r, operation)
}

func (r *MachinaMachineOperationReconciler) mapMachineOperation(ctx context.Context, obj client.Object) []Request {
	op, ok := obj.(*machinav1alpha3.MachineOperation)
	if !ok || !r.shouldEnqueue(ctx, op) {
		return nil
	}

	return []Request{NewMachineOperationRequest(op.Name)}
}

func (r *MachinaMachineOperationReconciler) shouldEnqueue(ctx context.Context, op *machinav1alpha3.MachineOperation) bool {
	if op.Status.Phase != "" && op.Status.Phase != machinav1alpha3.OperationPhasePending {
		return false
	}

	if _, ok := r.handlers[op.Spec.OperationKind]; !ok {
		return false
	}

	matches, err := r.matchesMachine(ctx, op)
	if err != nil {
		log.FromContext(ctx).Error(err, "invalid MachineOperation target selector", "operation", op.Name)
		return false
	}

	return matches
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
		return selectorMatches(op.Spec.MachineSelector, node.Labels)
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

	return selectorMatches(op.Spec.MachineSelector, machine.Labels)
}

func (r *MachinaMachineOperationReconciler) MarkInProgress(ctx context.Context, op MachineOperation, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := r.client.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return err
		}

		if latest.Status.IsTerminal() {
			return nil
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

// FinishMachineOperation records the final status for a MachineOperation.
func FinishMachineOperation(ctx context.Context, c client.Client, op MachineOperation, result MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		now := metav1.Now()
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		latest.Status.Phase = result.Phase
		latest.Status.Message = result.Message

		latest.Status.CompletedAt = &now
		if result.ObservedMachineGeneration > 0 {
			latest.Status.ObservedMachineGeneration = result.ObservedMachineGeneration
		}

		conditionStatus := metav1.ConditionTrue
		if result.Phase == machinav1alpha3.OperationPhaseFailed {
			conditionStatus = metav1.ConditionFalse
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             conditionStatus,
			Reason:             result.Reason,
			Message:            result.Message,
			ObservedGeneration: latest.Generation,
		})

		return c.Status().Update(ctx, &latest)
	})
}

var _ MachineOperationStore[int64] = (*MachinaMachineOperationReconciler)(nil)

func selectorMatches(selector *metav1.LabelSelector, targetLabels map[string]string) (bool, error) {
	compiled, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, err
	}

	return compiled.Matches(labels.Set(targetLabels)), nil
}

var _ MachineOperationRequestReconciler = (*MachinaMachineOperationReconciler)(nil)
