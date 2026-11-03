// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// A mode change is declared in the Machine's spec.isolation and made at the
// node's next repave, the one the agent performs when its Node is deleted. The
// outcome is reported in status.isolationChange. The agent never writes spec.

// isolationChangeStrategy maps the engine's strategy to the status enum.
func isolationChangeStrategy(s migrate.Strategy) v1alpha3.IsolationChangeStrategy {
	switch s {
	case migrate.InPlace:
		return v1alpha3.IsolationChangeInPlace
	case migrate.Reprovision:
		return v1alpha3.IsolationChangeReprovision
	default:
		return ""
	}
}

// isolationChangeFromJournal turns a journal and its resolved outcome into the
// status to report. A nil outcome is success, ErrMigrationHandedOff means a
// reprovision is still running without this daemon, anything else is a failure
// whose text says whether the node was put back.
func isolationChangeFromJournal(j migrate.Journal, outcome error) v1alpha3.IsolationChangeStatus {
	change := v1alpha3.IsolationChangeStatus{
		TargetMode: v1alpha3.IsolationMode(j.To),
		Strategy:   isolationChangeStrategy(j.Strategy),
	}

	switch {
	case outcome == nil:
		change.Phase = v1alpha3.IsolationChangeSucceeded
		change.Message = fmt.Sprintf("the node now runs in isolation mode %s (%s)", j.To, j.Strategy)
	case errors.Is(outcome, ErrMigrationHandedOff):
		change.Phase = v1alpha3.IsolationChangeInProgress
		change.Message = fmt.Sprintf("the host resets and bootstraps in isolation mode %s", j.To)
	default:
		change.Phase = v1alpha3.IsolationChangeFailed
		change.Message = outcome.Error()
	}

	return change
}

// reportIsolationChange records change in status.isolationChange. It patches
// that field only, with an optimistic lock, so it cannot overwrite the rest of
// the status. LastTransitionTime moves only when the phase or target changes.
func reportIsolationChange(ctx context.Context, c client.Client, machineName string, change v1alpha3.IsolationChangeStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine v1alpha3.Machine
		if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			return err
		}

		next := change

		prev := machine.Status.IsolationChange
		if prev != nil && prev.Phase == next.Phase && prev.TargetMode == next.TargetMode && !prev.LastTransitionTime.IsZero() {
			next.LastTransitionTime = prev.LastTransitionTime
		} else {
			next.LastTransitionTime = metav1.Now()
		}

		if prev != nil && apiequality.Semantic.DeepEqual(*prev, next) {
			return nil
		}

		before := machine.DeepCopy()
		machine.Status.IsolationChange = &next

		return c.Status().Patch(ctx, &machine, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

// settleIsolationChanges resolves the mode changes an earlier daemon, or an
// earlier reconcile, left unfinished: it resumes or rolls back each journal on
// disk (see resolveJournal), reports the outcome in status.isolationChange and
// removes the journal once the report is made. It reports whether there was
// anything to settle, which includes a journal it could not read: nothing new
// starts over a change that may be under way. A reprovision that is still
// running stays in progress. What cannot be reported now is tried again at the next start. The caller
// holds installation ownership.
func settleIsolationChanges(ctx context.Context, log *slog.Logger, c client.Client, machineName string, operator nodeOperator) bool {
	var (
		pending []migrate.Journal
		blocked bool
	)

	for _, js := range []migrate.JournalStore{inPlaceJournalStore(), reprovisionJournalFor()} {
		j, err := js.Load()

		switch {
		case errors.Is(err, migrate.ErrNoJournal):
		case err != nil:
			// A change may be under way. Nothing new starts over it.
			log.Error("an isolation change journal is unreadable; the node may be mid-change and needs attention", "error", err)

			blocked = true
		default:
			pending = append(pending, j)
		}
	}

	for _, j := range pending {
		outcome := resolveJournal(ctx, log, j)
		change := isolationChangeFromJournal(j, outcome)

		if err := reportIsolationChange(ctx, c, machineName, change); err != nil {
			log.Warn("could not report the isolation change outcome; trying again at the next start", "to", j.To, "error", err)

			continue
		}

		if errors.Is(outcome, ErrMigrationHandedOff) {
			log.Info("isolation reprovision is still in progress", "to", j.To)

			continue
		}

		log.Info("isolation change resolved", "to", j.To, "outcome", outcomeText(outcome))
		// Only this journal: another may be pending, and its report can still fail.
		clearJournals(log, j)

		afterIsolationChange(ctx, log, c, machineName, operator, outcome == nil)
	}

	return blocked || len(pending) > 0
}

// afterIsolationChange brings the Machine up to date with what the host now
// runs: the observed mode, and after a success the applied configuration
// version. Failures are logged; both are reported again when the daemon next
// starts or repaves.
func afterIsolationChange(ctx context.Context, log *slog.Logger, c client.Client, machineName string, operator nodeOperator, succeeded bool) {
	active, err := operator.FindActiveMachine(log)
	if err != nil {
		log.Warn("could not read the applied config after an isolation change", "error", err)

		return
	}

	if err := reportObservedIsolation(ctx, c, active.Config); err != nil {
		log.Warn("could not report observed isolation", "error", err)
	}

	if !succeeded {
		return
	}

	_, appliedRef, err := resolveDesiredRepaveConfig(ctx, c, machineName, active.Config)
	if err != nil {
		log.Warn("could not resolve the desired configuration after an isolation change", "error", err)

		return
	}

	if err := markAppliedConfiguration(ctx, c, machineName, appliedRef); err != nil {
		log.Warn("could not mark the desired configuration applied", "error", err)
	}
}

// changeIsolation moves the node to the mode the Machine declares, as part of
// the repave. The target is built from the desired configuration, so a new
// Kubernetes version or labels in the same repave are applied with it. The
// strategy follows the pair of modes: in place when both are nspawn or
// container, a reprovision otherwise.
//
// A failed change is recorded and not retried: it returns success with no
// requeue, so a node is not put through repeated outages. The next repave tries
// again.
func (r *repaveReconciler) changeIsolation(
	ctx context.Context,
	active *ActiveMachine,
	desired *provision.UnboundedAgentConfig,
	appliedRef *v1alpha3.MachineConfigurationRefStatus,
) (reconcile.Result, error) {
	target, err := desired.IsolationMode()
	if err != nil {
		// The CRD enum keeps an unknown mode out of spec.isolation, and the
		// status cannot name one either.
		r.log.Error("the declared isolation mode is not known; leaving the node as it is", "error", err)

		return reconcile.Result{}, nil
	}

	change := v1alpha3.IsolationChangeStatus{TargetMode: v1alpha3.IsolationMode(target)}

	strategy, err := migrate.Plan(activeModeOf(active), target)
	if err != nil {
		return r.recordFailedIsolationChange(ctx, active, change, err), nil
	}

	change.Strategy = isolationChangeStrategy(strategy)
	change.Phase = v1alpha3.IsolationChangeInProgress
	change.Message = fmt.Sprintf("changing the node from isolation mode %s to %s (%s)", activeModeOf(active), target, strategy)

	if err := reportIsolationChange(ctx, r.Client, r.machineName, change); err != nil {
		r.log.Warn("could not report the start of the isolation change", "error", err)
	}

	r.log.Info("changing isolation mode at repave", "from", activeModeOf(active), "to", target, "strategy", strategy)

	err = r.nodeOperator.MigrateIsolation(ctx, r.log, active, desired, strategy)

	switch {
	case errors.Is(err, ErrMigrationHandedOff):
		// The host resets and bootstraps without this daemon. The daemon that
		// starts afterward reports the outcome.
		return reconcile.Result{}, nil
	case err != nil:
		return r.recordFailedIsolationChange(ctx, active, change, err), nil
	}

	change.Phase = v1alpha3.IsolationChangeSucceeded
	change.Message = fmt.Sprintf("the node now runs in isolation mode %s (%s)", target, strategy)

	if err := reportIsolationChange(ctx, r.Client, r.machineName, change); err != nil {
		return reconcile.Result{}, fmt.Errorf("report the isolation change: %w", err)
	}

	clearReportedJournals(r.log)

	if err := reportObservedIsolation(ctx, r.Client, &desired.AgentConfig); err != nil {
		r.log.Warn("could not report observed isolation", "error", err)
	}

	if err := markAppliedConfiguration(ctx, r.Client, r.machineName, appliedRef); err != nil {
		return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
	}

	return reconcile.Result{}, nil
}

// recordFailedIsolationChange records the failure and returns the result of the
// reconcile: always success with no requeue. If the failure cannot be recorded
// it is only logged, and the terminal journal stays so the next start reports
// it; retrying here would put the node through another outage.
func (r *repaveReconciler) recordFailedIsolationChange(
	ctx context.Context,
	active *ActiveMachine,
	change v1alpha3.IsolationChangeStatus,
	cause error,
) reconcile.Result {
	r.log.Error("the isolation change failed; it is tried again at the next repave", "to", change.TargetMode, "error", cause)

	change.Phase = v1alpha3.IsolationChangeFailed
	change.Message = cause.Error()

	if err := reportIsolationChange(ctx, r.Client, r.machineName, change); err != nil {
		r.log.Warn("could not record the failed isolation change", "error", err)

		return reconcile.Result{}
	}

	clearReportedJournals(r.log)

	r.restoreNodeRegistration(ctx, active)

	return reconcile.Result{}
}

var (
	// nodeReturnWait is how long a failed change gives the Node object to come
	// back before the node stack is restarted. A rolled-back stack restarts on
	// its own and its kubelet registers the Node within this time.
	nodeReturnWait = 45 * time.Second
	nodeReturnPoll = 5 * time.Second
)

// restoreNodeRegistration makes sure the Node object the operator deleted for
// this repave exists again after a failed change. When the change was refused
// before anything was touched, the old stack never restarted, and a running
// kubelet does not create a deleted Node object by itself, so the node would
// stay out of the cluster. Restarting the stack makes the kubelet register it.
func (r *repaveReconciler) restoreNodeRegistration(ctx context.Context, active *ActiveMachine) {
	deadline := time.Now().Add(nodeReturnWait)

	for {
		var node corev1.Node

		err := r.Get(ctx, client.ObjectKey{Name: r.nodeName}, &node)
		if err == nil {
			return
		}

		if !apierrors.IsNotFound(err) {
			r.log.Warn("could not check whether the Node came back after the failed isolation change", "error", err)

			return
		}

		if !time.Now().Before(deadline) {
			break
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(nodeReturnPoll):
		}
	}

	r.log.Info("the Node did not come back after the failed isolation change; restarting the node stack so the kubelet registers it")

	if err := r.nodeOperator.RestartNode(ctx, r.log, active); err != nil {
		r.log.Warn("could not restart the node stack after the failed isolation change; restart it so the Node registers", "error", err)
	}
}
