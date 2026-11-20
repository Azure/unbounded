// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/helpers/statusmsg"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	defaultProviderPollInterval        = 15 * time.Second
	defaultProviderStallAfter          = 75 * time.Minute
	defaultProviderStalledPollInterval = 5 * time.Minute
)

func (r *MachineOperationReconciler) reconcileResumableOperation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
) (ctrl.Result, error) {
	target, ok := operationTarget(op, machine.Name)
	if !ok {
		return r.failOperation(ctx, op, "TargetStateMissing", fmt.Sprintf("MachineOperation target %s is missing", machine.Name))
	}

	if target.Phase == unboundedv1alpha3.OperationPhaseComplete {
		return r.completeOperation(ctx, op, target.ObservedGeneration, target.Message)
	}

	if target.Phase == unboundedv1alpha3.OperationPhaseFailed {
		return r.failOperation(ctx, op, "TargetFailed", target.Message)
	}

	message := fmt.Sprintf("executing %s via %s", op.Spec.OperationKind, providerMatch.provider.Name())
	if err := r.ensureOperationInProgress(ctx, client.ObjectKey{Name: op.Name}, op, message); err != nil {
		if errors.Is(err, errOperationFinished) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	target, ok = operationTarget(op, machine.Name)
	if !ok {
		return r.failOperation(ctx, op, "TargetStateMissing", fmt.Sprintf("MachineOperation target %s is missing", machine.Name))
	}

	if target.ProviderOperation != nil && target.ProviderOperation.Provider != providerMatch.provider.Name() {
		message := fmt.Sprintf("persisted provider operation belongs to %q, not %q", target.ProviderOperation.Provider, providerMatch.provider.Name())

		return r.failOperationTarget(ctx, op, machine.Name, "ProviderOperationMismatch", message)
	}

	auth, authFailure, err := r.resolveOperationAuth(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	if authFailure != nil {
		if target.ProviderOperation != nil {
			return r.waitForResumableOperation(ctx, op.Name, machine.Name, "ProviderAuthUnavailable", authFailure.Message, r.providerStalledPollInterval())
		}

		return r.failOperationTarget(ctx, op, machine.Name, authFailure.Reason, authFailure.Message)
	}

	host, err := resolveMachineHost(machine)
	if err != nil {
		return r.failOperationTarget(ctx, op, machine.Name, "InvalidHost", err.Error())
	}

	// The replacement bootstrap data is built for Begin and for every Poll.
	// It can only be handed to a host when the host is created, and a
	// provider that has to delete the old host first creates the new one from
	// Poll. It carries a bootstrap token, so it is built again each time
	// instead of being stored in the persisted handle.
	request, err := r.operationRequest(ctx, op, machine, target, host.providerID, auth, providerMatch.operation.RequiresReplaceUserData())

	// bootstrapDataErr is why a Poll is called without the bootstrap data.
	var bootstrapDataErr error

	if err != nil && target.ProviderOperation != nil {
		// The provider already accepted the operation and may have changed
		// the host, so the operation must not end here, and most Polls only
		// observe and do not need the data. Poll without it: a Poll that
		// needs it returns an error and is called again.
		log.FromContext(ctx).Error(err, "poll without replacement bootstrap data", "machineOperation", op.Name, "machine", machine.Name)

		bootstrapDataErr = err
		request, err = r.operationRequest(ctx, op, machine, target, host.providerID, auth, false)
	}

	if err != nil {
		return r.failOperationTarget(ctx, op, machine.Name, "RequestBuildFailed", err.Error())
	}

	if target.ProviderOperation != nil {
		return r.pollResumableOperation(ctx, op, machine, providerMatch, request, target, bootstrapDataErr)
	}

	begin, beginErr := providerMatch.operation.Begin(ctx, request)
	if beginErr != nil {
		return r.handleBeginError(ctx, op, machine.Name, beginErr)
	}

	if strings.TrimSpace(begin.Operation.OperationID) == "" {
		return r.handleBeginError(ctx, op, machine.Name, errors.New("provider returned an empty operation ID"))
	}

	return r.applyBeginResult(ctx, op, machine, providerMatch, begin)
}

func (r *MachineOperationReconciler) handleBeginError(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machineName string,
	beginErr error,
) (ctrl.Result, error) {
	message := fmt.Sprintf("begin provider operation: %v", beginErr)
	reason := "BeginRetry"

	var permanentErr *publicmachineops.PermanentError
	if errors.As(beginErr, &permanentErr) {
		reason = valueOrDefault(permanentErr.Reason, "PermanentProviderFailure")
	}

	if err := r.updateOperationTarget(ctx, op.Name, machineName, func(target *unboundedv1alpha3.MachineOperationTargetStatus) {
		now := r.now()
		target.Attempts++
		target.LastAttemptAt = &now

		target.Message = message
		if target.StartedAt == nil {
			target.StartedAt = &now
		}

		if permanentErr != nil {
			r.markOperationTargetFinished(target, unboundedv1alpha3.OperationPhaseFailed, fmt.Sprintf("%s: %s", reason, message))
			target.Stage = ""
		}
	}); err != nil {
		return ctrl.Result{}, err
	}

	if permanentErr != nil {
		return r.failOperation(ctx, op, reason, message)
	}

	return ctrl.Result{RequeueAfter: r.providerPollInterval()}, nil
}

func (r *MachineOperationReconciler) applyBeginResult(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
	result publicmachineops.BeginResult,
) (ctrl.Result, error) {
	acceptedAt := r.now()

	if err := r.updateOperationTarget(ctx, op.Name, machine.Name, func(target *unboundedv1alpha3.MachineOperationTargetStatus) {
		target.Attempts++
		target.LastAttemptAt = &acceptedAt
		target.Phase = unboundedv1alpha3.OperationPhaseInProgress
		target.Stage = unboundedv1alpha3.OperationStageWaitingProvider

		target.Message = valueOrDefault(result.Message, fmt.Sprintf("waiting for provider operation %s", result.Operation.OperationID))
		if target.StartedAt == nil {
			target.StartedAt = &acceptedAt
		}

		target.ProviderOperation = providerOperationStatus(providerMatch.provider.Name(), result.Operation)
		setTargetCondition(target, metav1.Condition{
			Type:    unboundedv1alpha3.MachineOperationConditionProviderOperationStalled,
			Status:  metav1.ConditionFalse,
			Reason:  "WithinExpectedDuration",
			Message: "provider operation is within its expected duration",
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: durationOrDefault(result.RequeueAfter, r.providerPollInterval())}, nil
}

func (r *MachineOperationReconciler) pollResumableOperation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
	request OperationRequest,
	target *unboundedv1alpha3.MachineOperationTargetStatus,
	bootstrapDataErr error,
) (ctrl.Result, error) {
	providerOperation := publicmachineops.ProviderOperation{
		OperationID: target.ProviderOperation.OperationID,
		ResumeToken: target.ProviderOperation.ResumeToken,
	}

	result, err := providerMatch.operation.Poll(ctx, request, providerOperation)
	if err != nil {
		message := fmt.Sprintf("poll provider operation %s: %v", providerOperation.OperationID, err)
		if bootstrapDataErr != nil {
			// Most likely the cause, and the provider cannot know it.
			message = fmt.Sprintf("%s (the replacement bootstrap data could not be built: %v)", message, bootstrapDataErr)
		}

		var permanentErr *publicmachineops.PermanentError
		if errors.As(err, &permanentErr) {
			reason := valueOrDefault(permanentErr.Reason, "PermanentPollFailure")

			return r.failOperationTarget(ctx, op, machine.Name, reason, message)
		}

		return r.waitForResumableOperation(ctx, op.Name, machine.Name, "PollFailed", message, r.resumableRetryDelay(target))
	}

	switch result.State {
	case publicmachineops.ProviderOperationStateInProgress:
		delay := durationOrDefault(result.RequeueAfter, r.providerPollInterval())
		if r.providerOperationStalled(target) {
			delay = r.providerStalledPollInterval()
		}

		return r.waitForResumableOperation(ctx, op.Name, machine.Name, "InProgress", valueOrDefault(result.Message, fmt.Sprintf("provider operation %s is in progress", providerOperation.OperationID)), delay)
	case publicmachineops.ProviderOperationStateSucceeded:
		if err := r.applyOperationResult(ctx, machine, providerMatch, request, result.Result); err != nil {
			return ctrl.Result{}, err
		}

		message := valueOrDefault(result.Message, fmt.Sprintf("%s completed via %s", op.Spec.OperationKind, providerMatch.provider.Name()))
		if err := r.finishOperationTarget(ctx, op.Name, machine.Name, unboundedv1alpha3.OperationPhaseComplete, message); err != nil {
			return ctrl.Result{}, err
		}

		return r.completeOperation(ctx, op, machine.Generation, message)
	case publicmachineops.ProviderOperationStateFailed, publicmachineops.ProviderOperationStateCanceled:
		reason := valueOrDefault(result.Reason, string(result.State))
		message := valueOrDefault(result.Message, fmt.Sprintf("provider operation %s %s", providerOperation.OperationID, strings.ToLower(string(result.State))))

		return r.failOperationTarget(ctx, op, machine.Name, reason, message)
	default:
		message := fmt.Sprintf("provider operation %s returned unknown state %q", providerOperation.OperationID, result.State)

		return r.waitForResumableOperation(ctx, op.Name, machine.Name, "UnknownProviderState", message, r.providerPollInterval())
	}
}

func (r *MachineOperationReconciler) waitForResumableOperation(
	ctx context.Context,
	opName string,
	machineName string,
	reason string,
	message string,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	if err := r.updateOperationTarget(ctx, opName, machineName, func(target *unboundedv1alpha3.MachineOperationTargetStatus) {
		target.Phase = unboundedv1alpha3.OperationPhaseInProgress
		target.Message = message

		if target.ProviderOperation == nil {
			return
		}

		stalled := r.providerOperationStalled(target)
		conditionStatus := metav1.ConditionFalse
		conditionReason := "WithinExpectedDuration"
		conditionMessage := "provider operation is within its expected duration"

		if stalled {
			conditionStatus = metav1.ConditionTrue
			conditionReason = reason
			conditionMessage = message
		}

		setTargetCondition(target, metav1.Condition{
			Type:    unboundedv1alpha3.MachineOperationConditionProviderOperationStalled,
			Status:  conditionStatus,
			Reason:  conditionReason,
			Message: conditionMessage,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *MachineOperationReconciler) failOperationTarget(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machineName string,
	reason string,
	message string,
) (ctrl.Result, error) {
	if err := r.finishOperationTarget(ctx, op.Name, machineName, unboundedv1alpha3.OperationPhaseFailed, fmt.Sprintf("%s: %s", reason, message)); err != nil {
		return ctrl.Result{}, err
	}

	return r.failOperation(ctx, op, reason, message)
}

func (r *MachineOperationReconciler) finishOperationTarget(
	ctx context.Context,
	opName string,
	machineName string,
	phase unboundedv1alpha3.OperationPhase,
	message string,
) error {
	return r.updateOperationTarget(ctx, opName, machineName, func(target *unboundedv1alpha3.MachineOperationTargetStatus) {
		r.markOperationTargetFinished(target, phase, message)
	})
}

func (r *MachineOperationReconciler) markOperationTargetFinished(
	target *unboundedv1alpha3.MachineOperationTargetStatus,
	phase unboundedv1alpha3.OperationPhase,
	message string,
) {
	now := r.now()
	target.Phase = phase
	target.Message = message
	target.CompletedAt = &now
}

func (r *MachineOperationReconciler) updateOperationTarget(
	ctx context.Context,
	opName string,
	machineName string,
	mutate func(*unboundedv1alpha3.MachineOperationTargetStatus),
) error {
	return r.updateOperationStatus(ctx, opName, func(op *unboundedv1alpha3.MachineOperation) {
		for i := range op.Status.Targets {
			if op.Status.Targets[i].MachineRef != machineName {
				continue
			}

			mutate(&op.Status.Targets[i])
			op.Status.Targets[i].Message = statusmsg.TruncateCondition(op.Status.Targets[i].Message)
			op.Status.Message = op.Status.Targets[i].Message

			return
		}
	})
}

func (r *MachineOperationReconciler) updateOperationStatus(
	ctx context.Context,
	opName string,
	mutate func(*unboundedv1alpha3.MachineOperation),
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op unboundedv1alpha3.MachineOperation
		if err := r.Get(ctx, client.ObjectKey{Name: opName}, &op); err != nil {
			return err
		}

		mutate(&op)

		return r.Status().Update(ctx, &op)
	})
}

func (r *MachineOperationReconciler) waitForOlderConflictingOperation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
	providerMatch providerMatch,
) (ctrl.Result, bool, error) {
	older, err := r.olderConflictingOperation(ctx, op, machine)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	if older == "" {
		return ctrl.Result{}, false, nil
	}

	message := fmt.Sprintf("waiting for older host operation %s", older)
	err = r.updateOperationStatus(ctx, op.Name, func(latest *unboundedv1alpha3.MachineOperation) {
		unknownImmediateOutcome := providerMatch.operation.Mode() == publicmachineops.OperationModeImmediate &&
			latest.Status.Phase == unboundedv1alpha3.OperationPhaseInProgress
		if !unknownImmediateOutcome {
			latest.Status.Phase = unboundedv1alpha3.OperationPhasePending
		}

		latest.Status.Message = message
	})

	return ctrl.Result{RequeueAfter: r.providerPollInterval()}, true, err
}

func (r *MachineOperationReconciler) olderConflictingOperation(
	ctx context.Context,
	op *unboundedv1alpha3.MachineOperation,
	machine *unboundedv1alpha3.Machine,
) (string, error) {
	host, err := resolveMachineHost(machine)
	if err != nil {
		return "", err
	}

	operations, err := r.listOperationsForMachine(ctx, op.Spec.MachineRef)
	if err != nil {
		return "", err
	}

	var oldest *unboundedv1alpha3.MachineOperation

	for _, candidate := range operations {
		// A held operation is not started, so it never makes another
		// operation on the same Machine wait for it.
		if candidate.IsHeld() {
			continue
		}

		if candidate.Name == op.Name || candidate.Status.IsTerminal() || !isHostOperation(candidate.Spec.OperationKind) {
			continue
		}

		if candidate.Spec.MachineRef != op.Spec.MachineRef || !operationBefore(&candidate, op) {
			continue
		}

		if !r.providerFor(host.provider, candidate.Spec.OperationKind).supported() {
			continue
		}

		if oldest == nil || operationBefore(&candidate, oldest) {
			candidateCopy := candidate
			oldest = &candidateCopy
		}
	}

	if oldest != nil {
		return oldest.Name, nil
	}

	return "", nil
}

// listOperationsForMachine lists the MachineOperations that name one Machine.
// This runs for every host operation, and a selector operation makes one
// operation per machine, so listing them all each time would cost the square
// of the fleet. spec.machineRef is a selectable field: the API server
// returns only this machine's operations. An API server or CRD from before
// that rejects the selector; then, and from then on, everything is listed
// and filtered here.
func (r *MachineOperationReconciler) listOperationsForMachine(ctx context.Context, machineName string) ([]unboundedv1alpha3.MachineOperation, error) {
	var operations unboundedv1alpha3.MachineOperationList

	if !r.machineRefNotSelectable.Load() {
		err := r.List(ctx, &operations, client.MatchingFields{unboundedv1alpha3.MachineOperationMachineRefField: machineName})
		if err == nil {
			return operations.Items, nil
		}

		if !apierrors.IsBadRequest(err) && !isMissingFieldIndex(err) {
			return nil, fmt.Errorf("list MachineOperations for Machine %s: %w", machineName, err)
		}

		r.machineRefNotSelectable.Store(true)
		log.FromContext(ctx).Info("the API server does not support selecting MachineOperations by spec.machineRef; listing all of them for every host operation, which does not scale")
	}

	if err := r.List(ctx, &operations); err != nil {
		return nil, fmt.Errorf("list MachineOperations: %w", err)
	}

	filtered := operations.Items[:0]

	for _, operation := range operations.Items {
		if operation.Spec.MachineRef == machineName {
			filtered = append(filtered, operation)
		}
	}

	return filtered, nil
}

// isMissingFieldIndex reports the error a cache-backed or in-memory client
// returns for a field selector it has no index for. Such a client holds
// every MachineOperation already, so filtering here costs no API call.
func isMissingFieldIndex(err error) bool {
	return strings.Contains(err.Error(), "no index with name") || strings.Contains(err.Error(), "Index with name")
}

func operationBefore(a, b *unboundedv1alpha3.MachineOperation) bool {
	// An operation that was held for its turn in a rollout joins the queue
	// when it is released, so it waits for one that started in the meantime.
	aQueued, bQueued := a.QueueTime(), b.QueueTime()
	if !aQueued.Equal(&bQueued) {
		return aQueued.Before(&bQueued)
	}

	return a.Name < b.Name
}

func operationTarget(op *unboundedv1alpha3.MachineOperation, machineName string) (*unboundedv1alpha3.MachineOperationTargetStatus, bool) {
	for i := range op.Status.Targets {
		if op.Status.Targets[i].MachineRef == machineName {
			return &op.Status.Targets[i], true
		}
	}

	return nil, false
}

func providerOperationStatus(provider string, operation publicmachineops.ProviderOperation) *unboundedv1alpha3.ProviderOperationStatus {
	return &unboundedv1alpha3.ProviderOperationStatus{
		Provider:    provider,
		OperationID: operation.OperationID,
		ResumeToken: operation.ResumeToken,
	}
}

func setTargetCondition(target *unboundedv1alpha3.MachineOperationTargetStatus, condition metav1.Condition) {
	condition = normalizeCondition(condition)

	condition.ObservedGeneration = target.ObservedGeneration
	if condition.LastTransitionTime.IsZero() {
		condition.LastTransitionTime = metav1.Now()
	}

	apimeta.SetStatusCondition(&target.Conditions, condition)
}

func (r *MachineOperationReconciler) providerOperationStalled(target *unboundedv1alpha3.MachineOperationTargetStatus) bool {
	if target.ProviderOperation == nil || target.LastAttemptAt == nil {
		return false
	}

	return r.now().Sub(target.LastAttemptAt.Time) >= r.providerStallAfter()
}

// resumableRetryDelay is the wait before an accepted provider operation is
// tried again after a failure that is not the provider's verdict.
func (r *MachineOperationReconciler) resumableRetryDelay(target *unboundedv1alpha3.MachineOperationTargetStatus) time.Duration {
	if r.providerOperationStalled(target) {
		return r.providerStalledPollInterval()
	}

	return r.providerPollInterval()
}

func (r *MachineOperationReconciler) providerPollInterval() time.Duration {
	if r.ProviderPollInterval <= 0 {
		return defaultProviderPollInterval
	}

	return r.ProviderPollInterval
}

func (r *MachineOperationReconciler) providerStallAfter() time.Duration {
	if r.ProviderStallAfter <= 0 {
		return defaultProviderStallAfter
	}

	return r.ProviderStallAfter
}

func (r *MachineOperationReconciler) providerStalledPollInterval() time.Duration {
	if r.ProviderStalledPollInterval <= 0 {
		return defaultProviderStalledPollInterval
	}

	return r.ProviderStalledPollInterval
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}

	return fallback
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}

	return fallback
}
