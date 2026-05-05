// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	OperationConditionCompleted = "Completed"
)

// OperationRequest is the generic provider-facing view of a MachineOperation.
type OperationRequest struct {
	Machine    *unboundedv1alpha3.Machine
	ProviderID string
	Operation  unboundedv1alpha3.OperationKind
	Parameters map[string]string
}

// Provider executes MachineOperation requests for a specific external provider.
type Provider interface {
	Name() string
	Supports(operation unboundedv1alpha3.OperationKind) bool
	Execute(ctx context.Context, request OperationRequest) error
}

// MachineOperationReconciler reconciles MachineOperation objects that target
// externally controlled machines.
type MachineOperationReconciler struct {
	client.Client
	Providers               []Provider
	MaxConcurrentReconciles int
	Now                     func() metav1.Time
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/finalizers,verbs=update
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch

func (r *MachineOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	opKey := client.ObjectKey{Name: req.Name}

	var op unboundedv1alpha3.MachineOperation
	if err := r.Get(ctx, opKey, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		return r.reconcileTerminal(ctx, &op)
	}

	if op.Spec.MachineRef == "" {
		if op.Spec.MachineSelector != nil {
			logger.V(1).Info("selector-based operation not handled by external power controller", "operation", op.Name)
			return ctrl.Result{}, nil
		}

		return r.failOperation(ctx, &op, "InvalidSpec", "spec.machineRef is required")
	}

	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: op.Spec.MachineRef}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return r.failOperation(ctx, &op, "MachineNotFound", fmt.Sprintf("Machine %s not found", op.Spec.MachineRef))
		}

		return ctrl.Result{}, fmt.Errorf("get Machine %s: %w", op.Spec.MachineRef, err)
	}

	providerID, provider, ok := r.providerFor(&machine, op.Spec.OperationKind)
	if !ok {
		logger.V(1).Info("operation not handled by external power controller",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return ctrl.Result{}, nil
	}

	shouldExecute := op.Status.Phase == "" || op.Status.Phase == unboundedv1alpha3.OperationPhasePending
	if shouldExecute {
		if err := r.markInProgress(ctx, &op, fmt.Sprintf("executing %s via %s", op.Spec.OperationKind, provider.Name())); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Get(ctx, opKey, &op); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if !shouldExecute {
		logger.V(1).Info("operation already in progress, not re-executing external power action",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return ctrl.Result{}, nil
	}

	if err := provider.Execute(ctx, OperationRequest{Machine: &machine, ProviderID: providerID, Operation: op.Spec.OperationKind, Parameters: op.Spec.Parameters}); err != nil {
		return r.failOperation(ctx, &op, "ExecutionFailed", err.Error())
	}

	return r.completeOperation(ctx, &op, machine.Generation, fmt.Sprintf("%s completed via %s", op.Spec.OperationKind, provider.Name()))
}

func (r *MachineOperationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("machineoperation-external-power").
		For(&unboundedv1alpha3.MachineOperation{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				op, ok := e.Object.(*unboundedv1alpha3.MachineOperation)
				return ok && shouldReconcileOperation(op)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				op, ok := e.ObjectNew.(*unboundedv1alpha3.MachineOperation)
				return ok && shouldReconcileOperation(op)
			},
			DeleteFunc: func(e event.DeleteEvent) bool { return false },
			GenericFunc: func(e event.GenericEvent) bool {
				op, ok := e.Object.(*unboundedv1alpha3.MachineOperation)
				return ok && shouldReconcileOperation(op)
			},
		}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

func shouldReconcileOperation(op *unboundedv1alpha3.MachineOperation) bool {
	if !op.Status.IsTerminal() {
		return true
	}

	return op.Spec.TTLSecondsAfterFinished != nil && op.Status.CompletedAt != nil
}

func (r *MachineOperationReconciler) providerFor(machine *unboundedv1alpha3.Machine, operation unboundedv1alpha3.OperationKind) (string, Provider, bool) {
	if machine.Spec.Provider == "" || machine.Spec.ProviderID == "" {
		return "", nil, false
	}

	for _, provider := range r.Providers {
		if provider.Name() == machine.Spec.Provider && provider.Supports(operation) {
			return machine.Spec.ProviderID, provider, true
		}
	}

	return "", nil, false
}

func (r *MachineOperationReconciler) reconcileTerminal(ctx context.Context, op *unboundedv1alpha3.MachineOperation) (ctrl.Result, error) {
	if op.Spec.TTLSecondsAfterFinished == nil || op.Status.CompletedAt == nil {
		return ctrl.Result{}, nil
	}

	deadline := op.Status.CompletedAt.Add(time.Duration(*op.Spec.TTLSecondsAfterFinished) * time.Second)

	now := r.now().Time
	if now.Before(deadline) {
		return ctrl.Result{RequeueAfter: deadline.Sub(now)}, nil
	}

	if err := r.Delete(ctx, op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return ctrl.Result{}, nil
}

func (r *MachineOperationReconciler) markInProgress(ctx context.Context, op *unboundedv1alpha3.MachineOperation, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest unboundedv1alpha3.MachineOperation
		if err := r.Get(ctx, client.ObjectKeyFromObject(op), &latest); err != nil {
			return err
		}

		now := r.now()
		latest.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress

		latest.Status.Message = message
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            message,
			ObservedGeneration: latest.Generation,
		})

		if err := r.Status().Update(ctx, &latest); err != nil {
			return fmt.Errorf("mark MachineOperation InProgress: %w", err)
		}

		return nil
	})
}

func (r *MachineOperationReconciler) completeOperation(ctx context.Context, op *unboundedv1alpha3.MachineOperation, observedMachineGeneration int64, message string) (ctrl.Result, error) {
	return r.finishOperation(ctx, op, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", message, observedMachineGeneration, nil)
}

func (r *MachineOperationReconciler) failOperation(ctx context.Context, op *unboundedv1alpha3.MachineOperation, reason, message string) (ctrl.Result, error) {
	return r.finishOperation(ctx, op, unboundedv1alpha3.OperationPhaseFailed, reason, message, 0, nil)
}

func (r *MachineOperationReconciler) finishOperation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	phase unboundedv1alpha3.OperationPhase,
	reason string,
	message string,
	observedMachineGeneration int64,
	execErr error,
) (ctrl.Result, error) {
	var updated unboundedv1alpha3.MachineOperation

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, client.ObjectKeyFromObject(op), &updated); err != nil {
			return err
		}

		now := r.now()
		if updated.Status.StartedAt == nil {
			updated.Status.StartedAt = &now
		}

		updated.Status.Phase = phase
		updated.Status.Message = message

		updated.Status.CompletedAt = &now
		if observedMachineGeneration > 0 {
			updated.Status.ObservedMachineGeneration = observedMachineGeneration
		}

		conditionStatus := metav1.ConditionTrue
		if phase == unboundedv1alpha3.OperationPhaseFailed {
			conditionStatus = metav1.ConditionFalse
		}

		apimeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: updated.Generation,
		})

		return r.Status().Update(ctx, &updated)
	}); err != nil {
		if execErr != nil {
			return ctrl.Result{}, execErr
		}

		return ctrl.Result{}, fmt.Errorf("finish MachineOperation: %w", err)
	}

	if updated.Spec.TTLSecondsAfterFinished != nil {
		return r.reconcileTerminal(ctx, &updated)
	}

	return ctrl.Result{}, execErr
}

func (r *MachineOperationReconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}
