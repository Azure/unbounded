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
)

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

	desiredConfig, appliedRef, err := resolveDesiredRepaveConfig(ctx, r.Client, r.machineName, active.Config)
	if err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			r.log.Info("repave skipped until desired configuration is available", "error", err)

			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}

		return reconcile.Result{}, err
	}

	// A different declared mode is a mode change. This agent cannot change the
	// mode of a running node: it reports the change as refused and repaves in
	// the mode the node runs, so version and configuration rollouts continue.
	if desiredMode := effectiveIsolation(&desiredConfig.AgentConfig); desiredMode != activeModeOf(active) {
		r.refuseIsolationChange(ctx, active, desiredMode)
		desiredConfig.Isolation = active.Config.Isolation
	}

	if !hasDrift(active.Config, &desiredConfig.AgentConfig) {
		r.log.Info("repave skipped because no desired configuration drift exists")

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

// refuseIsolationChange reports a mode change this agent will not make in
// status.isolationChange, as Failed with reason UnsupportedMode. The report is
// best effort: the repave in the current mode goes ahead whether or not it
// lands.
func (r *repaveReconciler) refuseIsolationChange(ctx context.Context, active *ActiveMachine, target isolation.Mode) {
	current := activeModeOf(active)

	message := fmt.Sprintf("this agent cannot change the isolation mode of a running node; it keeps running %q and the repave goes ahead in that mode", current)
	if _, err := isolation.ParseMode(string(target)); err != nil {
		message = fmt.Sprintf("this agent cannot run isolation %q (%v); it keeps running %q and the repave goes ahead in that mode", target, err, current)
	}

	r.log.Warn("refusing isolation mode change", "from", current, "to", target, "reason", message)

	err := patchIsolationStatus(ctx, r.Client, r.machineName, isolationStatus{
		Applied: v1alpha3.IsolationMode(current),
		Change: &v1alpha3.IsolationChangeStatus{
			TargetMode:         v1alpha3.IsolationMode(target),
			Phase:              v1alpha3.IsolationChangeFailed,
			Reason:             v1alpha3.IsolationChangeReasonUnsupportedMode,
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
