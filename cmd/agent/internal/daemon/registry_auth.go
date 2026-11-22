// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// registryAuthRequeue is how often the registry-auth reconciler re-runs when
// nothing needs to change. It re-verifies the kubelet's active flags and keeps
// the reported condition fresh; a bounded restart only happens when the desired
// policy actually changed.
const registryAuthRequeue = 5 * time.Minute

// registryAuth restart budget: the agent retries a failed kubelet restart a
// bounded number of times with exponential backoff before giving up and
// reporting the failure. The budget is charged before the disruptive restart,
// so a restart that cannot verify its result still counts.
const (
	registryAuthMaxAttempts   = 3
	registryAuthBaseBackoff   = 2 * time.Minute
	registryAuthMaxBackoff    = 30 * time.Minute
	registryAuthBackoffFactor = 2
)

// registryAuthBudgetDir is where the agent persists the per-slot restart
// budgets. It is a package variable so tests can redirect it; production uses
// the agent config directory next to the applied configuration.
var registryAuthBudgetDir = goalstates.AgentConfigDir

// registryAuthBudgetPath is where the agent persists the per-slot restart
// budget. It survives agent restarts, so a host that reboots does not get a
// fresh budget for a still-failing policy.
func registryAuthBudgetPath(slot string) string {
	return filepath.Join(registryAuthBudgetDir, "registry-auth-restart-"+slot+".json")
}

// registryAuthBudget is the persisted bounded-restart state for one slot.
type registryAuthBudget struct {
	// PolicyHash identifies the policy the budget is charged against. A change
	// to the desired policy resets the budget so the new policy gets a full
	// set of attempts.
	PolicyHash string `json:"policyHash"`
	// Attempts is how many disruptive restarts have been charged.
	Attempts int `json:"attempts"`
	// LastAttempt is when the last restart was charged. It drives the backoff.
	LastAttempt time.Time `json:"lastAttempt"`
}

// registryAuthReconciler configures the kubelet's image credential provider per
// the operator-managed Machine.spec.registryAuth policy, verifies the kubelet
// actually picked up the flags, restarts the kubelet (bounded) when the policy
// changes, and reports the outcome in Machine.Status.conditions (RegistryAuth).
type registryAuthReconciler struct {
	log         *slog.Logger
	machineName string

	// active returns the currently active node slot and its applied
	// configuration. It is the nodeOperator's FindActiveMachine.
	active func(*slog.Logger) (*ActiveMachine, error)
	// backends is the backend of every isolation mode this agent can operate.
	backends ModeBackends
	// client writes the Machine status condition.
	client client.Client
}

// registryAuthActivePolicy reports the desired registry-auth policy for the
// machine. active is true when a managed provider should be configured; policy
// is the full policy (set only when active).
type registryAuthActivePolicy struct {
	active     bool
	policy     registryauth.Config
	policyHash string
}

func desiredRegistryAuth(machine *v1alpha3.Machine) registryAuthActivePolicy {
	p := registryAuthActivePolicy{}

	spec := machine.Spec.RegistryAuth
	if spec == nil || spec.Mode == "" || spec.Mode == registryauth.ExternalMode {
		return p
	}

	policy := registryauth.Config{
		Mode:         spec.Mode,
		Registry:     spec.Registry,
		ClientID:     spec.ClientID,
		TenantID:     spec.TenantID,
		SNIName:      spec.SNIName,
		ProxyAddress: spec.ProxyAddress,
		Audience:     spec.Audience,
		CACert:       spec.CACert,
	}

	if policy.Mode != registryauth.IdentityBindingMode {
		// An unknown mode is treated as external: the kubelet's authentication
		// is left untouched rather than configured with a half-known policy.
		return p
	}

	p.active = true
	p.policy = policy
	p.policyHash = registryAuthPolicyHash(policy)

	return p
}

// ReconcileRegistryAuth brings the kubelet's credential provider to the desired
// state described by the machine's spec.registryAuth.
func (r *registryAuthReconciler) ReconcileRegistryAuth(ctx context.Context) (reconcile.Result, error) {
	machine, err := getLocalMachine(ctx, r.client, r.machineName)
	if err != nil {
		return reconcile.Result{}, err
	}

	desired := desiredRegistryAuth(machine)

	active, err := r.active(r.log)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("find active machine: %w", err)
	}

	// Resolve the mode-aware node-stack goal state for the active slot. This
	// re-probes the host for the machine's layout, which is cheap and
	// idempotent, and points at the same root the running node uses in every
	// isolation mode. The offline bundle is not resolved here; the provider
	// download resolves it lazily only when a download is actually needed.
	gs, err := registryAuthNodeStart(r.log, active)
	if err != nil {
		return reconcile.Result{}, err
	}

	// The durable work to do for this policy revision: whether the provider
	// flags still need to be (re)applied to the node-config drop-in, reloaded,
	// and the kubelet restarted. This is tracked independently of the on-disk
	// bytes (finding: a pass that writes the files then crashes, or fails the
	// restart, must still retry on the next pass even though the bytes are
	// already correct), and is cleared only when the kubelet is verified
	// running the expected flags.
	pending, err := r.pendingWork(ctx, active, gs, desired)
	if err != nil {
		// A placement or reload failure is reported and retried on the normal
		// requeue.
		if werr := r.writeCondition(ctx, machine, false, "ApplyFailed", err.Error()); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
	}

	if pending.restart {
		if err := r.boundedRestartKubelet(ctx, active, desired); err != nil {
			// The restart (or its verification) failed. The budget already
			// recorded the charge and any backoff; report the failure reason
			// and retry after the backoff. Exhaustion alone never marks the
			// condition healthy.
			reason, message, after := registryAuthFailureOutcome(desired.policyHash, err)
			if werr := r.writeCondition(ctx, machine, false, reason, message); werr != nil {
				r.log.Warn("write registry-auth condition failed", "error", werr)
			}

			if after > 0 {
				return reconcile.Result{RequeueAfter: after}, nil
			}

			return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
		}
	}

	// The policy is applied (or there is nothing to apply). Verify the kubelet
	// is running the expected flags before reporting the condition healthy.
	verified, err := r.verifyKubelet(ctx, active, desired)
	if err != nil {
		if werr := r.writeCondition(ctx, machine, false, "VerifyFailed", err.Error()); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
	}

	if !verified {
		// The kubelet is not running the expected flags. If the pending work is
		// still set, the next pass retries the bounded restart; once the budget
		// is exhausted the condition stays failing rather than flipping to
		// healthy.
		message := "kubelet is not running the expected image credential provider flags"
		if werr := r.writeCondition(ctx, machine, false, "VerificationFailed", message); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
	}

	// The policy is verified healthy. Record the verified revision in the
	// durable ledger and reset the bounded-restart budget so a healthy node does
	// not carry a stale attempt count or a stale "restart pending" flag into a
	// future change. These durability transitions are load-bearing: if the
	// verified-revision record or the budget reset is not durably written (the
	// rename + directory sync fails), a later crash would re-derive work or
	// leave a stale attempt count, and the node could restart unboundedly. A
	// failed durability transition therefore must not produce a healthy
	// success; it is reported as a failure and retried.
	if err := r.clearPendingWork(active.Name, desired.policyHash); err != nil {
		if werr := r.writeCondition(ctx, machine, false, "LedgerWriteFailed", "verified registry-auth ledger could not be durably recorded"); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, fmt.Errorf("clear registry-auth pending work: %w", err)
	}

	if err := r.resetBudget(active.Name, desired.policyHash); err != nil {
		if werr := r.writeCondition(ctx, machine, false, "BudgetResetFailed", "registry-auth restart budget could not be durably reset"); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, fmt.Errorf("reset registry-auth budget: %w", err)
	}

	// The healthy condition reasons match the API contract: "Configured" for an
	// active, verified managed provider, and "External" when the policy leaves
	// the kubelet's authentication untouched.
	reason := registryauth.ConditionReasonExternal
	if desired.active {
		reason = registryauth.ConditionReasonConfigured
	}

	if err := r.writeCondition(ctx, machine, true, reason, registryAuthHealthMessage(desired)); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
}

// pendingWorkResult reports the durable work state after a pass applied the
// policy to disk.
type pendingWorkResult struct {
	// restart is true when a kubelet restart (or reload) is still owed to the
	// kubelet for the current policy revision.
	restart bool
}

// registryAuthPending is the durable record of restart work owed to a slot. It
// is set when files or the node-config drop-in change (so a restart is owed) and
// cleared only when the kubelet is verified running the expected flags. This
// makes retry independent of the on-disk bytes: a pass that wrote the files and
// then crashed, or a restart that failed, or a backoff that deferred the
// restart, all leave pending.work set so the next pass restarts again rather
// than concluding the work is done because the bytes now match.
type registryAuthPending struct {
	// PolicyHash ties the pending work to the policy revision it was charged
	// against; a different policy clears the stale pending work.
	PolicyHash string `json:"policyHash"`
	// Work is true while a restart/reload is owed for PolicyHash.
	Work bool `json:"work"`
}

func registryAuthPendingPath(slot string) string {
	return filepath.Join(registryAuthBudgetDir, "registry-auth-pending-"+slot+".json")
}

// pendingWork brings the provider binary, the credential-provider and azure
// config, and the kubelet node-config drop-in to the desired state, daemon-
// reloads the mode's systemd manager so the drop-in is picked up, and persists
// whether a restart is still owed. It reports the durable work state so the
// caller can decide whether to charge a bounded restart.
//
// The durable intent is persisted BEFORE the on-disk mutations it protects: a
// crash that lands anywhere after the intent is written but before the kubelet
// is verified running the expected flags still finds work owed on the next
// pass, so the original failure window (bytes changed, ledger still false) is
// closed. A policy revision change marks the new revision as owed up front. The
// manager reload is retried whenever work is owed, even when this pass made no
// on-disk change, so a reload that failed or was skipped by a crash is not
// lost.
func (r *registryAuthReconciler) pendingWork(ctx context.Context, active *ActiveMachine, gs *goalstates.MachineGoalState, desired registryAuthActivePolicy) (*pendingWorkResult, error) {
	root := gs.NodeStart.Root()
	slot := active.Name

	// Establish the durable intent for this policy revision BEFORE any on-disk
	// mutation. Work is owed whenever a new revision arrived, a prior pass left
	// work owed (a failed reload or restart, a backoff), or this pass is about
	// to change files. The intent is persisted as work=true up front (not as
	// the as-yet-unknown result) so that a crash landing anywhere after the
	// write but before the kubelet is verified running the expected flags still
	// finds work owed on the next pass: the original failure window, where the
	// bytes changed but the ledger still read false, is closed. The record is
	// cleared to work=false only once this pass determines nothing is owed.
	pending, err := r.readPending(slot)
	if err != nil {
		return nil, err
	}

	owesWork := pending.Work || pending.PolicyHash != desired.policyHash
	if err := r.savePending(slot, desired.policyHash, true); err != nil {
		// Do not proceed with mutations if the intent cannot be durably
		// recorded: a pass that writes files but cannot persist that a restart
		// is owed reintroduces the lost-recovery window.
		return nil, fmt.Errorf("persist registry-auth pending work for %s: %w", slot, err)
	}

	changed := false

	if desired.active {
		if err := ensureACRProviderBinary(ctx, r.log, active, gs); err != nil {
			return nil, fmt.Errorf("ensure ACR provider binary: %w", err)
		}

		providerConfig, err := desired.policy.ProviderConfig()
		if err != nil {
			return nil, fmt.Errorf("render credential provider config: %w", err)
		}

		azureConfig, err := desired.policy.AzureConfig()
		if err != nil {
			return nil, fmt.Errorf("render azure config: %w", err)
		}

		if c, err := writeFileIfChanged(root.Path(goalstates.RegistryAuthCredentialConfigPath), providerConfig, 0o644); err != nil {
			return nil, fmt.Errorf("write credential provider config: %w", err)
		} else {
			changed = changed || c
		}

		if c, err := writeFileIfChanged(root.Path(goalstates.RegistryAuthAzureConfigPath), azureConfig, 0o644); err != nil {
			return nil, fmt.Errorf("write azure config: %w", err)
		} else {
			changed = changed || c
		}

		// Toggle the managed provider flags in the kubelet node-config drop-in
		// by re-rendering from the template with the provider on.
		if c, err := applyProviderToNodeConfig(root, activeModeOf(active), active, gs, &desired.policy); err != nil {
			return nil, fmt.Errorf("apply provider to node-config drop-in: %w", err)
		} else {
			changed = changed || c
		}
	} else {
		// No managed provider: re-render the drop-in from the template with the
		// provider off, leaving the kubelet's other authentication untouched.
		if c, err := applyProviderToNodeConfig(root, activeModeOf(active), active, gs, nil); err != nil {
			return nil, fmt.Errorf("remove provider from node-config drop-in: %w", err)
		} else {
			changed = changed || c
		}
	}

	// A change on this pass means the kubelet must pick it up, so work is owed
	// even when the revision did not change and no prior pass owed it.
	owesWork = owesWork || changed

	if !owesWork {
		// Nothing is owed for this revision: clear the up-front intent so the
		// ledger reflects the verified steady state rather than a stale "restart
		// owed" flag. (A crash after this clear still retries harmlessly: the
		// next pass re-reads a fresh ledger and re-derives that nothing is owed
		// because the bytes already match.)
		if err := r.savePending(slot, desired.policyHash, false); err != nil {
			return nil, fmt.Errorf("clear registry-auth pending work for %s: %w", slot, err)
		}
	}

	if owesWork {
		// Reload the mode's systemd manager so the drop-in is used before the
		// restart. This runs whenever work is owed (a fresh change or a retry
		// after a failed reload), not only when this pass wrote bytes, so a
		// reload that failed or was skipped by a crash is retried.
		if err := r.reloadKubeletUnit(ctx, active); err != nil {
			// The reload failed; the work is still owed (persisted true) so the
			// caller retries on the next pass. Report the reload failure.
			return &pendingWorkResult{restart: true}, err
		}
	}

	return &pendingWorkResult{restart: owesWork}, nil
}

func (r *registryAuthReconciler) readPending(slot string) (*registryAuthPending, error) {
	path := registryAuthPendingPath(slot)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &registryAuthPending{}, nil
		}

		return nil, fmt.Errorf("read registry-auth pending work for %s: %w", slot, err)
	}

	var pending registryAuthPending
	if err := json.Unmarshal(data, &pending); err != nil {
		// A corrupt pending file must not grant a free restart: surface it so
		// the caller does not silently restart.
		return nil, fmt.Errorf("corrupt registry-auth pending work for %s: %w", slot, err)
	}

	return &pending, nil
}

func (r *registryAuthReconciler) savePending(slot, policyHash string, work bool) error {
	return writeJSONFile(registryAuthPendingPath(slot), &registryAuthPending{PolicyHash: policyHash, Work: work})
}

// clearPendingWork records that the policy revision was applied and verified,
// so the durable ledger stops owing a restart for it. It persists
// {PolicyHash, Work: false} rather than removing the file, so the next pass can
// tell that the current revision is already verified and does not re-derive work
// (which a fresh record, PolicyHash="" Work=false, would do for a managed policy
// by treating the hash as a revision change).
func (r *registryAuthReconciler) clearPendingWork(slot, policyHash string) error {
	return r.savePending(slot, policyHash, false)
}

// reloadKubeletUnit runs systemctl daemon-reload in the active node stack so a
// changed drop-in is picked up before the kubelet is restarted. It is the
// mode-correct reload: it runs through the node's backend (in-machine for
// nspawn/container, on the host for none) so the manager that owns the kubelet
// unit is the one reloaded.
func (r *registryAuthReconciler) reloadKubeletUnit(ctx context.Context, active *ActiveMachine) error {
	backend, err := r.backendFor(active)
	if err != nil {
		return err
	}

	if _, err := backend.Exec(ctx, isolation.SlotName(active.Name), isolation.ExecSpec{
		Args: []string{"systemctl", "daemon-reload"},
	}); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	return nil
}

// boundedRestartKubelet charges the bounded-restart budget and restarts only
// the kubelet unit. A restart is attempted only within budget and only after
// the backoff since the last attempt has elapsed; otherwise the budget is
// reported as the failure so the caller backs off. The budget is charged
// before the disruptive restart, so a restart that cannot be verified still
// counts.
func (r *registryAuthReconciler) boundedRestartKubelet(ctx context.Context, active *ActiveMachine, desired registryAuthActivePolicy) error {
	slot := active.Name

	budget, err := loadRegistryAuthBudget(slot)
	if err != nil {
		return err
	}

	// A policy change resets the budget so the new policy gets a full set of
	// attempts.
	if budget.PolicyHash != desired.policyHash {
		budget = &registryAuthBudget{PolicyHash: desired.policyHash}
	}

	if budget.Attempts >= registryAuthMaxAttempts {
		return &registryAuthBudgetExhausted{budget: budget}
	}

	backoff := registryAuthBackoff(budget.Attempts)
	if wait := time.Until(budget.LastAttempt.Add(backoff)); wait > 0 {
		// The backoff has not elapsed. Do not charge or restart yet; report a
		// retryable backoff so the caller schedules the next attempt.
		return &registryAuthBackoffPending{wait: wait}
	}

	// Charge the attempt before the disruptive restart.
	budget.Attempts++
	budget.LastAttempt = time.Now()

	if err := saveRegistryAuthBudget(slot, budget); err != nil {
		return err
	}

	if err := r.restartKubelet(ctx, active); err != nil {
		return fmt.Errorf("restart kubelet: %w", err)
	}

	// Give the kubelet a moment to come back up before the caller verifies it.
	if err := r.waitKubeletRestart(ctx, active); err != nil {
		return fmt.Errorf("wait for kubelet restart: %w", err)
	}

	return nil
}

// restartKubelet restarts only the kubelet unit in the active node stack, using
// the mode's unit name.
func (r *registryAuthReconciler) restartKubelet(ctx context.Context, active *ActiveMachine) error {
	backend, err := r.backendFor(active)
	if err != nil {
		return err
	}

	unit := registryAuthKubeletUnitName(activeModeOf(active))

	_, err = backend.Exec(ctx, isolation.SlotName(active.Name), isolation.ExecSpec{
		Args: []string{"systemctl", "restart", unit},
	})

	return err
}

// waitKubeletRestart polls the backend until the node stack reports running,
// bounding the wait. It returns an error if the stack does not come back up in
// time, so the caller treats the restart as failed.
func (r *registryAuthReconciler) waitKubeletRestart(ctx context.Context, active *ActiveMachine) error {
	deadline := time.Now().Add(60 * time.Second)

	backend, err := r.backendFor(active)
	if err != nil {
		return err
	}

	for {
		st, err := backend.Status(ctx, isolation.SlotName(active.Name))
		if err == nil && st.Running {
			return nil
		}

		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("node stack not running: %w", err)
			}

			return fmt.Errorf("node stack did not report running within 60s (%s)", st.Detail)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// verifyKubelet confirms the running kubelet's actual argument list matches the
// desired policy. It does not read a configured ExecStart (which the review
// flags as not the running state): it resolves the unit's MainPID in the node's
// namespace, reads that process's /proc/<pid>/cmdline, and checks the exact argv
// tokens. A missing MainPID or an unreadable cmdline is an unknown state and is
// reported as a failure, not as proven cleanup.
func (r *registryAuthReconciler) verifyKubelet(ctx context.Context, active *ActiveMachine, desired registryAuthActivePolicy) (bool, error) {
	backend, err := r.backendFor(active)
	if err != nil {
		return false, err
	}

	unit := registryAuthKubeletUnitName(activeModeOf(active))

	mainPID, err := registryAuthMainPID(ctx, backend, active, unit)
	if err != nil {
		return false, fmt.Errorf("read kubelet MainPID: %w", err)
	}

	if mainPID <= 0 {
		// The kubelet is not running. This is not proven cleanup (a healthy node
		// runs its kubelet); report it as a failure rather than healthy.
		return false, fmt.Errorf("kubelet %s has no MainPID; it is not running", unit)
	}

	argv, err := registryAuthProcessArgv(ctx, backend, active, mainPID)
	if err != nil {
		return false, fmt.Errorf("read kubelet argv: %w", err)
	}

	// External: a running kubelet must not carry any of the managed provider
	// flags. Proven external is a running kubelet with no managed tokens.
	if !desired.active {
		return !registryAuthArgvHasManagedFlags(argv), nil
	}

	// Managed: the running kubelet's EFFECTIVE provider flags must be the
	// managed ones. The generated kubelet unit expands KUBELET_EXTRA_ARGS after
	// KUBELET_NODE_CONFIG_ARGS, and kubelet resolves a repeated flag by the last
	// occurrence, so mere presence of the managed tokens is not proof that they
	// are in effect: a later --image-credential-provider-config=... from another
	// drop-in or environment source would override the managed value. The check
	// therefore takes the last occurrence of each managed flag name and requires
	// it to equal the managed value (and that the flag name is present at all).
	return registryAuthEffectiveManagedFlags(argv), nil
}

// writeCondition publishes the RegistryAuth condition on the Machine status.
func (r *registryAuthReconciler) writeCondition(ctx context.Context, machine *v1alpha3.Machine, ready bool, reason, message string) error {
	status := metav1.ConditionTrue
	if !ready {
		status = metav1.ConditionFalse
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionRegistryAuth,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})

	return r.client.Status().Update(ctx, machine)
}

// registryAuthHealthMessage is the condition message for a healthy policy.
func registryAuthHealthMessage(desired registryAuthActivePolicy) string {
	if !desired.active {
		return "registry auth is external; the kubelet's authentication is untouched"
	}

	return fmt.Sprintf("managed ACR credential provider configured and verified for registry %s", desired.policy.Registry)
}

// backendFor returns the isolation backend for the active machine's mode.
func (r *registryAuthReconciler) backendFor(active *ActiveMachine) (isolation.Backend, error) {
	factory, ok := r.backends[activeModeOf(active)]
	if !ok || factory == nil {
		return nil, fmt.Errorf("this agent has no backend for isolation mode %q", activeModeOf(active))
	}

	return factory(r.log, active.Config), nil
}

// registryAuthControllerAdapter adapts the registry-auth reconciler to the
// controller-runtime Reconciler interface, ignoring the request's identity
// (the agent operates exactly one Machine, named by the reconciler).
type registryAuthControllerAdapter struct {
	r *registryAuthReconciler
}

func (a *registryAuthControllerAdapter) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	return a.r.ReconcileRegistryAuth(ctx)
}

// registryAuthPredicate enqueues on Machine create and on spec.registryAuth
// changes. It does not enqueue on status changes, so the condition this
// reconciler writes does not trigger a self-requeue loop.
func registryAuthPredicate(machineName string) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetName() == machineName
		},
		DeleteFunc: func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetName() == machineName
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew.GetName() != machineName {
				return false
			}

			oldMachine, ok := e.ObjectOld.(*v1alpha3.Machine)
			if !ok {
				return true
			}

			newMachine, ok := e.ObjectNew.(*v1alpha3.Machine)
			if !ok {
				return true
			}

			return !registryAuthSpecEqual(oldMachine.Spec.RegistryAuth, newMachine.Spec.RegistryAuth)
		},
	}
}

func registryAuthSpecEqual(a, b *v1alpha3.RegistryAuthPolicy) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return *a == *b
}
