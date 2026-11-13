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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// A mode change is declared in the Machine's spec.isolation and made at the
// node's next repave, the one the agent performs when its Node is deleted. The
// outcome is reported in status.isolationChange, always through
// patchIsolationStatus. The agent never writes the spec of an existing
// Machine.

// settleRequeueDelay is how soon the repave that a settle interrupted runs
// again. The settle only put the node in a known state; the Node deletion that
// triggered the reconcile still asks for a repave.
const settleRequeueDelay = 2 * time.Second

// blockedSettleRequeueDelay is how soon a repave that a blocked settle held
// back tries again. A repave runs only on a Node delete event, and a running
// kubelet does not re-create a deleted Node, so a blocked settle that ended the
// reconcile would leave the node unregistered for good. Retrying keeps the
// Node deletion's request for a repave: once the journal is repaired or
// removed, or the cleanup that kept the change open succeeds, the repave goes
// ahead.
const blockedSettleRequeueDelay = time.Minute

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

// isolationStatusFromJournal turns a journal and its resolved outcome into the
// status to report. A nil outcome is success, and the applied mode is then the
// target, in the same patch. ErrMigrationHandedOff means a reprovision is still
// running without this daemon. Anything else is a failure whose text says
// whether the node was put back; after a rollback the applied mode is the
// source, and after a failed rollback it is left as it is.
func isolationStatusFromJournal(j migrate.Journal, outcome error) isolationStatus {
	change := &v1alpha3.IsolationChangeStatus{
		TargetMode:         v1alpha3.IsolationMode(j.To),
		Strategy:           isolationChangeStrategy(j.Strategy),
		LastTransitionTime: metav1.Now(),
	}

	s := isolationStatus{Change: change}

	switch {
	case outcome == nil:
		change.Phase = v1alpha3.IsolationChangeSucceeded
		change.Message = fmt.Sprintf("the node now runs in isolation mode %s (%s)", j.To, j.Strategy)
		s.Applied = v1alpha3.IsolationMode(j.To)
	case errors.Is(outcome, ErrMigrationHandedOff):
		change.Phase = v1alpha3.IsolationChangeInProgress
		change.Message = fmt.Sprintf("the host resets and bootstraps in isolation mode %s", j.To)
	default:
		change.Phase = v1alpha3.IsolationChangeFailed
		change.Message = outcome.Error()

		if j.Step == migrate.StepRolledBack {
			s.Applied = v1alpha3.IsolationMode(j.From)
		}
	}

	return s
}

// settleOutcome is what settling the journals found.
type settleOutcome struct {
	// resolved is true when a journal was resolved to a terminal outcome.
	resolved bool
	// blocked is true when a journal is unreadable or not over: nothing new
	// may start over it.
	blocked bool
	// handedOff is true when a reprovision is running without this daemon.
	handedOff bool
	// failedTarget is the target mode of a change that was resolved as a
	// failure, so the repave that follows does not try it again at once.
	failedTarget isolation.Mode
}

// settledJournal is one journal whose host state was resolved and whose
// outcome is still to be reported.
type settledJournal struct {
	store   migrate.JournalStore
	journal migrate.Journal
	outcome error
	// unreadable is the error reading the journal, when it could not be read.
	unreadable error
}

// resolveJournals brings the host to a known state for every journal on disk
// (see resolveJournal), without reporting anything: it needs no API server and
// no credentials, so the daemon runs it at start before it builds a client. A
// journal that resolveJournal moved is read again, so the result says whether
// the change is now over. The caller holds installation ownership.
func (e *migrationEnv) resolveJournals(ctx context.Context, log *slog.Logger) []settledJournal {
	var out []settledJournal

	for _, store := range e.journals() {
		j, err := store.Load()

		switch {
		case errors.Is(err, migrate.ErrNoJournal):
			continue
		case err != nil:
			log.Error("an isolation change journal is unreadable; the node may be mid-change and needs attention", "path", store.Path, "error", err)
			out = append(out, settledJournal{store: store, unreadable: err})

			continue
		}

		outcome := e.resolveJournal(ctx, log, j)

		// resolveJournal may have moved the journal (to a terminal step, or
		// removed it with a cleanup). What is on disk now is what counts.
		if after, err := store.Load(); err == nil {
			j = after
		} else if !errors.Is(err, migrate.ErrNoJournal) {
			out = append(out, settledJournal{store: store, unreadable: err})

			continue
		}

		out = append(out, settledJournal{store: store, journal: j, outcome: outcome})
	}

	return out
}

// reportSettled reports each settled journal in status.isolationChange and
// removes a journal once its terminal outcome is reported. A journal that is
// not terminal is kept, whatever was reported: the change it records is not
// over, and the next start or repave resolves it again. The caller holds
// installation ownership.
func (e *migrationEnv) reportSettled(ctx context.Context, log *slog.Logger, c client.Client, machineName string, settled []settledJournal) settleOutcome {
	var res settleOutcome

	for _, s := range settled {
		if s.unreadable != nil {
			res.blocked = true

			e.reportUnreadableJournal(ctx, log, c, machineName, s)

			continue
		}

		j := s.journal
		status := isolationStatusFromJournal(j, s.outcome)

		reportErr := patchIsolationStatus(ctx, c, machineName, status)
		if reportErr != nil {
			log.Warn("could not report the isolation change outcome; trying again later", "to", j.To, "error", reportErr)
		}

		switch {
		case errors.Is(s.outcome, ErrMigrationHandedOff):
			log.Info("isolation reprovision is still in progress", "to", j.To)

			res.handedOff = true
		case !j.Terminal():
			log.Warn("the isolation change is not over; its journal is kept and resolved again at the next start or repave",
				"to", j.To, "step", j.Step, "outcome", outcomeText(s.outcome))

			res.blocked = true
		default:
			log.Info("isolation change resolved", "to", j.To, "outcome", outcomeText(s.outcome))

			res.resolved = true

			if s.outcome != nil {
				res.failedTarget = j.To
			}

			// Only this journal, and only once its outcome is reported: a crash
			// or a failed report leaves it to be reported again.
			if reportErr == nil {
				e.clearJournal(log, j)
			}
		}
	}

	return res
}

// reportUnreadableJournal records that a journal cannot be read, naming the
// file, so an operator sees why the node takes no new change.
func (e *migrationEnv) reportUnreadableJournal(ctx context.Context, log *slog.Logger, c client.Client, machineName string, s settledJournal) {
	err := patchIsolationStatus(ctx, c, machineName, isolationStatus{Change: &v1alpha3.IsolationChangeStatus{
		TargetMode: unknownTargetMode,
		Phase:      v1alpha3.IsolationChangeFailed,
		Message: fmt.Sprintf("the isolation change journal %s cannot be read (%v); the node may be mid-change and takes no new change until it is repaired or removed",
			s.store.Path, s.unreadable),
		LastTransitionTime: metav1.Now(),
	}})
	if err != nil {
		log.Warn("could not report the unreadable isolation change journal", "path", s.store.Path, "error", err)
	}
}

// unknownTargetMode is the target reported for a change whose journal cannot
// be read. targetMode is a free string, so it can say so.
const unknownTargetMode v1alpha3.IsolationMode = "unknown"

// settleIsolationChanges resolves the mode changes an earlier daemon, or an
// earlier reconcile, left unfinished, and reports them. The caller holds
// installation ownership.
func (e *migrationEnv) settleIsolationChanges(ctx context.Context, log *slog.Logger, c client.Client, machineName string) settleOutcome {
	return e.reportSettled(ctx, log, c, machineName, e.resolveJournals(ctx, log))
}

// settleBeforeRepave settles earlier changes at the start of a repave and says
// how the reconcile goes on: done is true when it must stop here with result.
// A settle that resolved something requeues the repave rather than consuming
// it, since the Node deletion still asks for one. A blocked settle requeues the
// repave after blockedSettleRequeueDelay, and starts nothing over the change it
// could not settle. It does not restart the node stack to bring the Node back
// either: the host may be mid-change, and the settle says why nothing
// changed. A reprovision in progress ends the reconcile: the daemon it
// installs takes over.
func (r *repaveReconciler) settleBeforeRepave(ctx context.Context) (reconcile.Result, bool) {
	res := r.migration().settleIsolationChanges(ctx, r.log, r.Client, r.machineName)

	switch {
	case res.handedOff:
		r.markHandedOff()

		return reconcile.Result{}, true
	case res.blocked:
		r.log.Info("repave deferred: an earlier isolation change is not settled", "retry_after", blockedSettleRequeueDelay)

		return reconcile.Result{RequeueAfter: blockedSettleRequeueDelay}, true
	case res.resolved:
		if res.failedTarget != "" {
			// The change was tried and failed. The repave this settle
			// interrupted goes ahead in the current mode; the next one tries
			// the change again.
			r.skipChangeTo = res.failedTarget
		}

		return reconcile.Result{RequeueAfter: settleRequeueDelay}, true
	default:
		return reconcile.Result{}, false
	}
}

// allowsReprovision reports whether the Machine carries the opt-in a
// reprovision needs.
func allowsReprovision(machine *v1alpha3.Machine) bool {
	return machine.Annotations[v1alpha3.AnnotationAllowReprovision] == "true"
}

// changeIsolation moves the node to the mode the Machine declares, as part of
// the repave. The target is built from the desired configuration, so a new
// Kubernetes version or labels in the same repave are applied with it.
//
// A failed change is recorded and not retried: it returns success with no
// requeue, so a node is not put through repeated outages. The next repave
// tries again.
func (r *repaveReconciler) changeIsolation(
	ctx context.Context,
	active *ActiveMachine,
	desired *provision.UnboundedAgentConfig,
	strategy migrate.Strategy,
	appliedRef *v1alpha3.MachineConfigurationRefStatus,
) (reconcile.Result, error) {
	target := effectiveIsolation(&desired.AgentConfig)
	from := activeModeOf(active)

	change := v1alpha3.IsolationChangeStatus{
		TargetMode:         v1alpha3.IsolationMode(target),
		Strategy:           isolationChangeStrategy(strategy),
		Phase:              v1alpha3.IsolationChangeInProgress,
		Message:            fmt.Sprintf("changing the node from isolation mode %s to %s (%s)", from, target, strategy),
		LastTransitionTime: metav1.Now(),
	}

	if err := patchIsolationStatus(ctx, r.Client, r.machineName, isolationStatus{Change: &change}); err != nil {
		r.log.Warn("could not report the start of the isolation change", "error", err)
	}

	r.log.Info("changing isolation mode at repave", "from", from, "to", target, "strategy", strategy)

	err := r.migrator().MigrateIsolation(ctx, r.log, active, desired, strategy)

	switch {
	case errors.Is(err, ErrMigrationHandedOff):
		// The host resets and bootstraps without this daemon. The daemon that
		// starts afterward reports the outcome.
		r.markHandedOff()

		return reconcile.Result{}, nil
	case err != nil:
		return r.recordFailedIsolationChange(ctx, active, change, err), nil
	}

	change.Phase = v1alpha3.IsolationChangeSucceeded
	change.Message = fmt.Sprintf("the node now runs in isolation mode %s (%s)", target, strategy)
	change.LastTransitionTime = metav1.Now()

	if err := patchIsolationStatus(ctx, r.Client, r.machineName, isolationStatus{
		Applied: v1alpha3.IsolationMode(target),
		Change:  &change,
	}); err != nil {
		// The terminal journal stays, so the next start or repave reports it.
		return reconcile.Result{}, fmt.Errorf("report the isolation change: %w", err)
	}

	r.migration().clearTerminalJournals(r.log)

	if err := markAppliedConfiguration(ctx, r.Client, r.machineName, appliedRef); err != nil {
		return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
	}

	return reconcile.Result{}, nil
}

// recordFailedIsolationChange records the failure and returns the result of the
// reconcile: always success with no requeue. If the failure cannot be recorded
// it is only logged, and the terminal journal stays so the next start reports
// it; retrying here would put the node through another outage. Either way the
// node's registration is restored, unless the rollback itself failed.
func (r *repaveReconciler) recordFailedIsolationChange(
	ctx context.Context,
	active *ActiveMachine,
	change v1alpha3.IsolationChangeStatus,
	cause error,
) reconcile.Result {
	rollbackFailed := errors.Is(cause, migrate.ErrRollbackFailed)

	r.log.Error("the isolation change failed; it is tried again at the next repave", "to", change.TargetMode, "error", cause)

	change.Phase = v1alpha3.IsolationChangeFailed
	change.Message = cause.Error()
	change.LastTransitionTime = metav1.Now()

	status := isolationStatus{Change: &change}
	if !rollbackFailed {
		status.Applied = v1alpha3.IsolationMode(activeModeOf(active))
	}

	if err := patchIsolationStatus(ctx, r.Client, r.machineName, status); err != nil {
		r.log.Warn("could not record the failed isolation change; it is reported at the next start", "error", err)
	} else {
		r.migration().clearTerminalJournals(r.log)
	}

	if rollbackFailed {
		// The source could not be put back, possibly because the target may
		// still run. Restarting the source now could run two node stacks.
		r.log.Error("the node was not restored after the failed isolation change and needs attention; not restarting it")

		return reconcile.Result{}
	}

	r.restoreNodeRegistration(ctx, active)

	return reconcile.Result{}
}

// restoreNodeRegistration makes sure the Node object the operator deleted for
// this repave exists again after a change that did not happen. When the change
// was refused or failed before anything was touched, the old stack never
// restarted, and a running kubelet does not create a deleted Node object by
// itself, so the node would stay out of the cluster. Restarting the stack makes
// the kubelet register it.
func (r *repaveReconciler) restoreNodeRegistration(ctx context.Context, active *ActiveMachine) {
	wait, poll := r.migration().nodeReturn()
	deadline := time.Now().Add(wait)

	for {
		var node corev1.Node

		err := r.Get(ctx, client.ObjectKey{Name: r.nodeName}, &node)
		if err == nil {
			return
		}

		if !apierrors.IsNotFound(err) {
			r.log.Warn("could not check whether the Node came back after the isolation change", "error", err)

			return
		}

		if !time.Now().Before(deadline) {
			break
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}

	r.log.Info("the Node did not come back after the isolation change; restarting the node stack so the kubelet registers it")

	if err := r.nodeOperator.RestartNode(ctx, r.log, active); err != nil {
		r.log.Warn("could not restart the node stack after the isolation change; restart it so the Node registers", "error", err)
	}
}
