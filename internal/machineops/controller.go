// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	OperationConditionCompleted = "Completed"
)

// OperationRequest is the generic provider-facing view of a MachineOperation.
type OperationRequest = publicmachineops.OperationRequest

// OperationAuth is the provider-facing credential material resolved for an operation.
type OperationAuth = publicmachineops.OperationAuth

// OperationResult describes provider-side changes that must be reflected after execution.
type OperationResult = publicmachineops.OperationResult

// Provider registers MachineOperation lifecycle strategies for one external provider.
type Provider = publicmachineops.Provider

// MachineOperationReconciler reconciles MachineOperation objects that target
// externally controlled machines.
type MachineOperationReconciler struct {
	client.Client
	RESTMapper                  apimeta.RESTMapper
	Providers                   []*Provider
	SiteName                    string
	ProviderName                string
	MaxConcurrentReconciles     int
	ProviderPollInterval        time.Duration
	ProviderStallAfter          time.Duration
	ProviderStalledPollInterval time.Duration
	Now                         func() metav1.Time
	ClusterInfo                 *ClusterInfo
	KubeClient                  kubernetes.Interface
	APIServerEndpoint           string
	CredentialSecretNamespace   string
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/finalizers,verbs=update
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperationcredentials,verbs=get;list;watch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineconfigurationversions,verbs=get;list
// +kubebuilder:rbac:groups=infrastructure.unbounded-cloud.io,resources=azuremachines,verbs=get
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

	if !r.ownsMachine(&machine) {
		logger.V(1).Info("operation not owned by this external power controller",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name,
			"machineSite", siteNameFromLabels(machine.Labels),
			"machineProvider", machine.Spec.Provider,
			"controllerSite", r.SiteName,
			"controllerProvider", r.ProviderName)

		return ctrl.Result{}, nil
	}

	providerMatch := r.providerFor(&machine, op.Spec.OperationKind)
	if !providerMatch.supported() {
		if providerMatch.provider != nil && isHostOperation(op.Spec.OperationKind) {
			return r.failOperation(ctx, &op, "UnsupportedOperation", fmt.Sprintf("%s is not supported for %s", op.Spec.OperationKind, machine.Spec.Provider))
		}

		logger.V(1).Info("operation not handled by external power controller",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return ctrl.Result{}, nil
	}

	if err := validateProviderReference(&machine, providerMatch.provider); err != nil {
		return r.failOperation(ctx, &op, "InvalidProviderRef", err.Error())
	}

	if isHostOperation(op.Spec.OperationKind) {
		result, waiting, err := r.waitForOlderConflictingOperation(ctx, &op, &machine, providerMatch)
		if err != nil || waiting {
			return result, err
		}
	}

	if _, ok := operationTarget(&op, machine.Name); !ok {
		if err := r.initializeOperationTarget(ctx, &op, &machine, providerMatch); err != nil {
			var permanentErr *targetInputError
			if errors.As(err, &permanentErr) {
				return r.failOperation(ctx, &op, "RequestBuildFailed", err.Error())
			}

			return ctrl.Result{}, fmt.Errorf("initialize MachineOperation target input: %w", err)
		}

		if err := r.Get(ctx, opKey, &op); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	if providerMatch.operation.Mode() == publicmachineops.OperationModeLongRunning {
		return r.reconcileResumableOperation(ctx, &op, &machine, providerMatch)
	}

	return r.reconcileSupportedOperation(ctx, opKey, &op, &machine, providerMatch)
}

func (r *MachineOperationReconciler) reconcileSupportedOperation(
	ctx context.Context,
	opKey client.ObjectKey,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	shouldExecute := shouldExecuteOperation(op, providerMatch.operation)
	if !shouldExecute {
		logger.V(1).Info("operation already in progress, not re-executing external power action",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return ctrl.Result{}, nil
	}

	auth, authFailure, err := r.resolveOperationAuth(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	if authFailure != nil {
		return r.failOperationTarget(ctx, op, machine.Name, authFailure.Reason, authFailure.Message)
	}

	message := fmt.Sprintf("executing %s via %s", op.Spec.OperationKind, providerMatch.provider.Name())
	if err := r.ensureOperationInProgress(ctx, opKey, op, message); err != nil {
		return ctrl.Result{}, err
	}

	target, ok := operationTarget(op, machine.Name)
	if !ok {
		return r.failOperation(ctx, op, "TargetStateMissing", fmt.Sprintf("MachineOperation target %s is missing", machine.Name))
	}

	operationRequest, err := r.operationRequest(ctx, op, machine, target, machine.Spec.ProviderID, auth, providerMatch.operation.RequiresReplaceUserData())
	if err != nil {
		return r.failOperationTarget(ctx, op, machine.Name, "BootstrapDataFailed", err.Error())
	}

	operationResult, err := providerMatch.operation.Execute(ctx, operationRequest)
	if err != nil {
		return r.failOperationTarget(ctx, op, machine.Name, "ExecutionFailed", err.Error())
	}

	if err := r.applyOperationResult(ctx, machine, providerMatch, operationRequest, operationResult); err != nil {
		return ctrl.Result{}, err
	}

	message = fmt.Sprintf("%s completed via %s", op.Spec.OperationKind, providerMatch.provider.Name())
	if err := r.finishOperationTarget(ctx, op.Name, machine.Name, unboundedv1alpha3.OperationPhaseComplete, message); err != nil {
		return ctrl.Result{}, err
	}

	return r.completeOperation(ctx, op, machine.Generation, message)
}

func (r *MachineOperationReconciler) ensureOperationInProgress(
	ctx context.Context,
	opKey client.ObjectKey,
	op *unboundedv1alpha3.MachineOperation,
	message string,
) error {
	if op.Status.Phase == unboundedv1alpha3.OperationPhaseInProgress {
		return nil
	}

	if err := r.markInProgress(ctx, op, message); err != nil {
		return err
	}

	return client.IgnoreNotFound(r.Get(ctx, opKey, op))
}

func (r *MachineOperationReconciler) operationRequest(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	target *unboundedv1alpha3.MachineOperationTargetStatus,
	providerID string,
	auth *OperationAuth,
	includeReplaceUserData bool,
) (OperationRequest, error) {
	request := OperationRequest{
		MachineName:       machine.Name,
		MachineUID:        machine.UID,
		MachineGeneration: target.ObservedGeneration,
		OperationName:     op.Name,
		OperationUID:      op.UID,
		ProviderID:        providerID,
		Operation:         op.Spec.OperationKind,
		Parameters:        op.Spec.Parameters,
		Auth:              auth,
	}

	if target.Input != nil {
		request.ProviderRef = target.Input.ProviderRef.DeepCopy()
		request.HostImage = target.Input.HostImage
	}

	if op.Spec.OperationKind != unboundedv1alpha3.OperationHostReplace || !includeReplaceUserData {
		return request, nil
	}

	userData, err := r.buildReplaceUserData(ctx, machine)
	if err != nil {
		return OperationRequest{}, err
	}

	request.ReplaceUserData = userData

	return request, nil
}

func (r *MachineOperationReconciler) applyOperationResult(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
	request OperationRequest,
	result OperationResult,
) error {
	if result.ProviderID != "" && result.ProviderID != machine.Spec.ProviderID {
		updatedGeneration, err := r.updateMachineProviderID(ctx, machine, result.ProviderID)
		if err != nil {
			return err
		}

		machine.Spec.ProviderID = result.ProviderID
		machine.Generation = updatedGeneration
	}

	if err := providerMatch.operation.Cleanup(ctx, request, result); err != nil {
		return fmt.Errorf("cleanup %s via %s: %w", request.Operation, providerMatch.provider.Name(), err)
	}

	return nil
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

func shouldExecuteOperation(op *unboundedv1alpha3.MachineOperation, operation *publicmachineops.Operation) bool {
	if op.Status.Phase == "" || op.Status.Phase == unboundedv1alpha3.OperationPhasePending {
		return true
	}

	return operation.ReplaySafe() && op.Status.Phase == unboundedv1alpha3.OperationPhaseInProgress
}

func isHostOperation(operation unboundedv1alpha3.OperationKind) bool {
	return strings.HasPrefix(string(operation), "Host")
}

type providerMatch struct {
	provider  *Provider
	operation *publicmachineops.Operation
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
				oldOperation, oldOK := e.ObjectOld.(*unboundedv1alpha3.MachineOperation)
				newOperation, newOK := e.ObjectNew.(*unboundedv1alpha3.MachineOperation)

				return oldOK && newOK && shouldReconcileOperationUpdate(oldOperation, newOperation)
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

func shouldReconcileOperationUpdate(oldOperation, newOperation *unboundedv1alpha3.MachineOperation) bool {
	if oldOperation.Generation != newOperation.Generation {
		return shouldReconcileOperation(newOperation)
	}

	becameTerminal := !oldOperation.Status.IsTerminal() && newOperation.Status.IsTerminal()

	return becameTerminal && shouldReconcileOperation(newOperation)
}

func (r *MachineOperationReconciler) providerFor(machine *unboundedv1alpha3.Machine, operation unboundedv1alpha3.OperationKind) providerMatch {
	if machine.Spec.Provider == "" {
		return providerMatch{}
	}

	var matched providerMatch

	for _, provider := range r.Providers {
		if provider == nil {
			continue
		}

		if provider.Name() != machine.Spec.Provider {
			continue
		}

		matched.provider = provider
		matched.operation, _ = provider.Operation(operation)

		return matched
	}

	return matched
}

func (m providerMatch) supported() bool {
	return m.provider != nil && m.operation != nil
}

func (r *MachineOperationReconciler) ownsMachine(machine *unboundedv1alpha3.Machine) bool {
	if r.SiteName != "" && siteNameFromLabels(machine.Labels) != r.SiteName {
		return false
	}

	if r.ProviderName != "" && machine.Spec.Provider != r.ProviderName {
		return false
	}

	return true
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
	message = truncateUTF8Bytes(message, maxConditionMessageBytes)

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

		for i := range latest.Status.Targets {
			target := &latest.Status.Targets[i]
			if target.Phase == unboundedv1alpha3.OperationPhaseComplete || target.Phase == unboundedv1alpha3.OperationPhaseFailed {
				continue
			}

			target.Phase = unboundedv1alpha3.OperationPhaseInProgress

			target.Message = message
			if target.StartedAt == nil {
				target.StartedAt = &now
			}
		}

		condition := normalizeCondition(metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            message,
			ObservedGeneration: latest.Generation,
		})
		apimeta.SetStatusCondition(&latest.Status.Conditions, condition)

		if err := r.Status().Update(ctx, &latest); err != nil {
			return fmt.Errorf("mark MachineOperation InProgress: %w", err)
		}

		return nil
	})
}

func (r *MachineOperationReconciler) completeOperation(ctx context.Context, op *unboundedv1alpha3.MachineOperation, observedMachineGeneration int64, message string) (ctrl.Result, error) {
	return r.finishOperation(ctx, op, unboundedv1alpha3.OperationPhaseComplete, "Succeeded", message, observedMachineGeneration)
}

func (r *MachineOperationReconciler) failOperation(ctx context.Context, op *unboundedv1alpha3.MachineOperation, reason, message string) (ctrl.Result, error) {
	return r.finishOperation(ctx, op, unboundedv1alpha3.OperationPhaseFailed, reason, message, 0)
}

func (r *MachineOperationReconciler) finishOperation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	phase unboundedv1alpha3.OperationPhase,
	reason string,
	message string,
	observedMachineGeneration int64,
) (ctrl.Result, error) {
	reason = normalizeConditionReason(reason)
	message = truncateUTF8Bytes(message, maxConditionMessageBytes)

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

		condition := normalizeCondition(metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: updated.Generation,
		})
		apimeta.SetStatusCondition(&updated.Status.Conditions, condition)

		return r.Status().Update(ctx, &updated)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("finish MachineOperation: %w", err)
	}

	if updated.Spec.TTLSecondsAfterFinished != nil {
		return r.reconcileTerminal(ctx, &updated)
	}

	return ctrl.Result{}, nil
}

func (r *MachineOperationReconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}
