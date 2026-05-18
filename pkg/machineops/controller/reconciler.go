// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	// OperationConditionCompleted is the MachineOperation completion condition type.
	OperationConditionCompleted = "Completed"
)

// Config configures a Reconciler.
type Config struct {
	Client                     client.Client
	Operations                 []Operation
	TargetResolver             TargetResolver
	UnsupportedOperationPolicy UnsupportedOperationPolicy
	Now                        func() metav1.Time
}

// Reconciler fetches, claims, dispatches, and records MachineOperation state.
type Reconciler struct {
	client                     client.Client
	operations                 operationRegistry
	targetResolver             TargetResolver
	unsupportedOperationPolicy UnsupportedOperationPolicy
	store                      *StatusStore
	now                        func() metav1.Time
}

// NewReconciler returns a reusable MachineOperation reconciler.
func NewReconciler(cfg Config) (*Reconciler, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("client is required")
	}
	operations, err := newOperationRegistry(cfg.Operations)
	if err != nil {
		return nil, err
	}
	if cfg.TargetResolver == nil {
		return nil, fmt.Errorf("target resolver is required")
	}

	now := cfg.Now
	if now == nil {
		now = metav1.Now
	}

	return &Reconciler{
		client:                     cfg.Client,
		operations:                 operations,
		targetResolver:             cfg.TargetResolver,
		unsupportedOperationPolicy: cfg.UnsupportedOperationPolicy,
		store:                      NewStatusStore(cfg.Client, now),
		now:                        now,
	}, nil
}

// Reconcile handles a controller-runtime request.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.ReconcileName(ctx, req.Name)
}

// ReconcileName handles a MachineOperation by name.
func (r *Reconciler) ReconcileName(ctx context.Context, name string) (ctrl.Result, error) {
	opKey := client.ObjectKey{Name: name}

	var op machinav1alpha3.MachineOperation
	if err := r.client.Get(ctx, opKey, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		if !ShouldReconcileOperation(&op) {
			return ctrl.Result{}, nil
		}
		if _, ok := r.operations[op.Spec.OperationKind]; !ok && r.unsupportedOperationPolicy != UnsupportedOperationFail {
			return ctrl.Result{}, nil
		}
		target, err := r.targetResolver.ResolveTarget(ctx, &op)
		if err != nil {
			return ctrl.Result{}, err
		}
		if target.Decision == TargetIgnore {
			return ctrl.Result{}, nil
		}

		return r.reconcileTerminal(ctx, &op)
	}

	target, err := r.targetResolver.ResolveTarget(ctx, &op)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch target.Decision {
	case TargetIgnore:
		return ctrl.Result{}, nil
	case TargetFail:
		return r.finishAndReconcileTerminal(ctx, requestFromOperation(&op, target.Machine), OperationResult{
			Phase:   machinav1alpha3.OperationPhaseFailed,
			Reason:  target.Reason,
			Message: target.Message,
		})
	case TargetClaim:
	default:
		return ctrl.Result{}, fmt.Errorf("unknown MachineOperation target decision %q", target.Decision)
	}

	operation, ok := r.operations[op.Spec.OperationKind]
	if !ok {
		if r.unsupportedOperationPolicy != UnsupportedOperationFail {
			return ctrl.Result{}, nil
		}

		return r.finishAndReconcileTerminal(ctx, requestFromOperation(&op, target.Machine), OperationResult{
			Phase:   machinav1alpha3.OperationPhaseFailed,
			Reason:  "UnsupportedOperation",
			Message: fmt.Sprintf("no handler registered for operation kind %s", op.Spec.OperationKind),
		})
	}

	if !operationShouldExecute(&op, operation) {
		log.FromContext(ctx).V(1).Info("operation already in progress, not re-executing",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind)

		return ctrl.Result{}, nil
	}

	handlerResult, err := operation.Handler(ctx, r, requestFromOperation(&op, target.Machine))
	if err != nil {
		return handlerResult, err
	}

	terminalResult, err := r.reconcileTerminalByName(ctx, op.Name)
	if err != nil {
		return terminalResult, err
	}
	if handlerResult != (ctrl.Result{}) {
		return handlerResult, nil
	}

	return terminalResult, nil
}

// ShouldEnqueue returns true when obj is a MachineOperation this reconciler
// should queue.
func (r *Reconciler) ShouldEnqueue(ctx context.Context, obj client.Object) bool {
	op, ok := obj.(*machinav1alpha3.MachineOperation)
	if !ok {
		return false
	}
	if !ShouldReconcileOperation(op) {
		return false
	}

	target, err := r.targetResolver.ResolveTarget(ctx, op)
	if err != nil {
		log.FromContext(ctx).Error(err, "resolve MachineOperation target for enqueue", "operation", op.Name)
		return false
	}

	if target.Decision == TargetIgnore {
		return false
	}
	if _, ok := r.operations[op.Spec.OperationKind]; !ok && r.unsupportedOperationPolicy != UnsupportedOperationFail {
		return false
	}

	return true
}

// EventFilter returns a controller-runtime predicate for MachineOperation events.
func (r *Reconciler) EventFilter() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			op, ok := e.Object.(*machinav1alpha3.MachineOperation)
			return ok && ShouldReconcileOperation(op)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			op, ok := e.ObjectNew.(*machinav1alpha3.MachineOperation)
			return ok && ShouldReconcileOperation(op)
		},
		DeleteFunc: func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool {
			op, ok := e.Object.(*machinav1alpha3.MachineOperation)
			return ok && ShouldReconcileOperation(op)
		},
	}
}

// SetupWithManager registers a plain MachineOperation controller with mgr.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, name string, maxConcurrentReconciles int) error {
	if name == "" {
		return fmt.Errorf("controller name is required")
	}
	if maxConcurrentReconciles <= 0 {
		maxConcurrentReconciles = 1
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(&machinav1alpha3.MachineOperation{}).
		WithEventFilter(r.EventFilter()).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

// MarkInProgress records that op has started execution.
func (r *Reconciler) MarkInProgress(ctx context.Context, op OperationRequest, message string) error {
	return r.store.MarkInProgress(ctx, op, message)
}

// Finish records the final status for a MachineOperation.
func (r *Reconciler) Finish(ctx context.Context, op OperationRequest, result OperationResult) error {
	return r.store.Finish(ctx, op, result)
}

func (r *Reconciler) reconcileTerminalByName(ctx context.Context, name string) (ctrl.Result, error) {
	var latest machinav1alpha3.MachineOperation
	if err := r.client.Get(ctx, client.ObjectKey{Name: name}, &latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !latest.Status.IsTerminal() {
		return ctrl.Result{}, nil
	}

	return r.reconcileTerminal(ctx, &latest)
}

func (r *Reconciler) finishAndReconcileTerminal(ctx context.Context, op OperationRequest, result OperationResult) (ctrl.Result, error) {
	if err := r.Finish(ctx, op, result); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileTerminalByName(ctx, op.Name)
}

func (r *Reconciler) reconcileTerminal(ctx context.Context, op *machinav1alpha3.MachineOperation) (ctrl.Result, error) {
	if op.Spec.TTLSecondsAfterFinished == nil || op.Status.CompletedAt == nil {
		return ctrl.Result{}, nil
	}

	deadline := op.Status.CompletedAt.Add(time.Duration(*op.Spec.TTLSecondsAfterFinished) * time.Second)
	now := r.now().Time
	if now.Before(deadline) {
		return ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
	}

	if err := r.client.Delete(ctx, op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

func requestFromOperation(op *machinav1alpha3.MachineOperation, machine *machinav1alpha3.Machine) OperationRequest {
	return OperationRequest{
		Object:     op,
		Machine:    machine,
		Name:       op.Name,
		UID:        op.UID,
		Kind:       op.Spec.OperationKind,
		Parameters: op.Spec.Parameters,
	}
}

// ShouldReconcileOperation returns true when a MachineOperation event should be
// queued by a generic controller.
func ShouldReconcileOperation(op *machinav1alpha3.MachineOperation) bool {
	if !op.Status.IsTerminal() {
		return true
	}

	return op.Spec.TTLSecondsAfterFinished != nil && op.Status.CompletedAt != nil
}

func operationShouldExecute(op *machinav1alpha3.MachineOperation, operation Operation) bool {
	if op.Status.Phase == "" || op.Status.Phase == machinav1alpha3.OperationPhasePending {
		return true
	}

	return op.Status.Phase == machinav1alpha3.OperationPhaseInProgress && operation.ReexecuteInProgress
}
