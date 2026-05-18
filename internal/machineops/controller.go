// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machineopscontroller "github.com/Azure/unbounded/pkg/machineops/controller"
)

const (
	OperationConditionCompleted = machineopscontroller.OperationConditionCompleted
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
	operationController     *machineopscontroller.Reconciler
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineoperations/finalizers,verbs=update
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list
// +kubebuilder:rbac:groups="",resources=configmaps;services;secrets,verbs=get
// +kubebuilder:rbac:nonResourceURLs=/version,verbs=get

func (r *MachineOperationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controller, err := r.controller()
	if err != nil {
		return ctrl.Result{}, err
	}

	return controller.Reconcile(ctx, req)
}

func (r *MachineOperationReconciler) resolveOperationTarget(ctx context.Context, op *unboundedv1alpha3.MachineOperation) (machineopscontroller.TargetResult, error) {
	logger := log.FromContext(ctx)

	if op.Spec.MachineRef == "" {
		if op.Spec.MachineSelector != nil {
			logger.V(1).Info("selector-based operation not handled by external power controller", "operation", op.Name)
			return machineopscontroller.TargetResult{Decision: machineopscontroller.TargetIgnore}, nil
		}

		return machineopscontroller.TargetResult{
			Decision: machineopscontroller.TargetFail,
			Reason:   "InvalidSpec",
			Message:  "spec.machineRef is required",
		}, nil
	}

	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, client.ObjectKey{Name: op.Spec.MachineRef}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return machineopscontroller.TargetResult{
				Decision: machineopscontroller.TargetFail,
				Reason:   "MachineNotFound",
				Message:  fmt.Sprintf("Machine %s not found", op.Spec.MachineRef),
			}, nil
		}

		return machineopscontroller.TargetResult{}, fmt.Errorf("get Machine %s: %w", op.Spec.MachineRef, err)
	}

	providerMatch := r.providerFor(&machine, op.Spec.OperationKind)
	if providerMatch.provider == nil {
		if providerMatch.providerExists && isHostOperation(op.Spec.OperationKind) {
			return machineopscontroller.TargetResult{
				Decision: machineopscontroller.TargetFail,
				Reason:   "UnsupportedOperation",
				Message:  fmt.Sprintf("%s is not supported for %s", op.Spec.OperationKind, machine.Spec.Provider),
			}, nil
		}

		logger.V(1).Info("operation not handled by external power controller",
			"operation", op.Name,
			"operationKind", op.Spec.OperationKind,
			"machine", machine.Name)

		return machineopscontroller.TargetResult{Decision: machineopscontroller.TargetIgnore}, nil
	}

	return machineopscontroller.TargetResult{Decision: machineopscontroller.TargetClaim, Machine: &machine}, nil
}

func (r *MachineOperationReconciler) handleOperation(ctx context.Context, store machineopscontroller.Store, request machineopscontroller.OperationRequest) (ctrl.Result, error) {
	op := request.Object
	machine := request.Machine
	if machine == nil {
		return ctrl.Result{}, fmt.Errorf("resolved Machine is required")
	}

	providerMatch := r.providerFor(machine, op.Spec.OperationKind)
	if providerMatch.provider == nil {
		return ctrl.Result{}, fmt.Errorf("provider for %s on Machine %s is no longer available", op.Spec.OperationKind, machine.Name)
	}
	if op.Status.Phase != unboundedv1alpha3.OperationPhaseInProgress {
		if err := store.MarkInProgress(ctx, request, fmt.Sprintf("executing %s via %s", op.Spec.OperationKind, providerMatch.provider.Name())); err != nil {
			return ctrl.Result{}, err
		}
	}

	operationRequest := OperationRequest{
		Machine:       machine,
		OperationName: op.Name,
		OperationUID:  op.UID,
		ProviderID:    providerMatch.providerID,
		Operation:     op.Spec.OperationKind,
		Parameters:    op.Spec.Parameters,
	}
	if op.Spec.OperationKind == unboundedv1alpha3.OperationHostReplace {
		userData, err := r.buildReplaceUserData(ctx, machine)
		if err != nil {
			return ctrl.Result{}, store.Finish(ctx, request, machineopscontroller.OperationResult{
				Phase:   unboundedv1alpha3.OperationPhaseFailed,
				Reason:  "BootstrapDataFailed",
				Message: err.Error(),
			})
		}

		operationRequest.ReplaceUserData = userData
	}

	operationResult, err := providerMatch.provider.Execute(ctx, operationRequest)
	if err != nil {
		return ctrl.Result{}, store.Finish(ctx, request, machineopscontroller.OperationResult{
			Phase:   unboundedv1alpha3.OperationPhaseFailed,
			Reason:  "ExecutionFailed",
			Message: err.Error(),
		})
	}

	if operationResult.ProviderID != "" && operationResult.ProviderID != machine.Spec.ProviderID {
		updatedGeneration, err := r.updateMachineProviderID(ctx, machine, operationResult.ProviderID)
		if err != nil {
			return ctrl.Result{}, err
		}

		machine.Spec.ProviderID = operationResult.ProviderID
		machine.Generation = updatedGeneration
	}

	if err := providerMatch.provider.Cleanup(ctx, operationRequest, operationResult); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleanup %s via %s: %w", op.Spec.OperationKind, providerMatch.provider.Name(), err)
	}

	return ctrl.Result{}, store.Finish(ctx, request, machineopscontroller.OperationResult{
		Phase:                     unboundedv1alpha3.OperationPhaseComplete,
		Reason:                    "Succeeded",
		Message:                   fmt.Sprintf("%s completed via %s", op.Spec.OperationKind, providerMatch.provider.Name()),
		ObservedMachineGeneration: machine.Generation,
	})
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
	controller, err := r.controller()
	if err != nil {
		return err
	}

	return controller.SetupWithManager(mgr, "machineoperation-external-power", r.MaxConcurrentReconciles)
}

func shouldReconcileOperation(op *unboundedv1alpha3.MachineOperation) bool {
	return machineopscontroller.ShouldReconcileOperation(op)
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

func (r *MachineOperationReconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}

func (r *MachineOperationReconciler) controller() (*machineopscontroller.Reconciler, error) {
	if r.operationController != nil {
		return r.operationController, nil
	}

	controller, err := machineopscontroller.NewReconciler(machineopscontroller.Config{
		Client: r.Client,
		Operations: []machineopscontroller.Operation{
			{Kind: unboundedv1alpha3.OperationHostReboot, Handler: r.handleOperation},
			{Kind: unboundedv1alpha3.OperationHostPowerOff, Handler: r.handleOperation},
			{Kind: unboundedv1alpha3.OperationHostPowerOn, Handler: r.handleOperation},
			{Kind: unboundedv1alpha3.OperationHostReplace, Handler: r.handleOperation, ReexecuteInProgress: true},
		},
		TargetResolver:             machineopscontroller.TargetResolverFunc(r.resolveOperationTarget),
		UnsupportedOperationPolicy: machineopscontroller.UnsupportedOperationFail,
		Now:                        r.now,
	})
	if err != nil {
		return nil, err
	}

	r.operationController = controller
	return controller, nil
}
