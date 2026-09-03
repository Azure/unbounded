// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package component defines the extension contract for the unbounded operator.
// A component is a unit of desired state the operator reconciles for Sites.
// Implementations satisfy either ClusterComponent (cluster-wide singletons such
// as net and machina) or SiteComponent (per-Site units such as metalman and
// storage) and are assembled into a Registry the SiteReconciler drives. Adding a
// component is a matter of implementing one interface and registering it; the
// reconcile loop, status conditions, ordering, and the Site-less pass are all
// driven from the registry.
package component

import "time"

// Reason strings published on Site.status.conditions. They must be CamelCase
// alphanumeric to satisfy metav1.Condition validation.
const (
	ReasonReconciled     = "Reconciled"
	ReasonDisabled       = "Disabled"
	ReasonNoSites        = "NoSites"
	ReasonReconcileError = "ReconcileError"

	// ReasonBackendNotReady reports that a component withheld objects which
	// direct apiserver traffic at a backend of its own, because that backend is
	// not serving.
	//
	// It covers both ways of arriving there. Usually the backend is known to be
	// down, the component is otherwise reconciled, and what is missing is state
	// it does not control and can only wait for. It is also used when the
	// backend's state could not be read at all: the component still emits
	// everything the answer does not govern, but sets Err, so the pass is a
	// reconcile error and goes through backoff. See NotReadyErr.
	ReasonBackendNotReady = "BackendNotReady"

	// ReasonDependencyNotWritten marks a component whose operations were never
	// attempted because something they depend on failed. It is deliberately
	// distinct from ReconcileError: the component itself did not fail, and the
	// component that did already reports the underlying error.
	ReasonDependencyNotWritten = "DependencyNotWritten"

	// ReasonOverrideNotApplied marks a component whose workload was withheld
	// because the overrides that would have shaped it could not be used. The
	// running workload is untouched, which is the point, but the component is
	// not reconciled: it did not write what it planned.
	ReasonOverrideNotApplied = "OverrideNotApplied"

	// ReasonPlanRejected marks every component in a pass the executor refused
	// to run at all, for a dependency cycle or contradictory shared
	// operations. Nothing was written, so no component's planning verdict
	// describes the cluster.
	ReasonPlanRejected = "PlanRejected"
)

// Result is the outcome of reconciling a single component for one pass. The
// SiteReconciler translates it into a Site status condition
// (Ready -> ConditionTrue/False, Reason, Message) and aggregates Err into the
// reconcile error so a failing component requeues the Site.
//
// A not-ready Result without an Err does not by itself trigger a retry: the
// driver requeues on Err (with backoff) or after the smallest positive
// RequeueAfter across components. A component that is waiting on state it does
// not watch should set RequeueAfter (see NotReadyAfter) so it is re-checked;
// otherwise it only re-runs when one of its watches fires.
type Result struct {
	Ready   bool
	Reason  string
	Message string
	Err     error

	// RequeueAfter, when positive, asks the driver to re-run the Site after the
	// given duration even without an error or a watch event. The driver uses the
	// smallest positive RequeueAfter across all components in a pass.
	RequeueAfter time.Duration
}

// Reconciled reports a successfully reconciled component.
func Reconciled() Result {
	return Result{Ready: true, Reason: ReasonReconciled, Message: "component reconciled"}
}

// ReconciledAfter reports a successfully reconciled component and asks the
// driver to check it again after the given duration.
//
// It takes a message rather than reusing Reconciled's, because a component that
// wants to be re-checked is waiting on something, and the condition is the only
// place that can say what. "component reconciled" on a Site that is quietly
// polling tells a reader nothing about why.
func ReconciledAfter(message string, after time.Duration) Result {
	return Result{
		Ready:        true,
		Reason:       ReasonReconciled,
		Message:      message,
		RequeueAfter: after,
	}
}

// Disabled reports a component that is intentionally not running.
func Disabled(message string) Result {
	return Result{Ready: true, Reason: ReasonDisabled, Message: message}
}

// NoSites reports a cluster component retained while no Sites exist.
func NoSites(message string) Result {
	return Result{Ready: true, Reason: ReasonNoSites, Message: message}
}

// NotReady reports a component that is not yet ready for a caller-supplied
// reason without treating it as a reconcile error. It does not schedule a retry
// on its own; use NotReadyAfter, set Err, or rely on a watch to re-trigger.
func NotReady(reason, message string) Result {
	return Result{Ready: false, Reason: reason, Message: message}
}

// NotReadyAfter reports a not-ready component and asks the driver to re-check the
// Site after the given duration, for readiness that depends on state the
// component does not watch (for example polling an external resource).
func NotReadyAfter(reason, message string, after time.Duration) Result {
	return Result{Ready: false, Reason: reason, Message: message, RequeueAfter: after}
}

// NotReadyErr reports a component that could not establish readiness because a
// read failed, under a caller-supplied reason rather than ReasonReconcileError.
//
// It exists for the case where a failed read is not a reason to stop
// reconciling. A component that cannot answer one question about its own
// readiness still knows what the rest of its manifests should look like, so it
// emits them and withholds only what the unanswered question governs. The error
// is still surfaced, both in the condition message and as the aggregated
// reconcile error, so the pass goes through error backoff and the failure is not
// mistaken for an orderly wait.
func NotReadyErr(reason string, err error, after time.Duration) Result {
	return Result{
		Ready:        false,
		Reason:       reason,
		Message:      err.Error(),
		Err:          err,
		RequeueAfter: after,
	}
}

// Failed reports a component reconcile error. The error is surfaced both as the
// condition message and as the aggregated reconcile error.
func Failed(err error) Result {
	return Result{Ready: false, Reason: ReasonReconcileError, Message: err.Error(), Err: err}
}
