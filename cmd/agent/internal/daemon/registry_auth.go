// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
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

	// Resolve the node-stack root for the active slot. This re-probes the host
	// for the machine's layout, which is cheap and idempotent.
	root, err := registryAuthRoot(r.log, active)
	if err != nil {
		return reconcile.Result{}, err
	}

	changed, err := r.applyPolicy(ctx, active, root, desired)
	if err != nil {
		// A placement failure is reported and retried on the normal requeue.
		if werr := r.writeCondition(ctx, machine, false, "ApplyFailed", err.Error()); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
	}

	if changed {
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
		message := "kubelet is not running the expected image credential provider flags"
		if werr := r.writeCondition(ctx, machine, false, "VerificationFailed", message); werr != nil {
			r.log.Warn("write registry-auth condition failed", "error", werr)
		}

		return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
	}

	// The policy is verified healthy. Reset the bounded-restart budget so a
	// healthy node does not carry a stale attempt count into a future change.
	if err := r.resetBudget(active.Name, desired.policyHash); err != nil {
		r.log.Warn("reset registry-auth budget failed", "error", err)
	}

	// The healthy condition reasons match the API contract: "Configured" for an
	// active, verified managed provider, and "External" when the policy leaves
	// the kubelet's authentication untouched.
	reason := "External"
	if desired.active {
		reason = "Configured"
	}

	if err := r.writeCondition(ctx, machine, true, reason, registryAuthHealthMessage(desired)); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: registryAuthRequeue}, nil
}

// applyPolicy writes the provider binary, configuration, and the kubelet
// drop-in (or removes them) into the node stack. It reports whether anything on
// disk changed, which is what triggers a kubelet restart.
func (r *registryAuthReconciler) applyPolicy(ctx context.Context, active *ActiveMachine, root isolation.NodeRoot, desired registryAuthActivePolicy) (bool, error) {
	if !desired.active {
		// No managed provider: remove the drop-in and leave the kubelet's
		// authentication entirely alone.
		return removeRegistryAuthDropIn(root)
	}

	if err := ensureACRProviderBinary(ctx, r.log, active, root); err != nil {
		return false, fmt.Errorf("ensure ACR provider binary: %w", err)
	}

	providerConfig, err := desired.policy.ProviderConfig()
	if err != nil {
		return false, fmt.Errorf("render credential provider config: %w", err)
	}

	azureConfig, err := desired.policy.AzureConfig()
	if err != nil {
		return false, fmt.Errorf("render azure config: %w", err)
	}

	changed := false

	if c, err := writeFileIfChanged(root.Path(goalstates.RegistryAuthCredentialConfigPath), providerConfig, 0o644); err != nil {
		return false, fmt.Errorf("write credential provider config: %w", err)
	} else {
		changed = changed || c
	}

	if c, err := writeFileIfChanged(root.Path(goalstates.RegistryAuthAzureConfigPath), azureConfig, 0o644); err != nil {
		return false, fmt.Errorf("write azure config: %w", err)
	} else {
		changed = changed || c
	}

	if c, err := writeRegistryAuthDropInFor(root, active.Mode, &desired.policy); err != nil {
		return false, fmt.Errorf("write registry-auth drop-in: %w", err)
	} else {
		changed = changed || c
	}

	return changed, nil
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
	if wait := budget.LastAttempt.Add(backoff).Sub(time.Now()); wait > 0 {
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

	unit := registryAuthKubeletUnit(active.Mode)

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

// verifyKubelet reads the kubelet's active argument list and confirms it runs
// the expected image credential provider flags. It returns true when the
// expected flags match the desired policy: the managed config and bin-dir for an
// active policy, or neither for an external one.
func (r *registryAuthReconciler) verifyKubelet(ctx context.Context, active *ActiveMachine, desired registryAuthActivePolicy) (bool, error) {
	backend, err := r.backendFor(active)
	if err != nil {
		return false, err
	}

	unit := registryAuthKubeletUnit(active.Mode)

	res, err := backend.Exec(ctx, isolation.SlotName(active.Name), isolation.ExecSpec{
		Args: []string{"systemctl", "show", "-p", "ExecMainArgumentList", "--value", unit},
	})
	if err != nil {
		return false, fmt.Errorf("read kubelet arguments: %w", err)
	}

	args := res.Stdout

	if !desired.active {
		// External: the kubelet must not be running the managed provider flags.
		return !strings.Contains(args, "--image-credential-provider-config="+goalstates.RegistryAuthCredentialConfigPath), nil
	}

	wantConfig := "--image-credential-provider-config=" + goalstates.RegistryAuthCredentialConfigPath
	wantBinDir := "--image-credential-provider-bin-dir=" + goalstates.RegistryAuthProviderBinDir

	return strings.Contains(args, wantConfig) && strings.Contains(args, wantBinDir), nil
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

// registryAuthKubeletUnit is the kubelet systemd unit for the mode.
func registryAuthKubeletUnit(mode isolation.Mode) string {
	switch mode {
	case isolation.ModeNone:
		return "unbounded-kubelet.service"
	default:
		// nspawn and container both run the in-machine kubelet.service.
		return goalstates.SystemdUnitKubelet
	}
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
