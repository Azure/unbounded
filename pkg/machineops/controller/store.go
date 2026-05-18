// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// StatusStore updates MachineOperation lifecycle status.
type StatusStore struct {
	client client.Client
	now    func() metav1.Time
}

// NewStatusStore returns a MachineOperation status store.
func NewStatusStore(c client.Client, now func() metav1.Time) *StatusStore {
	if now == nil {
		now = metav1.Now
	}

	return &StatusStore{client: c, now: now}
}

// MarkInProgress records that op has started execution.
func (s *StatusStore) MarkInProgress(ctx context.Context, op OperationRequest, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := s.client.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return err
		}
		if latest.Status.IsTerminal() {
			return nil
		}

		currentTime := s.now()
		latest.Status.Phase = machinav1alpha3.OperationPhaseInProgress
		latest.Status.Message = message
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &currentTime
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            message,
			ObservedGeneration: latest.Generation,
		})

		return s.client.Status().Update(ctx, &latest)
	})
}

// Finish records a result for op.
func (s *StatusStore) Finish(ctx context.Context, op OperationRequest, result OperationResult) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest machinav1alpha3.MachineOperation
		if err := s.client.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		currentTime := s.now()
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &currentTime
		}

		latest.Status.Phase = result.Phase
		latest.Status.Message = result.Message
		latest.Status.CompletedAt = &currentTime
		if result.ObservedMachineGeneration > 0 {
			latest.Status.ObservedMachineGeneration = result.ObservedMachineGeneration
		}

		conditionStatus := metav1.ConditionTrue
		if result.Phase == machinav1alpha3.OperationPhaseFailed {
			conditionStatus = metav1.ConditionFalse
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               OperationConditionCompleted,
			Status:             conditionStatus,
			Reason:             result.Reason,
			Message:            result.Message,
			ObservedGeneration: latest.Generation,
		})

		return s.client.Status().Update(ctx, &latest)
	})
}

var _ Store = (*StatusStore)(nil)
