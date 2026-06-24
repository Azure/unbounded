// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machinestatus

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const MaxConditionMessageLen = 1024

func Condition(conditionType string, status metav1.ConditionStatus, reason, message string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            TruncateMessage(message),
		ObservedGeneration: generation,
	}
}

func TruncateMessage(message string) string {
	if len(message) <= MaxConditionMessageLen {
		return message
	}

	return message[:MaxConditionMessageLen-3] + "..."
}

func SetConditionIfChanged(machine *v1alpha3.Machine, cond metav1.Condition) bool {
	existing := meta.FindStatusCondition(machine.Status.Conditions, cond.Type)
	if existing != nil && existing.Status == cond.Status && existing.Reason == cond.Reason &&
		existing.Message == cond.Message && existing.ObservedGeneration == cond.ObservedGeneration {
		return false
	}

	meta.SetStatusCondition(&machine.Status.Conditions, cond)

	return true
}

func Update(ctx context.Context, c client.Client, key client.ObjectKey, mutate func(*v1alpha3.Machine) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine v1alpha3.Machine
		if err := c.Get(ctx, key, &machine); err != nil {
			return err
		}

		if !mutate(&machine) {
			return nil
		}

		return c.Status().Update(ctx, &machine)
	})
}

func UpdateCondition(ctx context.Context, c client.Client, key client.ObjectKey, conditionType string, status metav1.ConditionStatus, reason, message string) error {
	return Update(ctx, c, key, func(machine *v1alpha3.Machine) bool {
		return SetConditionIfChanged(machine, Condition(conditionType, status, reason, message, machine.Generation))
	})
}

func Event(recorder events.EventRecorder, machine *v1alpha3.Machine, eventType, reason, message string) {
	if recorder == nil || machine == nil {
		return
	}

	recorder.Eventf(machine, nil, eventType, reason, reason, "%s", TruncateMessage(message))
}
