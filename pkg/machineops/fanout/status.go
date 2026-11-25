// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type childCounts struct {
	held           int
	active         int
	complete       int
	failed         int
	failedMachines []string
}

func countChildren(children []*unboundedv1alpha3.MachineOperation) childCounts {
	var counts childCounts

	for _, child := range children {
		switch {
		case child.Status.Phase == unboundedv1alpha3.OperationPhaseComplete:
			counts.complete++
		case child.Status.Phase == unboundedv1alpha3.OperationPhaseFailed:
			counts.failed++

			if child.Spec.MachineRef != "" {
				counts.failedMachines = append(counts.failedMachines, child.Spec.MachineRef)
			}
		case child.IsHeld():
			counts.held++
		default:
			counts.active++
		}
	}

	return counts
}

// failedSample returns the first few failed machines by name.
func (c childCounts) failedSample() []string {
	if len(c.failedMachines) == 0 {
		return nil
	}

	names := append([]string(nil), c.failedMachines...)
	sort.Strings(names)

	return names[:min(len(names), maxFailedMachines)]
}

func summaryMessage(total, held, active, complete, failed int) string {
	return fmt.Sprintf("machines: %d total, %d held, %d active, %d complete, %d failed",
		total, held, active, complete, failed)
}

func targetsResolved(operation *unboundedv1alpha3.MachineOperation) bool {
	condition := apimeta.FindStatusCondition(
		operation.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionTargetsResolved,
	)

	return condition != nil && condition.Status == metav1.ConditionTrue
}

func resolvedTotal(operation *unboundedv1alpha3.MachineOperation, listed int) int {
	if operation.Status.Summary != nil {
		return int(operation.Status.Summary.Total)
	}

	return listed
}

// childReleaseTime is when this controller let a child run, from the
// annotation it sets at that moment. A child without it was not released by
// this controller.
func childReleaseTime(child *unboundedv1alpha3.MachineOperation) (time.Time, bool) {
	value := child.Annotations[unboundedv1alpha3.MachineOperationReleasedAtAnnotation]
	if value == "" {
		return time.Time{}, false
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}

	return parsed, true
}

func setCompletedCondition(
	operation *unboundedv1alpha3.MachineOperation,
	status metav1.ConditionStatus,
	reason string,
	message string,
	now metav1.Time,
) {
	apimeta.SetStatusCondition(&operation.Status.Conditions, metav1.Condition{
		Type:               unboundedv1alpha3.MachineOperationConditionCompleted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: operation.Generation,
		LastTransitionTime: now,
	})
}

func statusEmpty(status unboundedv1alpha3.MachineOperationStatus) bool {
	return status.Phase == "" &&
		status.Message == "" &&
		status.StartedAt == nil &&
		status.CompletedAt == nil &&
		status.ObservedMachineGeneration == 0 &&
		len(status.Targets) == 0 &&
		status.Allocation == nil &&
		len(status.Conditions) == 0
}

func statusEqual(left, right unboundedv1alpha3.MachineOperationStatus) bool {
	return equality.Semantic.DeepEqual(left, right)
}
