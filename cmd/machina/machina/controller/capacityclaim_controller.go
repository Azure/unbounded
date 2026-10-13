// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineconfigs"
)

const capacityClaimRequeue = 30 * time.Second

// CapacityClaimReconciler allocates existing Machines to CapacityClaims.
type CapacityClaimReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Locker    DomainLocker
	Lifecycle CapacityLifecycle
}

// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=capacityclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=capacityclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=capacityclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=unbounded-cloud.io,resources=machineconfigurationversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch

func (r *CapacityClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var claim unboundedv1alpha3.CapacityClaim
	if err := r.Get(ctx, req.NamespacedName, &claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return r.reconcileRelease(ctx, &claim)
	}

	if err := validateCapacityClaim(&claim); err != nil {
		return ctrl.Result{}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseFailed,
			"InvalidPolicy",
			err.Error(),
			claim.Status.Machine,
		)
	}

	configImage, err := r.configurationImage(ctx, &claim)
	if err != nil {
		return ctrl.Result{}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhasePending,
			"ConfigurationUnavailable",
			err.Error(),
			claim.Status.Machine,
		)
	}

	if err := validateConfiguredImage(&claim, configImage); err != nil {
		return ctrl.Result{}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseFailed,
			"ImagePolicyMismatch",
			err.Error(),
			claim.Status.Machine,
		)
	}

	if !containsString(claim.Finalizers, unboundedv1alpha3.CapacityClaimFinalizer) {
		if err := r.updateClaim(ctx, claim.Name, func(current *unboundedv1alpha3.CapacityClaim) {
			current.Finalizers = append(current.Finalizers, unboundedv1alpha3.CapacityClaimFinalizer)
		}); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	if claim.Status.Machine == nil {
		recovered, recoverErr := r.recoverReservation(ctx, &claim)
		if recoverErr != nil {
			return ctrl.Result{}, recoverErr
		}

		if recovered != nil {
			if err := r.bindClaimStatus(ctx, claim.Name, recovered); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{Requeue: true}, nil
		}

		machine, allocateErr := r.allocate(ctx, &claim)
		if allocateErr != nil {
			if errors.Is(allocateErr, ErrDomainLocked) {
				return ctrl.Result{RequeueAfter: capacityClaimRequeue}, nil
			}

			return ctrl.Result{}, allocateErr
		}

		if machine == nil {
			return ctrl.Result{RequeueAfter: capacityClaimRequeue}, r.setClaimState(
				ctx,
				claim.Name,
				unboundedv1alpha3.CapacityClaimPhasePending,
				"NoMatchingCapacity",
				"No available, unreserved, non-quarantined Machine matches the claim",
				nil,
			)
		}

		if err := r.bindClaimStatus(ctx, claim.Name, machine); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileBound(ctx, &claim, configImage)
}

func validateCapacityClaim(claim *unboundedv1alpha3.CapacityClaim) error {
	if claim.Spec.Consumer.UID == "" {
		return errors.New("consumer.uid is required")
	}

	if claim.Spec.ImagePolicy.RequireNVIDIA && claim.Spec.ImagePolicy.RequiredImage == "" {
		return errors.New("imagePolicy.requireNVIDIA requires an exact imagePolicy.requiredImage trust anchor")
	}

	if claim.Spec.GPU == nil {
		return nil
	}

	gpu := claim.Spec.GPU
	if !strings.EqualFold(gpu.Vendor, "NVIDIA") {
		return fmt.Errorf("GPU vendor %q is unsupported", gpu.Vendor)
	}

	if strings.EqualFold(gpu.Model, "GB200") {
		if gpu.Count != 4 {
			return fmt.Errorf("dedicated NVIDIA GB200 capacity requires exactly 4 GPUs, got %d", gpu.Count)
		}

		if !claim.Spec.ImagePolicy.RequireNVIDIA {
			return errors.New("dedicated NVIDIA GB200 capacity requires imagePolicy.requireNVIDIA")
		}

		if claim.Spec.ImagePolicy.RequiredImage == "" {
			return errors.New("dedicated NVIDIA GB200 capacity requires an exact imagePolicy.requiredImage")
		}
	}

	return nil
}

func validateConfiguredImage(claim *unboundedv1alpha3.CapacityClaim, image string) error {
	if claim.Spec.ImagePolicy.RequireNVIDIA && claim.Spec.ImagePolicy.RequiredImage == "" {
		return errors.New("NVIDIA image validation has no exact requiredImage trust anchor")
	}

	if claim.Spec.ImagePolicy.RequiredImage != "" && claim.Spec.ImagePolicy.RequiredImage != image {
		return fmt.Errorf("configured image %q does not match required image %q", image, claim.Spec.ImagePolicy.RequiredImage)
	}

	return nil
}

func (r *CapacityClaimReconciler) configurationImage(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
) (string, error) {
	version, err := machineconfigs.ResolveVersionFromRef(ctx, r.Client, &claim.Spec.ConfigurationRef)
	if err != nil {
		return "", fmt.Errorf("resolve MachineConfiguration: %w", err)
	}

	if version.Spec.Template.Agent == nil || version.Spec.Template.Agent.Image == "" {
		return "", errors.New("MachineConfiguration has no agent image")
	}

	return version.Spec.Template.Agent.Image, nil
}

func (r *CapacityClaimReconciler) allocate(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
) (*unboundedv1alpha3.Machine, error) {
	machines, err := r.matchingMachines(ctx, claim)
	if err != nil {
		return nil, err
	}

	for i := range machines {
		candidate := &machines[i]
		if !machineEligible(candidate) {
			continue
		}

		domain := candidate.Labels[unboundedv1alpha3.MachineCapacityDomainLabelKey]
		if domain == "" {
			continue
		}

		var reserved *unboundedv1alpha3.Machine

		err := r.locker().WithLock(ctx, domain, func(lockCtx context.Context) error {
			return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				var current unboundedv1alpha3.Machine
				if getErr := r.Get(lockCtx, types.NamespacedName{Name: candidate.Name}, &current); getErr != nil {
					return getErr
				}

				if !machineEligible(&current) {
					return nil
				}

				current.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
					ClaimName:  claim.Name,
					ClaimUID:   claim.UID,
					Domain:     domain,
					ReservedAt: metav1.Now(),
				}
				apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
					Type:               unboundedv1alpha3.MachineConditionReserved,
					Status:             metav1.ConditionTrue,
					Reason:             "CapacityClaimBound",
					Message:            fmt.Sprintf("Reserved by CapacityClaim %s", claim.Name),
					ObservedGeneration: current.Generation,
				})
				apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
					Type:               unboundedv1alpha3.MachineConditionAvailable,
					Status:             metav1.ConditionFalse,
					Reason:             "Reserved",
					Message:            fmt.Sprintf("Reserved by CapacityClaim %s", claim.Name),
					ObservedGeneration: current.Generation,
				})

				if updateErr := r.Status().Update(lockCtx, &current); updateErr != nil {
					return updateErr
				}

				reserved = current.DeepCopy()

				return nil
			})
		})
		if err != nil {
			if errors.Is(err, ErrDomainLocked) {
				continue
			}

			return nil, fmt.Errorf("reserve Machine %s: %w", candidate.Name, err)
		}

		if reserved == nil {
			continue
		}

		if err := r.applyConfiguration(ctx, reserved.Name, &claim.Spec.ConfigurationRef); err != nil {
			return nil, err
		}

		return reserved, nil
	}

	return nil, nil
}

func (r *CapacityClaimReconciler) recoverReservation(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
) (*unboundedv1alpha3.Machine, error) {
	var machines unboundedv1alpha3.MachineList
	if err := r.List(ctx, &machines); err != nil {
		return nil, fmt.Errorf("list Machines for reservation recovery: %w", err)
	}

	var owned []unboundedv1alpha3.Machine

	for i := range machines.Items {
		reservation := machines.Items[i].Status.Reservation
		if reservation != nil && reservation.ClaimName == claim.Name && reservation.ClaimUID == claim.UID {
			owned = append(owned, machines.Items[i])
		}
	}

	if len(owned) == 0 {
		return nil, nil
	}

	sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })

	if len(owned) > 1 {
		for i := range owned {
			if err := r.quarantineMachine(
				ctx,
				&owned[i],
				"DuplicateReservation",
				fmt.Sprintf("CapacityClaim %s is reserved on multiple Machines; manual release is required", claim.Name),
			); err != nil {
				return nil, err
			}
		}

		return nil, fmt.Errorf("CapacityClaim %s is reserved on multiple Machines", claim.Name)
	}

	return &owned[0], nil
}

func (r *CapacityClaimReconciler) quarantineMachine(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
	reason string,
	message string,
) error {
	domain := machine.Status.Reservation.Domain

	return r.locker().WithLock(ctx, domain, func(lockCtx context.Context) error {
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var current unboundedv1alpha3.Machine
			if err := r.Get(lockCtx, types.NamespacedName{Name: machine.Name}, &current); err != nil {
				return err
			}

			apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: unboundedv1alpha3.MachineConditionQuarantined, Status: metav1.ConditionTrue,
				Reason: reason, Message: message, ObservedGeneration: current.Generation,
			})
			apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: unboundedv1alpha3.MachineConditionAvailable, Status: metav1.ConditionFalse,
				Reason: reason, Message: message, ObservedGeneration: current.Generation,
			})

			return r.Status().Update(lockCtx, &current)
		})
	})
}

func (r *CapacityClaimReconciler) reconcileBound(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
	configImage string,
) (ctrl.Result, error) {
	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Status.Machine.Name}, &machine); err != nil {
		reason := "BoundMachineUnavailable"
		if apierrors.IsNotFound(err) {
			reason = "BoundMachineNotFound"
		}

		return ctrl.Result{RequeueAfter: capacityClaimRequeue}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseFailed,
			reason,
			err.Error(),
			claim.Status.Machine,
		)
	}

	if machine.UID != claim.Status.Machine.UID {
		return ctrl.Result{}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseFailed,
			"MachineIdentityChanged",
			"The bound Machine name now has a different UID",
			nil,
		)
	}

	matches, err := machineMatchesClaim(&machine, claim)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !matches {
		return ctrl.Result{}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseFailed,
			"BoundMachineNoLongerMatches",
			"The bound Machine no longer matches the claim selectors",
			claim.Status.Machine,
		)
	}

	reservation := machine.Status.Reservation
	if reservation == nil {
		wins, err := r.claimWinsDuplicate(ctx, claim, &machine)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !wins {
			return ctrl.Result{}, r.setClaimState(
				ctx,
				claim.Name,
				unboundedv1alpha3.CapacityClaimPhaseFailed,
				"DuplicateBindingRejected",
				"Another CapacityClaim deterministically owns this Machine binding",
				nil,
			)
		}

		domain := machine.Labels[unboundedv1alpha3.MachineCapacityDomainLabelKey]
		if domain == "" {
			return ctrl.Result{}, r.setClaimState(
				ctx,
				claim.Name,
				unboundedv1alpha3.CapacityClaimPhaseFailed,
				"MissingCapacityDomain",
				"The bound Machine has no capacity-domain label",
				claim.Status.Machine,
			)
		}

		if err := r.reserveBoundMachine(ctx, claim, &machine, domain); err != nil {
			return ctrl.Result{}, err
		}
	} else if reservation.ClaimName != claim.Name || reservation.ClaimUID != claim.UID {
		return ctrl.Result{}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseFailed,
			"ReservationConflict",
			fmt.Sprintf("Machine is reserved by CapacityClaim %s", reservation.ClaimName),
			nil,
		)
	}

	ready, reason, message := machineSatisfiesClaim(&machine, claim, configImage)
	if !ready {
		if shouldQuarantineAllocation(reason) {
			if err := r.quarantineMachine(ctx, &machine, reason, message); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, r.setClaimState(
				ctx,
				claim.Name,
				unboundedv1alpha3.CapacityClaimPhaseFailed,
				reason,
				message,
				claim.Status.Machine,
			)
		}

		if err := r.provisionBoundMachine(ctx, claim, &machine); err != nil {
			statusErr := r.setClaimState(
				ctx,
				claim.Name,
				unboundedv1alpha3.CapacityClaimPhaseProvisioning,
				"ProvisioningBlocked",
				err.Error(),
				claim.Status.Machine,
			)
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}

			return ctrl.Result{RequeueAfter: capacityClaimRequeue}, err
		}

		return ctrl.Result{RequeueAfter: capacityClaimRequeue}, r.setClaimState(
			ctx,
			claim.Name,
			unboundedv1alpha3.CapacityClaimPhaseProvisioning,
			reason,
			message,
			claim.Status.Machine,
		)
	}

	return ctrl.Result{RequeueAfter: capacityClaimRequeue}, r.setClaimState(
		ctx,
		claim.Name,
		unboundedv1alpha3.CapacityClaimPhaseReady,
		"AllocationReady",
		"Bound Machine has a Ready Node and satisfies image and GPU observations",
		claim.Status.Machine,
	)
}

func (r *CapacityClaimReconciler) provisionBoundMachine(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
	machine *unboundedv1alpha3.Machine,
) error {
	domain := machine.Labels[unboundedv1alpha3.MachineCapacityDomainLabelKey]
	if machine.Status.Reservation != nil && machine.Status.Reservation.Domain != "" {
		domain = machine.Status.Reservation.Domain
	}

	if domain == "" {
		return errors.New("bound Machine has no capacity domain")
	}

	lifecycle := r.Lifecycle
	if lifecycle == nil {
		lifecycle = UnavailableCapacityLifecycle{}
	}

	return r.locker().WithLock(ctx, domain, func(lockCtx context.Context) error {
		var current unboundedv1alpha3.Machine
		if err := r.Get(lockCtx, types.NamespacedName{Name: machine.Name}, &current); err != nil {
			return err
		}

		if current.Status.Reservation == nil ||
			current.Status.Reservation.ClaimName != claim.Name ||
			current.Status.Reservation.ClaimUID != claim.UID {
			return errors.New("bound Machine reservation changed before provisioning")
		}

		return lifecycle.ProvisionWorker(lockCtx, &current)
	})
}

func machineSatisfiesClaim(
	machine *unboundedv1alpha3.Machine,
	claim *unboundedv1alpha3.CapacityClaim,
	configImage string,
) (bool, string, string) {
	if machine.Status.Allocation == nil || machine.Status.Allocation.Node == nil {
		return false, "NodeObservationMissing", "Machine has no Node allocation observation"
	}

	node := machine.Status.Allocation.Node
	if !node.Ready {
		return false, "NodeNotReady", "The observed Kubernetes Node is not Ready"
	}

	if node.InternalIP == "" {
		return false, "NodeIPMissing", "The Ready Kubernetes Node has no observed InternalIP"
	}

	if node.Image == "" || node.Image != configImage {
		return false, "ObservedImageMismatch", fmt.Sprintf("observed image %q does not match configured image %q", node.Image, configImage)
	}

	if claim.Spec.GPU == nil {
		return true, "", ""
	}

	if machine.Status.Allocation.GPU == nil {
		return false, "GPUObservationMissing", "Machine has no GPU allocation observation"
	}

	expected := claim.Spec.GPU

	observed := machine.Status.Allocation.GPU
	if !strings.EqualFold(observed.Vendor, expected.Vendor) || !strings.EqualFold(observed.Model, expected.Model) {
		return false, "GPUModelMismatch", fmt.Sprintf("observed GPU %s %s does not match %s %s", observed.Vendor, observed.Model, expected.Vendor, expected.Model)
	}

	if observed.Count != expected.Count || observed.HealthyCount != expected.Count {
		return false, "GPUHealthMismatch", fmt.Sprintf("expected %d healthy GPUs, observed %d total and %d healthy", expected.Count, observed.Count, observed.HealthyCount)
	}

	if (expected.RequireFabric || strings.EqualFold(expected.Model, "GB200")) && !observed.FabricHealthy {
		return false, "GPUFabricUnhealthy", "required GPU fabric is not healthy"
	}

	return true, "", ""
}

func shouldQuarantineAllocation(reason string) bool {
	switch reason {
	case "ObservedImageMismatch",
		"GPUObservationMissing",
		"GPUModelMismatch",
		"GPUHealthMismatch",
		"GPUFabricUnhealthy":
		return true
	default:
		return false
	}
}

func (r *CapacityClaimReconciler) reconcileRelease(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
) (ctrl.Result, error) {
	if !containsString(claim.Finalizers, unboundedv1alpha3.CapacityClaimFinalizer) {
		return ctrl.Result{}, nil
	}

	if claim.Status.Machine == nil {
		recovered, err := r.recoverReservation(ctx, claim)
		if err != nil {
			return ctrl.Result{}, err
		}

		if recovered != nil {
			if err := r.bindClaimStatus(ctx, claim.Name, recovered); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{Requeue: true}, nil
		}

		return ctrl.Result{}, r.removeFinalizer(ctx, claim.Name)
	}

	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Status.Machine.Name}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			blockedErr := errors.New("bound Machine is missing; release barriers cannot be verified")

			stage := claim.Status.ReleaseStage
			if stage == "" {
				stage = unboundedv1alpha3.CapacityReleaseStageCordonDrain
			}

			statusErr := r.setReleaseStage(
				ctx,
				claim.Name,
				stage,
				"ReleaseBlocked",
				blockedErr.Error(),
			)
			if statusErr != nil {
				return ctrl.Result{}, statusErr
			}

			return ctrl.Result{RequeueAfter: capacityClaimRequeue}, blockedErr
		}

		return ctrl.Result{}, err
	}

	stage := claim.Status.ReleaseStage
	if stage == "" {
		stage = unboundedv1alpha3.CapacityReleaseStageCordonDrain
	}

	if err := r.setReleaseStage(ctx, claim.Name, stage, "ReleaseInProgress", fmt.Sprintf("Executing release stage %s", stage)); err != nil {
		return ctrl.Result{}, err
	}

	lifecycle := r.Lifecycle
	if lifecycle == nil {
		lifecycle = UnavailableCapacityLifecycle{}
	}

	if stage == unboundedv1alpha3.CapacityReleaseStageComplete {
		return ctrl.Result{}, r.removeFinalizer(ctx, claim.Name)
	}

	domain := machine.Labels[unboundedv1alpha3.MachineCapacityDomainLabelKey]
	if machine.Status.Reservation != nil && machine.Status.Reservation.Domain != "" {
		domain = machine.Status.Reservation.Domain
	}

	if domain == "" {
		err := errors.New("bound Machine has no capacity domain")

		statusErr := r.setReleaseStage(ctx, claim.Name, stage, "ReleaseBlocked", err.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}

		return ctrl.Result{RequeueAfter: capacityClaimRequeue}, err
	}

	next := unboundedv1alpha3.CapacityReleaseStageComplete

	err := r.locker().WithLock(ctx, domain, func(lockCtx context.Context) error {
		switch stage {
		case unboundedv1alpha3.CapacityReleaseStageCordonDrain:
			next = unboundedv1alpha3.CapacityReleaseStageRevokeCredentials

			return lifecycle.CordonDrain(lockCtx, &machine)
		case unboundedv1alpha3.CapacityReleaseStageRevokeCredentials:
			next = unboundedv1alpha3.CapacityReleaseStageResetWorker

			return lifecycle.RevokeCredentials(lockCtx, &machine)
		case unboundedv1alpha3.CapacityReleaseStageResetWorker:
			if claim.Spec.ReleasePolicy == unboundedv1alpha3.CapacityReleaseRetainNode {
				next = unboundedv1alpha3.CapacityReleaseStageReleaseReservation
			} else {
				next = unboundedv1alpha3.CapacityReleaseStageDeleteNode
			}

			if err := lifecycle.ResetWorker(lockCtx, &machine); err != nil {
				return err
			}

			return r.clearMachineAllocation(lockCtx, machine.Name, "WorkerReset")
		case unboundedv1alpha3.CapacityReleaseStageDeleteNode:
			next = unboundedv1alpha3.CapacityReleaseStageReleaseReservation

			if err := lifecycle.DeleteNode(lockCtx, &machine); err != nil {
				return err
			}

			return r.clearMachineAllocation(lockCtx, machine.Name, "NodeDeleted")
		case unboundedv1alpha3.CapacityReleaseStageReleaseReservation:
			next = unboundedv1alpha3.CapacityReleaseStageComplete

			return r.clearReservation(lockCtx, machine.Name, claim)
		default:
			return fmt.Errorf("unknown release stage %q", stage)
		}
	})
	if err != nil {
		statusErr := r.setReleaseStage(ctx, claim.Name, stage, "ReleaseBlocked", err.Error())
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}

		return ctrl.Result{RequeueAfter: capacityClaimRequeue}, err
	}

	if next == unboundedv1alpha3.CapacityReleaseStageComplete {
		if err := r.setReleaseStage(ctx, claim.Name, next, "Released", "All release barriers completed"); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, r.removeFinalizer(ctx, claim.Name)
	}

	if err := r.setReleaseStage(ctx, claim.Name, next, "ReleaseInProgress", fmt.Sprintf("Next release stage is %s", next)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

func (r *CapacityClaimReconciler) matchingMachines(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
) ([]unboundedv1alpha3.Machine, error) {
	selectors := []*metav1.LabelSelector{
		&claim.Spec.MachineSelector,
		claim.Spec.SiteSelector,
		claim.Spec.DomainSelector,
	}

	var parsed []labels.Selector

	for _, selector := range selectors {
		if selector == nil {
			continue
		}

		value, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("parse CapacityClaim selector: %w", err)
		}

		parsed = append(parsed, value)
	}

	var list unboundedv1alpha3.MachineList
	if err := r.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list Machines: %w", err)
	}

	var matches []unboundedv1alpha3.Machine

	for i := range list.Items {
		set := labels.Set(list.Items[i].Labels)
		matched := true

		for _, selector := range parsed {
			if !selector.Matches(set) {
				matched = false
				break
			}
		}

		if matched {
			matches = append(matches, list.Items[i])
		}
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })

	return matches, nil
}

func machineEligible(machine *unboundedv1alpha3.Machine) bool {
	return machine.Status.Reservation == nil &&
		apimeta.IsStatusConditionTrue(machine.Status.Conditions, unboundedv1alpha3.MachineConditionAvailable) &&
		!apimeta.IsStatusConditionTrue(machine.Status.Conditions, unboundedv1alpha3.MachineConditionQuarantined)
}

func machineMatchesClaim(
	machine *unboundedv1alpha3.Machine,
	claim *unboundedv1alpha3.CapacityClaim,
) (bool, error) {
	selectors := []*metav1.LabelSelector{
		&claim.Spec.MachineSelector,
		claim.Spec.SiteSelector,
		claim.Spec.DomainSelector,
	}
	set := labels.Set(machine.Labels)

	for _, selector := range selectors {
		if selector == nil {
			continue
		}

		parsed, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return false, fmt.Errorf("parse CapacityClaim selector: %w", err)
		}

		if !parsed.Matches(set) {
			return false, nil
		}
	}

	return true, nil
}

func (r *CapacityClaimReconciler) reserveBoundMachine(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
	machine *unboundedv1alpha3.Machine,
	domain string,
) error {
	return r.locker().WithLock(ctx, domain, func(lockCtx context.Context) error {
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var current unboundedv1alpha3.Machine
			if err := r.Get(lockCtx, types.NamespacedName{Name: machine.Name}, &current); err != nil {
				return err
			}

			if current.Status.Reservation != nil {
				if current.Status.Reservation.ClaimName == claim.Name && current.Status.Reservation.ClaimUID == claim.UID {
					return nil
				}

				return fmt.Errorf("machine is reserved by CapacityClaim %s", current.Status.Reservation.ClaimName)
			}

			current.Status.Reservation = &unboundedv1alpha3.MachineReservationStatus{
				ClaimName:  claim.Name,
				ClaimUID:   claim.UID,
				Domain:     domain,
				ReservedAt: metav1.Now(),
			}
			apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: unboundedv1alpha3.MachineConditionReserved, Status: metav1.ConditionTrue,
				Reason: "CapacityClaimRecovered", Message: fmt.Sprintf("Reserved by CapacityClaim %s", claim.Name),
				ObservedGeneration: current.Generation,
			})
			apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
				Type: unboundedv1alpha3.MachineConditionAvailable, Status: metav1.ConditionFalse,
				Reason: "Reserved", Message: fmt.Sprintf("Reserved by CapacityClaim %s", claim.Name),
				ObservedGeneration: current.Generation,
			})

			return r.Status().Update(lockCtx, &current)
		})
	})
}

func (r *CapacityClaimReconciler) claimWinsDuplicate(
	ctx context.Context,
	claim *unboundedv1alpha3.CapacityClaim,
	machine *unboundedv1alpha3.Machine,
) (bool, error) {
	var claims unboundedv1alpha3.CapacityClaimList
	if err := r.List(ctx, &claims); err != nil {
		return false, fmt.Errorf("list CapacityClaims for duplicate repair: %w", err)
	}

	type identity struct {
		name string
		uid  types.UID
	}

	var bound []identity

	for i := range claims.Items {
		ref := claims.Items[i].Status.Machine
		if ref != nil && ref.Name == machine.Name && ref.UID == machine.UID {
			bound = append(bound, identity{name: claims.Items[i].Name, uid: claims.Items[i].UID})
		}
	}

	sort.Slice(bound, func(i, j int) bool {
		if bound[i].name != bound[j].name {
			return bound[i].name < bound[j].name
		}

		return bound[i].uid < bound[j].uid
	})

	return len(bound) == 0 || (bound[0].name == claim.Name && bound[0].uid == claim.UID), nil
}

func (r *CapacityClaimReconciler) clearReservation(
	ctx context.Context,
	machineName string,
	claim *unboundedv1alpha3.CapacityClaim,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine unboundedv1alpha3.Machine
		if err := r.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return client.IgnoreNotFound(err)
		}

		reservation := machine.Status.Reservation
		if reservation == nil {
			return nil
		}

		if reservation.ClaimName != claim.Name || reservation.ClaimUID != claim.UID {
			return fmt.Errorf("refusing to clear reservation owned by CapacityClaim %s", reservation.ClaimName)
		}

		machine.Status.Reservation = nil
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type: unboundedv1alpha3.MachineConditionReserved, Status: metav1.ConditionFalse,
			Reason: "Released", Message: fmt.Sprintf("CapacityClaim %s completed release barriers", claim.Name),
			ObservedGeneration: machine.Generation,
		})

		availableStatus := metav1.ConditionTrue
		availableReason := "Released"
		availableMessage := "Machine inventory is available for allocation"

		if apimeta.IsStatusConditionTrue(machine.Status.Conditions, unboundedv1alpha3.MachineConditionQuarantined) {
			availableStatus = metav1.ConditionFalse
			availableReason = "Quarantined"
			availableMessage = "Machine is quarantined"
		}

		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type: unboundedv1alpha3.MachineConditionAvailable, Status: availableStatus,
			Reason: availableReason, Message: availableMessage,
			ObservedGeneration: machine.Generation,
		})

		return r.Status().Update(ctx, &machine)
	})
}

func (r *CapacityClaimReconciler) clearMachineAllocation(
	ctx context.Context,
	machineName string,
	reason string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine unboundedv1alpha3.Machine
		if err := r.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return client.IgnoreNotFound(err)
		}

		machine.Status.Allocation = nil
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               unboundedv1alpha3.MachineConditionAllocationReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            "Worker allocation observations were cleared during release",
			ObservedGeneration: machine.Generation,
		})

		return r.Status().Update(ctx, &machine)
	})
}

func (r *CapacityClaimReconciler) applyConfiguration(
	ctx context.Context,
	machineName string,
	ref *unboundedv1alpha3.MachineConfigurationRef,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var machine unboundedv1alpha3.Machine
		if err := r.Get(ctx, types.NamespacedName{Name: machineName}, &machine); err != nil {
			return err
		}

		machine.Spec.ConfigurationRef = ref.DeepCopy()

		return r.Update(ctx, &machine)
	})
}

func (r *CapacityClaimReconciler) bindClaimStatus(
	ctx context.Context,
	claimName string,
	machine *unboundedv1alpha3.Machine,
) error {
	ref := &unboundedv1alpha3.BoundMachineReference{Name: machine.Name, UID: machine.UID}

	return r.setClaimState(ctx, claimName, unboundedv1alpha3.CapacityClaimPhaseBound, "MachineReserved", fmt.Sprintf("Reserved Machine %s", machine.Name), ref)
}

func (r *CapacityClaimReconciler) setClaimState(
	ctx context.Context,
	claimName string,
	phase unboundedv1alpha3.CapacityClaimPhase,
	reason string,
	message string,
	machine *unboundedv1alpha3.BoundMachineReference,
) error {
	return r.updateClaimStatus(ctx, claimName, func(claim *unboundedv1alpha3.CapacityClaim) {
		claim.Status.Phase = phase
		claim.Status.ObservedGeneration = claim.Generation
		claim.Status.Machine = machine

		boundStatus := metav1.ConditionFalse
		if machine != nil {
			boundStatus = metav1.ConditionTrue
		}

		apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type: unboundedv1alpha3.CapacityClaimConditionBound, Status: boundStatus,
			Reason: reason, Message: message, ObservedGeneration: claim.Generation,
		})

		readyStatus := metav1.ConditionFalse
		if phase == unboundedv1alpha3.CapacityClaimPhaseReady {
			readyStatus = metav1.ConditionTrue
		}

		apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type: unboundedv1alpha3.CapacityClaimConditionReady, Status: readyStatus,
			Reason: reason, Message: message, ObservedGeneration: claim.Generation,
		})

		configurationStatus := metav1.ConditionTrue

		switch reason {
		case "ConfigurationUnavailable", "ImagePolicyMismatch", "InvalidPolicy":
			configurationStatus = metav1.ConditionFalse
		}

		apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type: unboundedv1alpha3.CapacityClaimConditionConfigurationReady, Status: configurationStatus,
			Reason: reason, Message: message, ObservedGeneration: claim.Generation,
		})
	})
}

func (r *CapacityClaimReconciler) setReleaseStage(
	ctx context.Context,
	claimName string,
	stage unboundedv1alpha3.CapacityReleaseStage,
	reason string,
	message string,
) error {
	return r.updateClaimStatus(ctx, claimName, func(claim *unboundedv1alpha3.CapacityClaim) {
		claim.Status.Phase = unboundedv1alpha3.CapacityClaimPhaseReleasing
		claim.Status.ObservedGeneration = claim.Generation
		claim.Status.ReleaseStage = stage

		status := metav1.ConditionFalse
		if stage == unboundedv1alpha3.CapacityReleaseStageComplete {
			status = metav1.ConditionTrue
		}

		apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
			Type: unboundedv1alpha3.CapacityClaimConditionReleased, Status: status,
			Reason: reason, Message: message, ObservedGeneration: claim.Generation,
		})
	})
}

func (r *CapacityClaimReconciler) updateClaimStatus(
	ctx context.Context,
	name string,
	mutate func(*unboundedv1alpha3.CapacityClaim),
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var claim unboundedv1alpha3.CapacityClaim
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &claim); err != nil {
			return client.IgnoreNotFound(err)
		}

		mutate(&claim)

		return r.Status().Update(ctx, &claim)
	})
}

func (r *CapacityClaimReconciler) updateClaim(
	ctx context.Context,
	name string,
	mutate func(*unboundedv1alpha3.CapacityClaim),
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var claim unboundedv1alpha3.CapacityClaim
		if err := r.Get(ctx, types.NamespacedName{Name: name}, &claim); err != nil {
			return client.IgnoreNotFound(err)
		}

		mutate(&claim)

		return r.Update(ctx, &claim)
	})
}

func (r *CapacityClaimReconciler) removeFinalizer(ctx context.Context, name string) error {
	return r.updateClaim(ctx, name, func(claim *unboundedv1alpha3.CapacityClaim) {
		claim.Finalizers = removeString(claim.Finalizers, unboundedv1alpha3.CapacityClaimFinalizer)
	})
}

func (r *CapacityClaimReconciler) locker() DomainLocker {
	if r.Locker != nil {
		return r.Locker
	}

	return &MemoryDomainLocker{}
}

func (r *CapacityClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("capacityclaim").
		For(&unboundedv1alpha3.CapacityClaim{}).
		Watches(
			&unboundedv1alpha3.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.findClaimsForMachine),
		).
		Watches(
			&unboundedv1alpha3.MachineConfiguration{},
			handler.EnqueueRequestsFromMapFunc(r.findClaimsForConfiguration),
		).
		Watches(
			&unboundedv1alpha3.MachineConfigurationVersion{},
			handler.EnqueueRequestsFromMapFunc(r.findClaimsForConfigurationVersion),
		).
		Complete(r)
}

func (r *CapacityClaimReconciler) findClaimsForMachine(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	machine, ok := obj.(*unboundedv1alpha3.Machine)
	if !ok {
		return nil
	}

	var claims unboundedv1alpha3.CapacityClaimList
	if err := r.List(ctx, &claims); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list CapacityClaims for Machine event")

		return nil
	}

	var requests []reconcile.Request

	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Status.Machine != nil && claim.Status.Machine.Name == machine.Name {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name}})
			continue
		}

		matches, err := r.matchingMachines(ctx, claim)
		if err != nil {
			continue
		}

		for j := range matches {
			if matches[j].Name == machine.Name {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name}})
				break
			}
		}
	}

	return requests
}

func (r *CapacityClaimReconciler) findClaimsForConfiguration(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	configuration, ok := obj.(*unboundedv1alpha3.MachineConfiguration)
	if !ok {
		return nil
	}

	return r.claimRequestsForConfiguration(ctx, configuration.Name, nil)
}

func (r *CapacityClaimReconciler) findClaimsForConfigurationVersion(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	version, ok := obj.(*unboundedv1alpha3.MachineConfigurationVersion)
	if !ok {
		return nil
	}

	configurationName := version.Labels[unboundedv1alpha3.MCVConfigurationLabelKey]
	if configurationName == "" {
		return nil
	}

	return r.claimRequestsForConfiguration(ctx, configurationName, &version.Spec.Version)
}

func (r *CapacityClaimReconciler) claimRequestsForConfiguration(
	ctx context.Context,
	configurationName string,
	version *int32,
) []reconcile.Request {
	var claims unboundedv1alpha3.CapacityClaimList
	if err := r.List(ctx, &claims); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list CapacityClaims for MachineConfiguration event")

		return nil
	}

	var requests []reconcile.Request

	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.ConfigurationRef.Name != configurationName {
			continue
		}

		if version != nil && claim.Spec.ConfigurationRef.Version != nil &&
			*claim.Spec.ConfigurationRef.Version != *version {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: claim.Name},
		})
	}

	return requests
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}

	return false
}

func removeString(values []string, value string) []string {
	result := values[:0]
	for _, item := range values {
		if item != value {
			result = append(result, item)
		}
	}

	return result
}
