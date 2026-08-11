// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

	// ReasonDependencyNotWritten marks a component whose operations were never
	// attempted because something they depend on failed. It is deliberately
	// distinct from ReconcileError: the component itself did not fail, and the
	// component that did already reports the underlying error.
	ReasonDependencyNotWritten = "DependencyNotWritten"
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

// Failed reports a component reconcile error. The error is surfaced both as the
// condition message and as the aggregated reconcile error.
func Failed(err error) Result {
	return Result{Ready: false, Reason: ReasonReconcileError, Message: err.Error(), Err: err}
}
