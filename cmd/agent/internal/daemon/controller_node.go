// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
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
	"github.com/Azure/unbounded/internal/machineconfigs"
	"github.com/Azure/unbounded/internal/provision"
)

func (r *repaveReconciler) ReconcileRepave(ctx context.Context, _ string) (reconcile.Result, error) {
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

		// The versioned configuration is authoritative for a fleet, so a value
		// set here replaces whatever the machine was bootstrapped with. That is
		// what makes moving to a host OS with a different systemd a version
		// bump rather than an edit of every machine.
		if template.Agent.SystemExtension != nil {
			cfg.SystemExtension = provision.SystemExtensionFromSpec(template.Agent.SystemExtension)
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
