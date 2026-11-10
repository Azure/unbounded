// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/machineconfigs"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// ReconcileRepave brings the node to the desired configuration when its Node
// is deleted. That includes the isolation mode the Machine declares in
// spec.isolation: setting the field declares intent and takes effect here, at
// the next repave, never immediately.
func (r *repaveReconciler) ReconcileRepave(ctx context.Context, _ string) (reconcile.Result, error) {
	lock, err := r.installation.AcquireMutationLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return reconcile.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return reconcile.Result{}, err
	}

	defer releaseInstallationLock(r.log, lock)

	active, err := r.nodeOperator.FindActiveMachine(r.log)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("find active machine: %w", err)
	}

	// A reprovision runs without this daemon, which it is about to remove.
	// Nothing new starts until the daemon it installs takes over.
	if r.handoff.active() {
		r.log.Info("repave deferred: an isolation reprovision is resetting this host")

		return reconcile.Result{}, nil
	}

	// A mode change an earlier daemon or reconcile left unfinished is settled
	// and reported first.
	if result, done := r.settleBeforeRepave(ctx); done {
		return result, nil
	}

	skipChangeTo := r.skipChangeTo
	r.skipChangeTo = ""

	desiredConfig, appliedRef, err := resolveDesiredRepaveConfig(ctx, r.Client, r.machineName, active.Config)
	if err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			r.log.Info("repave skipped until desired configuration is available", "error", err)

			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}

		return reconcile.Result{}, err
	}

	// One live check before anything this reconcile can start: a mode change,
	// a repave in either mode, or the restart of a repave that keeps the mode.
	// A declared mode change is drift, so it is checked too. A refusal leaves
	// the running node alone and changes no mode; the unit guards check every
	// start again.
	if hasDrift(active.Config, &desiredConfig.AgentConfig) {
		if err := verifyAllocationMachine(ctx, r.log, r.apiReader, active.Config, r.allocationLaunch); err != nil {
			if errors.Is(err, errAllocationLaunchUnconfirmed) {
				return reconcile.Result{RequeueAfter: allocationLaunchCheckTimeout}, nil
			}

			return reconcile.Result{}, nil
		}
	}

	// A different declared mode is a mode change. When the change is not made
	// now, the repave goes ahead in the mode the node runs, so version and
	// configuration rollouts continue.
	keptMode := false

	if desiredMode := effectiveIsolation(&desiredConfig.AgentConfig); desiredMode != activeModeOf(active) {
		strategy, ok := r.planIsolationChange(ctx, active, desiredMode, skipChangeTo)
		if ok {
			return r.changeIsolation(ctx, active, desiredConfig, strategy, appliedRef)
		}

		desiredConfig.Isolation = active.Config.Isolation
		keptMode = true
	}

	if !hasDrift(active.Config, &desiredConfig.AgentConfig) {
		r.log.Info("repave skipped because no desired configuration drift exists")

		if keptMode {
			// The Node was deleted for a change that is not being made, and no
			// repave restarts the node stack, so make sure it registers again.
			r.restoreNodeRegistration(ctx, active)
		}

		if err := markAppliedConfiguration(ctx, r.Client, r.machineName, appliedRef); err != nil {
			return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
		}

		return reconcile.Result{}, nil
	}

	if err := r.nodeOperator.RepaveNode(ctx, r.log, active, desiredConfig); err != nil {
		return reconcile.Result{}, fmt.Errorf("update node for repave: %w", err)
	}

	if err := markAppliedConfiguration(ctx, r.Client, r.machineName, appliedRef); err != nil {
		return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
	}

	return reconcile.Result{}, nil
}

func resolveDesiredRepaveConfig(
	ctx context.Context,
	c client.Client,
	machineName string,
	applied *provision.AgentConfig,
) (*provision.UnboundedAgentConfig, *v1alpha3.MachineConfigurationRefStatus, error) {
	machine, err := getLocalMachine(ctx, c, machineName)
	if err != nil {
		return nil, nil, err
	}

	desired := configFromApplied(applied)

	// spec.isolation declares the mode the node moves to. Empty is no
	// declaration: the node keeps the mode it runs in.
	if machine.Spec.Isolation != "" {
		desired.Isolation = string(machine.Spec.Isolation)
	}

	// The download sources are not part of the applied agent config, so they
	// come from the Machine every time: without them a repave, and the
	// bootstrap after a reprovision, would fetch from the upstream defaults on
	// a host that can only reach the mirrors the Machine names.
	if machine.Spec.Agent != nil {
		desired.Downloads = provision.AgentDownloadsFromSpec(machine.Spec.Agent.Downloads)
	}

	var appliedRef *v1alpha3.MachineConfigurationRefStatus

	if machine.Spec.ConfigurationRef != nil {
		mcv, err := machineconfigs.ResolveVersionFromRef(ctx, c, machine.Spec.ConfigurationRef)
		if err != nil {
			return nil, nil, err
		}

		applyMachineConfigurationTemplate(&desired, mcv.Spec.Template)
		appliedRef = &v1alpha3.MachineConfigurationRefStatus{
			Name:        machine.Spec.ConfigurationRef.Name,
			Version:     mcv.Spec.Version,
			VersionName: mcv.Name,
		}
	}

	return &desired, appliedRef, nil
}

// planIsolationChange decides whether the declared mode change is made at this
// repave and with which strategy. It reports false, after recording why in
// status.isolationChange, when the change is not made: the target is a mode
// this agent cannot run or cannot change a running node to, or the change
// needs a reprovision and the Machine does not carry the allow-reprovision
// opt-in. skipChangeTo is the target of a change that just failed, which is
// not tried again in the same repave and is not reported again.
func (r *repaveReconciler) planIsolationChange(ctx context.Context, active *ActiveMachine, target, skipChangeTo isolation.Mode) (migrate.Strategy, bool) {
	current := activeModeOf(active)
	keeps := fmt.Sprintf("it keeps running %q and the repave goes ahead in that mode", current)

	if target == skipChangeTo {
		r.log.Info("not trying the failed isolation change again in the same repave", "from", current, "to", target)

		return "", false
	}

	if _, err := isolation.ParseMode(string(target)); err != nil {
		r.refuseIsolationChange(ctx, current, target, "", v1alpha3.IsolationChangeReasonUnsupportedMode,
			fmt.Sprintf("this agent cannot run isolation %q (%v); %s", target, err, keeps))

		return "", false
	}

	// An experimental mode is reached only on a host whose applied agent
	// configuration opts in to it.
	if active.Config != nil {
		if err := active.Config.CheckModeEnabled(target); err != nil {
			r.refuseIsolationChange(ctx, current, target, "", v1alpha3.IsolationChangeReasonExperimentalNotEnabled,
				fmt.Sprintf("%v; %s", err, keeps))

			return "", false
		}
	}

	strategy, err := migrate.Plan(current, target)
	if err != nil {
		r.refuseIsolationChange(ctx, current, target, "", v1alpha3.IsolationChangeReasonUnsupportedMode,
			fmt.Sprintf("this agent cannot change the node to isolation %q (%v); %s", target, err, keeps))

		return "", false
	}

	if strategy == migrate.Reprovision {
		machine, err := getLocalMachine(ctx, r.Client, r.machineName)
		if err != nil {
			r.log.Warn("could not read the Machine to check the reprovision opt-in; not changing the isolation mode", "error", err)

			return "", false
		}

		if !allowsReprovision(machine) {
			r.refuseIsolationChange(ctx, current, target, v1alpha3.IsolationChangeReprovision, v1alpha3.IsolationChangeReasonReprovisionNotAllowed,
				fmt.Sprintf("changing isolation mode %s to %s resets the host and bootstraps it again, which is not allowed on this Machine; "+
					"set the annotation %s: \"true\" to allow it, then delete the Node to repave. Until then %s",
					current, target, v1alpha3.AnnotationAllowReprovision, keeps))

			return "", false
		}
	}

	return strategy, true
}

// refuseIsolationChange reports a mode change that is not made in
// status.isolationChange, as Failed with reason. The report is best effort:
// the repave in the current mode goes ahead whether or not it lands.
func (r *repaveReconciler) refuseIsolationChange(
	ctx context.Context,
	current, target isolation.Mode,
	strategy v1alpha3.IsolationChangeStrategy,
	reason, message string,
) {
	r.log.Warn("refusing isolation mode change", "from", current, "to", target, "reason", reason, "message", message)

	err := patchIsolationStatus(ctx, r.Client, r.machineName, isolationStatus{
		Applied: v1alpha3.IsolationMode(current),
		Change: &v1alpha3.IsolationChangeStatus{
			TargetMode:         v1alpha3.IsolationMode(target),
			Phase:              v1alpha3.IsolationChangeFailed,
			Strategy:           strategy,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		},
	})
	if err != nil {
		r.log.Warn("could not report the refused isolation change", "error", err)
	}
}

func getLocalMachine(ctx context.Context, c client.Reader, machineName string) (*v1alpha3.Machine, error) {
	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		return nil, fmt.Errorf("get Machine %s: %w", machineName, err)
	}

	return &machine, nil
}

func markAppliedConfiguration(
	ctx context.Context,
	c client.Client,
	machineName string,
	appliedRef *v1alpha3.MachineConfigurationRefStatus,
) error {
	if appliedRef == nil {
		return nil
	}

	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		return fmt.Errorf("get Machine %s: %w", machineName, err)
	}

	machine.Status.Configuration = appliedRef
	setRepavePendingCondition(&machine)

	return c.Status().Update(ctx, &machine)
}

func configFromApplied(applied *provision.AgentConfig) provision.UnboundedAgentConfig {
	return provision.UnboundedAgentConfig{
		AgentConfig: *applied.DeepCopy(),
	}
}

func applyMachineConfigurationTemplate(
	cfg *provision.UnboundedAgentConfig,
	template v1alpha3.MachineConfigurationTemplate,
) {
	if template.Kubernetes != nil {
		if template.Kubernetes.Version != "" {
			cfg.Cluster.Version = strings.TrimPrefix(template.Kubernetes.Version, "v")
		}

		if template.Kubernetes.NodeLabels != nil {
			cfg.Kubelet.Labels = template.Kubernetes.NodeLabels
		}

		if template.Kubernetes.RegisterWithTaints != nil {
			cfg.Kubelet.RegisterWithTaints = taintStrings(template.Kubernetes.RegisterWithTaints)
		}
	}

	if template.Agent != nil {
		cfg.OCIImage = template.Agent.Image
		if template.Agent.LocalDNS != nil {
			cfg.LocalDNS = provision.LocalDNSFromSpec(template.Agent.LocalDNS)
		}
	}
}

func taintStrings(taints []corev1.Taint) []string {
	out := make([]string, 0, len(taints))
	for _, taint := range taints {
		value := taint.Key
		if taint.Value != "" {
			value += "=" + taint.Value
		}

		value += ":" + string(taint.Effect)
		out = append(out, value)
	}

	return out
}

func setRepavePendingCondition(machine *v1alpha3.Machine) {
	if machine.Spec.ConfigurationRef == nil || machine.Status.Configuration == nil {
		return
	}

	status := metav1.ConditionTrue
	reason := "Pending"
	message := "Machine has not been repaved with the desired configuration version"

	if machine.Status.Configuration.Name == machine.Spec.ConfigurationRef.Name &&
		machine.Spec.ConfigurationRef.Version != nil &&
		machine.Status.Configuration.Version == *machine.Spec.ConfigurationRef.Version {
		status = metav1.ConditionFalse
		reason = "Applied"
		message = "Machine has been repaved with the desired configuration version"
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionRepavePending,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})
}
