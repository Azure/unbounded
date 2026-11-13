// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// machineFencePatch is the only change the controller makes to a Machine's
// spec when it fences the Machine.
var machineFencePatch = client.RawPatch(types.MergePatchType, []byte(`{"spec":{"fenced":true}}`))

// reconcileRetired fences a retired allocation, waits for release to be
// authorized, releases the host and finishes.
func (r *MachineAllocationReconciler) reconcileRetired(ctx context.Context, s *allocationState, signal string) (ctrl.Result, error) {
	authorized := s.allocation.Spec.Lifecycle == unboundedv1alpha3.MachineAllocationReleased

	// Without the finalizer no token, Machine or operation was ever created.
	if !s.hasFinalizer() {
		if authorized {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionTrue, allocationReasonReleased,
				"no provider effect was started")
		} else {
			r.awaitRelease(s, signal)
		}

		return r.finishWrite(ctx, s)
	}

	fenced, err := r.fence(ctx, s, signal)
	if err != nil {
		return ctrl.Result{}, err
	}

	if s.status.Snapshot == nil {
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionAllocated, metav1.ConditionFalse, allocationReasonSnapshotMissing,
			"the finalizer is present but status.snapshot is missing; the status was lost and the allocation needs manual recovery")

		return r.finishWrite(ctx, s)
	}

	allocate, found, err := r.observeRecordedAllocate(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !found || !fenced {
		return r.finishWrite(ctx, s)
	}

	if !authorized {
		r.awaitRelease(s, signal)
		return r.finishWrite(ctx, s)
	}

	released, err := r.release(ctx, s, allocate)
	if err != nil {
		return ctrl.Result{}, err
	}

	if released {
		r.commit(s)
	}

	return r.finishWrite(ctx, s)
}

// commit sets Released=True, the commit point of the finish, which is
// written before any journal finalizer is removed. The status write that
// carries it must be the only write of its reconcile, so a reconcile that
// already wrote something else, such as the fence, commits in a later one.
func (r *MachineAllocationReconciler) commit(s *allocationState) {
	if s.wrote {
		s.wait(allocationWaitInterval)
		return
	}

	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionTrue, allocationReasonReleased,
		"the host and the bootstrap token are released")
}

func (r *MachineAllocationReconciler) awaitRelease(s *allocationState, signal string) {
	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonAwaitingRelease,
		fmt.Sprintf("retired (%s); the host is released only after the consumer sets spec.lifecycle to Released", signal))
}

// fence fences the Machine and revokes the bootstrap token, and reports
// whether both are confirmed: the token is gone and the Machine is fenced,
// being deleted or gone.
func (r *MachineAllocationReconciler) fence(ctx context.Context, s *allocationState, signal string) (bool, error) {
	machine, err := r.boundMachine(ctx, s)
	if err != nil {
		return false, err
	}

	if machine != nil && !machine.Spec.Fenced && machine.DeletionTimestamp.IsZero() {
		s.wrote = true

		if err := r.Patch(ctx, machine, machineFencePatch); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("fence Machine %s: %w", machine.Name, err)
		}
	}

	revoked, message, err := r.revokeToken(ctx, s)
	if err != nil {
		return false, err
	}

	if !revoked {
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionFalse, allocationReasonFencing, message)
		return false, nil
	}

	r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionFenced, metav1.ConditionTrue, signal,
		fmt.Sprintf("retired (%s); the bootstrap token is revoked and the Machine is fenced, being deleted or gone", signal))

	return true, nil
}

// revokeToken deletes the allocation's bootstrap token Secret and reports
// whether a live read shows it gone. A Secret with the token's name that is
// not this allocation's is never deleted.
func (r *MachineAllocationReconciler) revokeToken(ctx context.Context, s *allocationState) (bool, string, error) {
	key := client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: s.names.tokenSecret}

	for attempt := 0; attempt < 2; attempt++ {
		var secret corev1.Secret
		if err := r.APIReader.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return true, "", nil
			}

			return false, "", fmt.Errorf("get bootstrap token Secret %s/%s: %w", key.Namespace, key.Name, err)
		}

		if secret.Labels[unboundedv1alpha3.LabelAllocationUID] != string(s.allocation.UID) {
			return false, fmt.Sprintf("bootstrap token Secret %s/%s is not this allocation's and is left in place; the fence is incomplete",
				key.Namespace, key.Name), nil
		}

		if attempt > 0 {
			break
		}

		uid := secret.UID
		s.wrote = true

		if err := r.Delete(ctx, &secret, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			return false, "", fmt.Errorf("delete bootstrap token Secret %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	s.wait(allocationWaitInterval)

	return false, fmt.Sprintf("waiting for bootstrap token Secret %s/%s to be deleted", key.Namespace, key.Name), nil
}

// observeRecordedAllocate copies the state of the recorded HostAllocate, if
// any, into the status. It reports false when the recorded operation is
// missing or replaced.
func (r *MachineAllocationReconciler) observeRecordedAllocate(
	ctx context.Context,
	s *allocationState,
) (*unboundedv1alpha3.MachineOperation, bool, error) {
	recorded := s.status.Operations.Allocate
	if recorded == nil {
		return nil, true, nil
	}

	operation, err := r.recordedOperation(ctx, recorded)
	if err != nil {
		return nil, false, err
	}

	if operation == nil {
		r.allocateOperationMissing(s, recorded)
		return nil, false, nil
	}

	if err := r.observeAllocateOperation(ctx, s, operation); err != nil {
		return nil, false, err
	}

	return operation, true, nil
}

// release runs the HostRelease for a recorded HostAllocate once it is
// terminal. It reports true when the host is released or no HostAllocate was
// recorded.
func (r *MachineAllocationReconciler) release(
	ctx context.Context,
	s *allocationState,
	allocate *unboundedv1alpha3.MachineOperation,
) (bool, error) {
	if allocate == nil {
		return true, nil
	}

	if !allocate.Status.IsTerminal() {
		if reason, _ := operationCompletion(allocate); reason == allocationReasonBeginOutcomeUnknown {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonBeginOutcomeUnknown,
				fmt.Sprintf("HostAllocate MachineOperation %s may have begun and cannot be begun again; the host needs manual recovery", allocate.Name))

			return false, nil
		}

		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonAwaitingAllocateOperation,
			fmt.Sprintf("waiting for HostAllocate MachineOperation %s to finish", allocate.Name))
		s.wait(allocationWaitInterval)

		return false, nil
	}

	if ownershipUnknown(allocate) {
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonOwnershipUnknown,
			fmt.Sprintf("HostAllocate MachineOperation %s may have created a host and reported no owned resources; the host needs manual recovery",
				allocate.Name))

		return false, nil
	}

	var releaseOperation *unboundedv1alpha3.MachineOperation

	if recorded := s.status.Operations.Release; recorded != nil {
		operation, err := r.recordedOperation(ctx, recorded)
		if err != nil {
			return false, err
		}

		if operation == nil {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonReleaseOperationMissing,
				fmt.Sprintf("recorded HostRelease MachineOperation %s (%s) is missing or replaced; the host needs manual recovery", recorded.Name, recorded.UID))

			return false, nil
		}

		releaseOperation = operation
	}

	if releaseOperation == nil || releaseOperation.Status.Phase != unboundedv1alpha3.OperationPhaseComplete {
		machine, err := r.boundMachine(ctx, s)
		if err != nil {
			return false, err
		}

		if machine == nil {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonMachineMissing,
				fmt.Sprintf("Machine %s is gone before its host was released; the host needs manual recovery", s.names.machine))

			return false, nil
		}
	}

	if releaseOperation == nil {
		operation, err := r.ensureOperation(ctx, s, unboundedv1alpha3.OperationHostRelease, nil)
		if err != nil {
			return false, err
		}

		if operation == nil {
			r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, string(unboundedv1alpha3.FailureProviderConflict),
				fmt.Sprintf("MachineOperation %s exists and is not this allocation's HostRelease", s.names.releaseOperation))

			return false, nil
		}

		s.status.Operations.Release = &unboundedv1alpha3.UIDReference{Name: operation.Name, UID: operation.UID}
		releaseOperation = operation
	}

	switch releaseOperation.Status.Phase {
	case unboundedv1alpha3.OperationPhaseComplete:
		return true, nil
	case unboundedv1alpha3.OperationPhaseFailed:
		reason, message := operationCompletion(releaseOperation)
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonReleaseFailed,
			fmt.Sprintf("HostRelease MachineOperation %s failed: %s: %s", releaseOperation.Name, reason, message))
	default:
		r.setCondition(s, unboundedv1alpha3.MachineAllocationConditionReleased, metav1.ConditionFalse, allocationReasonReleasing,
			fmt.Sprintf("waiting for HostRelease MachineOperation %s", releaseOperation.Name))
	}

	return false, nil
}

// ownershipUnknown reports whether a terminal HostAllocate may have created
// a host whose resources it did not report: the engine failed it with
// OwnershipUnknown, or it has a provider handle and no allocation result.
func ownershipUnknown(allocate *unboundedv1alpha3.MachineOperation) bool {
	if reason, _ := operationCompletion(allocate); reason == allocationReasonOwnershipUnknown {
		return true
	}

	return hasProviderHandle(allocate) && allocate.Status.Allocation == nil
}

// cleanup runs after the commit. It removes the controller's finalizer from
// the operations, the Machine and the input ConfigMaps, and then the
// allocation finalizer. It observes no operation and writes no status, so a
// step that crashes repeats safely, even when the objects are being deleted
// and vanish with their last finalizer.
func (r *MachineAllocationReconciler) cleanup(ctx context.Context, s *allocationState) error {
	for _, name := range []string{s.names.allocateOperation, s.names.releaseOperation} {
		var operation unboundedv1alpha3.MachineOperation

		found, err := r.getConfirmed(ctx, client.ObjectKey{Name: name}, &operation)
		if err != nil {
			return err
		}

		if found && sameUIDReference(operation.Spec.AllocationRef, s.reference()) {
			if err := r.removeFinalizer(ctx, s, &operation, unboundedv1alpha3.MachineAllocationFinalizer); err != nil {
				return err
			}
		}
	}

	machine, err := r.boundMachine(ctx, s)
	if err != nil {
		return err
	}

	if machine != nil {
		if err := r.removeFinalizer(ctx, s, machine, unboundedv1alpha3.MachineAllocationFinalizer); err != nil {
			return err
		}
	}

	if snapshot := s.status.Snapshot; snapshot != nil {
		if err := r.unprotectInputs(ctx, s, snapshot); err != nil {
			return err
		}
	}

	return r.removeFinalizer(ctx, s, s.allocation, unboundedv1alpha3.MachineAllocationFinalizer)
}
