// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineops"
)

// Reasons for the IsolationDrift condition.
const (
	isolationReasonDrift          = "AppliedModeDiffers"
	isolationReasonInSync         = "AppliedModeMatches"
	isolationReasonNoDeclaration  = "NoModeDeclared"
	isolationReasonChangeFailed   = "ChangeFailed"
	isolationReasonChangeRefused  = "ChangeRefused"
	isolationReasonChangeProgress = "ChangeInProgress"
	isolationReasonChangeStale    = "ChangeStale"
	isolationReasonExperimental   = "ExperimentalModeDisabled"
)

// isolationChangeStaleAfter is how long a change may stay InProgress before
// the condition calls it stale. A change rebuilds the node stack, and a
// reprovision bootstraps the whole host again, so the threshold is generous; an
// InProgress older than this means the agent stopped reporting, for example
// because it died mid-change.
const isolationChangeStaleAfter = 2 * time.Hour

// syncIsolationConditions maintains the IsolationDrift condition on the
// Machine. It only mutates the in-memory status and reports whether anything
// changed, and how long until the condition must be computed again (zero when
// nothing is pending on time).
//
// spec.isolation declares the mode a node moves to at its next repave, so a
// difference from the applied mode is a declaration waiting for that repave,
// not an error. An empty spec.isolation declares nothing: a repave leaves the
// node in the mode it runs, so there is no drift to report. The condition is
// present only once the agent has reported status.isolation, and the
// controller never acts on it. status.isolationChange refines the reason while
// it is about the mode the spec declares.
//
// containerEnabled reports whether this machina enables the experimental
// container mode. When it does not, a declared container is reported as
// refused by machina, whatever the agent says.
func syncIsolationConditions(machine *unboundedv1alpha3.Machine, now time.Time, containerEnabled bool) (bool, time.Duration) {
	observed := machine.Status.Isolation
	if observed == "" {
		return apimeta.RemoveStatusCondition(&machine.Status.Conditions, unboundedv1alpha3.MachineConditionIsolationDrift), 0
	}

	declared := machine.Spec.Isolation

	cond := metav1.Condition{
		Type:   unboundedv1alpha3.MachineConditionIsolationDrift,
		Status: metav1.ConditionFalse,
	}

	var recheck time.Duration

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

		if err := checkIsolationEnabled(declared, containerEnabled); err != nil {
			cond.Reason = isolationReasonExperimental
			cond.Message = fmt.Sprintf("%v; the node keeps running %q", err, observed)

			break
		}

		if change := machine.Status.IsolationChange; change != nil && change.TargetMode == declared {
			recheck = describeIsolationChange(machine, change, &cond, now)
		}
	}

	cond.Message = machineops.TruncateUTF8Bytes(cond.Message, machineops.MaxConditionMessageBytes)

	return setConditionIfChanged(machine, cond), recheck
}

// describeIsolationChange sets the reason and message of a drift condition
// from the agent's report of a change to the declared mode. It returns how long
// until an InProgress report turns stale.
func describeIsolationChange(
	machine *unboundedv1alpha3.Machine,
	change *unboundedv1alpha3.IsolationChangeStatus,
	cond *metav1.Condition,
	now time.Time,
) time.Duration {
	declared, observed := machine.Spec.Isolation, machine.Status.Isolation

	switch change.Phase {
	case unboundedv1alpha3.IsolationChangeFailed:
		switch change.Reason {
		case unboundedv1alpha3.IsolationChangeReasonReprovisionNotAllowed:
			cond.Reason = isolationReasonChangeRefused
			cond.Message = refusedReprovisionMessage(machine, change)
		case unboundedv1alpha3.IsolationChangeReasonExperimentalNotEnabled:
			cond.Reason = isolationReasonChangeRefused
			cond.Message = fmt.Sprintf("the agent refused the change to the experimental isolation %q and the node still runs %q: %s; "+
				"the agent configuration on the host must set Experimental.ContainerIsolation, which machina writes when "+
				"experimentalContainerIsolation is enabled and the host is provisioned again",
				declared, observed, change.Message)
		case unboundedv1alpha3.IsolationChangeReasonUnsupportedMode:
			cond.Reason = isolationReasonChangeRefused
			cond.Message = fmt.Sprintf("the agent refused the change to isolation %q and the node still runs %q: %s; "+
				"run an agent version that supports the mode (spec.agent.version), and it is tried again at the next repave",
				declared, observed, change.Message)
		default:
			cond.Reason = isolationReasonChangeFailed
			cond.Message = fmt.Sprintf("the change to isolation %q failed and the node still runs %q: %s; it is tried again at the next repave",
				declared, observed, change.Message)
		}
	case unboundedv1alpha3.IsolationChangeInProgress:
		age := now.Sub(change.LastTransitionTime.Time)
		if !change.LastTransitionTime.IsZero() && age >= isolationChangeStaleAfter {
			cond.Reason = isolationReasonChangeStale
			cond.Message = fmt.Sprintf("the change from isolation %q to %q has been in progress since %s without a report from the agent; "+
				"check the agent on the host: %s",
				observed, declared, change.LastTransitionTime.UTC().Format(time.RFC3339), change.Message)

			return 0
		}

		cond.Reason = isolationReasonChangeProgress
		cond.Message = fmt.Sprintf("the node is changing from isolation %q to %q: %s", observed, declared, change.Message)

		if change.LastTransitionTime.IsZero() {
			return 0
		}

		return isolationChangeStaleAfter - age
	}

	return 0
}

// refusedReprovisionMessage explains a change refused for want of the
// allow-reprovision opt-in, and whether the opt-in has been given since.
func refusedReprovisionMessage(machine *unboundedv1alpha3.Machine, change *unboundedv1alpha3.IsolationChangeStatus) string {
	declared, observed := machine.Spec.Isolation, machine.Status.Isolation
	annotation := unboundedv1alpha3.AnnotationAllowReprovision

	if machine.Annotations[annotation] == "true" {
		return fmt.Sprintf("the change to isolation %q needs a reprovision and was refused (%s); %s=true is now set, so the next repave resets and bootstraps the host in %q",
			declared, change.Message, annotation, declared)
	}

	return fmt.Sprintf("the change to isolation %q needs a reprovision (the host is reset and bootstrapped again) and was refused, so the node still runs %q: %s; "+
		"set the annotation %s=true on the Machine to allow it at the next repave, or withdraw spec.isolation",
		declared, observed, change.Message, annotation)
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

// checkIsolationEnabled returns an error when mode is experimental and this
// machina does not enable it. Machina then neither provisions a host in that
// mode nor reports a pending change to it as on its way.
func checkIsolationEnabled(mode unboundedv1alpha3.IsolationMode, containerEnabled bool) error {
	if mode == unboundedv1alpha3.IsolationContainer && !containerEnabled {
		return fmt.Errorf("spec.isolation %q is experimental and not enabled on this machina; "+
			"set experimentalContainerIsolation: true in the machina configuration to use it", mode)
	}

	return nil
}
