// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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
	Machine         *unboundedv1alpha3.Machine
	OperationName   string
	OperationUID    types.UID
	ProviderID      string
	Operation       unboundedv1alpha3.OperationKind
	Parameters      map[string]string
	ReplaceUserData string
}

// OperationResult describes provider-side changes that must be reflected after
// execution, such as replacement of an underlying cloud resource identity.
type OperationResult struct {
	ProviderID        string
	CleanupProviderID string
}

// Provider executes MachineOperation requests for a specific external provider.
type Provider interface {
	Name() string
	Supports(operation unboundedv1alpha3.OperationKind) bool
	Execute(ctx context.Context, request OperationRequest) (OperationResult, error)
	Cleanup(ctx context.Context, request OperationRequest, result OperationResult) error
}

// MachineOperationReconciler reconciles MachineOperation objects that target
// externally controlled machines.
type MachineOperationReconciler struct {
	client.Client
	Providers               []Provider
	MaxConcurrentReconciles int
	Now                     func() metav1.Time
	ClusterInfo             *ClusterInfo
	KubeClient              kubernetes.Interface
	APIServerEndpoint       string
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/finalizers,verbs=update
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// +kubebuilder:rbac:groups="",resources=configmaps;services;secrets,verbs=get
// +kubebuilder:rbac:nonResourceURLs=/version,verbs=get

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

	providerMatch := r.providerFor(&machine, op.Spec.OperationKind)
	if providerMatch.provider == nil {
		if providerMatch.providerExists && isHostOperation(op.Spec.OperationKind) {
			return r.failOperation(ctx, &op, "UnsupportedOperation", fmt.Sprintf("%s is not supported for %s", op.Spec.OperationKind, machine.Spec.Provider))
		}

		logger.V(1).Info("operation not handled by external power controller",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return ctrl.Result{}, nil
	}

	shouldExecute := shouldExecuteOperation(&op)
	if shouldExecute {
		if op.Status.Phase != unboundedv1alpha3.OperationPhaseInProgress {
			if err := r.markInProgress(ctx, &op, fmt.Sprintf("executing %s via %s", op.Spec.OperationKind, providerMatch.provider.Name())); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.Get(ctx, opKey, &op); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
	}

	if !shouldExecute {
		logger.V(1).Info("operation already in progress, not re-executing external power action",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return ctrl.Result{}, nil
	}

	operationRequest := OperationRequest{
		Machine:       &machine,
		OperationName: op.Name,
		OperationUID:  op.UID,
		ProviderID:    providerMatch.providerID,
		Operation:     op.Spec.OperationKind,
		Parameters:    op.Spec.Parameters,
	}
	if op.Spec.OperationKind == unboundedv1alpha3.OperationHostReplace {
		userData, err := r.buildReplaceUserData(ctx, &machine)
		if err != nil {
			return r.failOperation(ctx, &op, "BootstrapDataFailed", err.Error())
		}

		operationRequest.ReplaceUserData = userData
	}

	operationResult, err := providerMatch.provider.Execute(ctx, operationRequest)
	if err != nil {
		return r.failOperation(ctx, &op, "ExecutionFailed", err.Error())
	}

	if operationResult.ProviderID != "" && operationResult.ProviderID != machine.Spec.ProviderID {
		updatedGeneration, err := r.updateMachineProviderID(ctx, &machine, operationResult.ProviderID)
		if err != nil {
			return ctrl.Result{}, err
		}

		machine.Spec.ProviderID = operationResult.ProviderID
		machine.Generation = updatedGeneration
	}

	if err := providerMatch.provider.Cleanup(ctx, operationRequest, operationResult); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup %s via %s: %w", op.Spec.OperationKind, providerMatch.provider.Name(), err)
	}

	return r.completeOperation(ctx, &op, machine.Generation, fmt.Sprintf("%s completed via %s", op.Spec.OperationKind, providerMatch.provider.Name()))
}

func (r *MachineOperationReconciler) updateMachineProviderID(ctx context.Context, machine *unboundedv1alpha3.Machine, providerID string) (int64, error) {
	updatedGeneration := machine.Generation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest unboundedv1alpha3.Machine
		if err := r.Get(ctx, client.ObjectKeyFromObject(machine), &latest); err != nil {
			return err
		}

		patch := client.MergeFrom(latest.DeepCopy())

		latest.Spec.ProviderID = providerID
		if err := r.Patch(ctx, &latest, patch); err != nil {
			return fmt.Errorf("patch Machine providerID: %w", err)
		}

		if err := r.Get(ctx, client.ObjectKeyFromObject(machine), &latest); err != nil {
			return err
		}

		updatedGeneration = latest.Generation

		return nil
	})

	return updatedGeneration, err
}

func shouldExecuteOperation(op *unboundedv1alpha3.MachineOperation) bool {
	if op.Status.Phase == "" || op.Status.Phase == unboundedv1alpha3.OperationPhasePending {
		return true
	}

	return op.Spec.OperationKind == unboundedv1alpha3.OperationHostReplace && op.Status.Phase == unboundedv1alpha3.OperationPhaseInProgress
}

func isHostOperation(operation unboundedv1alpha3.OperationKind) bool {
	return strings.HasPrefix(string(operation), "Host")
}

type providerMatch struct {
	provider       Provider
	providerID     string
	providerExists bool
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

func (r *MachineOperationReconciler) providerFor(machine *unboundedv1alpha3.Machine, operation unboundedv1alpha3.OperationKind) providerMatch {
	if machine.Spec.Provider == "" || machine.Spec.ProviderID == "" {
		return providerMatch{}
	}

	var matched providerMatch

	for _, provider := range r.Providers {
		if provider.Name() != machine.Spec.Provider {
			continue
		}

		matched.providerExists = true
		if provider.Supports(operation) {
			matched.provider = provider
			matched.providerID = machine.Spec.ProviderID

			return matched
		}
	}

	return matched
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
