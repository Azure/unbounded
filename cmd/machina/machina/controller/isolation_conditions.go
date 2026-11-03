// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// Reasons for the IsolationDrift condition.
const (
	isolationReasonDrift          = "AppliedModeDiffers"
	isolationReasonInSync         = "AppliedModeMatches"
	isolationReasonNoDeclaration  = "NoModeDeclared"
	isolationReasonChangeFailed   = "ChangeFailed"
	isolationReasonChangeProgress = "ChangeInProgress"
)

// syncIsolationConditions maintains the IsolationDrift condition (design 6.5)
// on the Machine. It only mutates the in-memory status and reports whether
// anything changed.
//
// spec.isolation declares the mode a node moves to at its next repave, so a
// difference from the applied mode is a declaration waiting for that repave,
// not an error. An empty spec.isolation declares nothing: a repave leaves the
// node in the mode it runs, so there is no drift to report. The condition is
// present only once the agent has reported status.isolation, and the
// controller never acts on it. status.isolationChange refines the reason while
// it is about the mode the spec declares.
func syncIsolationConditions(machine *unboundedv1alpha3.Machine) bool {
	changed := false

	observed := machine.Status.Isolation
	if observed == "" {
		if apimeta.RemoveStatusCondition(&machine.Status.Conditions, unboundedv1alpha3.MachineConditionIsolationDrift) {
			changed = true
		}

		return changed
	}

	declared := machine.Spec.Isolation

	cond := metav1.Condition{
		Type:   unboundedv1alpha3.MachineConditionIsolationDrift,
		Status: metav1.ConditionFalse,
	}

	switch declared {
	case "":
		cond.Reason = isolationReasonNoDeclaration
		cond.Message = fmt.Sprintf("spec.isolation is not set; a repave leaves the node in its applied isolation %q", observed)
	case observed:
		cond.Reason = isolationReasonInSync
		cond.Message = fmt.Sprintf("spec.isolation %q matches applied isolation %q", declared, observed)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = isolationReasonDrift
		cond.Message = fmt.Sprintf("spec.isolation is %q but the applied isolation is %q; the node changes at its next repave", declared, observed)

		if change := machine.Status.IsolationChange; change != nil && change.TargetMode == declared {
			switch change.Phase {
			case unboundedv1alpha3.IsolationChangeFailed:
				cond.Reason = isolationReasonChangeFailed
				cond.Message = fmt.Sprintf("the change to isolation %q failed and the node still runs %q: %s; it is tried again at the next repave",
					declared, observed, change.Message)
			case unboundedv1alpha3.IsolationChangeInProgress:
				cond.Reason = isolationReasonChangeProgress
				cond.Message = fmt.Sprintf("the node is changing from isolation %q to %q: %s", observed, declared, change.Message)
			}
		}
	}

	return setConditionIfChanged(machine, cond) || changed
}

// setConditionIfChanged sets cond and reports whether the stored condition
// differs (status, reason or message) from what was there before.
func setConditionIfChanged(machine *unboundedv1alpha3.Machine, cond metav1.Condition) bool {
	cond.ObservedGeneration = machine.Generation

	prev := apimeta.FindStatusCondition(machine.Status.Conditions, cond.Type)
	if prev != nil && prev.Status == cond.Status && prev.Reason == cond.Reason &&
		prev.Message == cond.Message && prev.ObservedGeneration == cond.ObservedGeneration {
		return false
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, cond)

	return true
}
