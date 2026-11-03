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
)

// ReconcileRepave brings the node to the desired configuration when its Node
// is deleted. That includes the isolation mode the Machine declares in
// spec.isolation: setting the field declares intent and takes effect here, at
// the next repave, never immediately.
//
// A repave is a replacement: the Node was drained and deleted, and the node
// stack is rebuilt. Extension point: an upgrade that swaps containerd and
// kubelet on the host without deleting the Node would be a separate trigger and
// operation beside this one (the none backend's versioned install directory and
// its atomic current link are the mechanism for it). It is not built; this
// reconciler stays tied to Node deletion.
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

	// A mode change an earlier daemon or reconcile left unfinished is settled
	// and reported first. Nothing new starts in the same reconcile: the node was
	// only just put in a known state.
	if settleIsolationChanges(ctx, r.log, r.Client, r.machineName, r.nodeOperator) {
		return reconcile.Result{}, nil
	}

	desiredConfig, appliedRef, err := resolveDesiredRepaveConfig(ctx, r.Client, r.machineName, active.Config)
	if err != nil {
		if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
			r.log.Info("repave skipped until desired configuration is available", "error", err)

			return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
		}

		return reconcile.Result{}, err
	}

	if !hasDrift(active.Config, &desiredConfig.AgentConfig) {
		r.log.Info("repave skipped because no desired configuration drift exists")

		if err := markAppliedConfiguration(ctx, r.Client, r.machineName, appliedRef); err != nil {
			return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
		}

		return reconcile.Result{}, nil
	}

	// A different declared mode is a mode change, not a same-mode repave.
	if desiredMode, err := desiredConfig.IsolationMode(); err != nil || desiredMode != activeModeOf(active) {
		return r.changeIsolation(ctx, active, desiredConfig, appliedRef)
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

func getLocalMachine(ctx context.Context, c client.Client, machineName string) (*v1alpha3.Machine, error) {
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
