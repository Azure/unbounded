// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// SessionStatusRecorder durably records a server-observed milestone on the
// exact immutable session and its exact MachineOperation target.
type SessionStatusRecorder struct {
	Client client.Client
	Now    func() metav1.Time
}

func (r *SessionStatusRecorder) RecordCondition(ctx context.Context, sessionName string, sessionUID types.UID, condition metav1.Condition) error {
	if r == nil || r.Client == nil || sessionName == "" || sessionUID == "" {
		return fmt.Errorf("session status recorder is not configured")
	}

	var session v1alpha3.NetbootSession
	if err := r.Client.Get(ctx, client.ObjectKey{Name: sessionName}, &session); err != nil {
		return fmt.Errorf("get NetbootSession %s: %w", sessionName, err)
	}
	if session.UID != sessionUID {
		return fmt.Errorf("NetbootSession %s identity changed", sessionName)
	}

	condition.ObservedGeneration = session.Generation
	condition.LastTransitionTime = r.now()
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.NetbootSession
		if err := r.Client.Get(ctx, client.ObjectKey{Name: sessionName}, &latest); err != nil {
			return err
		}
		if latest.UID != sessionUID {
			return fmt.Errorf("NetbootSession %s identity changed", sessionName)
		}
		if apimeta.IsStatusConditionTrue(latest.Status.Conditions, condition.Type) {
			return nil
		}
		apimeta.SetStatusCondition(&latest.Status.Conditions, condition)
		if latest.Status.Phase == v1alpha3.NetbootSessionPhaseReady {
			latest.Status.Phase = v1alpha3.NetbootSessionPhaseActive
		}

		return r.Client.Status().Update(ctx, &latest)
	}); err != nil {
		return fmt.Errorf("update NetbootSession %s status: %w", sessionName, err)
	}

	return r.recordTargetCondition(ctx, &session, condition)
}

func (r *SessionStatusRecorder) recordTargetCondition(ctx context.Context, session *v1alpha3.NetbootSession, condition metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var operation v1alpha3.MachineOperation
		if err := r.Client.Get(ctx, client.ObjectKey{Name: session.Spec.Operation.Name}, &operation); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if operation.UID != session.Spec.Operation.UID {
			return fmt.Errorf("MachineOperation %s identity changed", operation.Name)
		}

		for i := range operation.Status.Targets {
			target := &operation.Status.Targets[i]
			if target.Input == nil || target.Input.NetbootSessionRef == nil || target.Input.NetbootSessionRef.Name != session.Name || target.Input.NetbootSessionRef.UID != session.UID {
				continue
			}
			if apimeta.IsStatusConditionTrue(target.Conditions, condition.Type) {
				return nil
			}
			condition.ObservedGeneration = target.ObservedGeneration
			apimeta.SetStatusCondition(&target.Conditions, condition)

			return r.Client.Status().Update(ctx, &operation)
		}

		return fmt.Errorf("MachineOperation %s has no target for NetbootSession %s", operation.Name, session.Name)
	})
}

func (r *SessionStatusRecorder) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}
