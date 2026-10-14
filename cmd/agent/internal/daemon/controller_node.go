// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
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

	nodeName := desiredConfig.NodeName
	if nodeName == "" {
		nodeName = r.machineName
	}

	if !hasDrift(active.Config, &desiredConfig.AgentConfig) {
		r.log.Info("repave skipped because no desired configuration drift exists")

		if err := syncNVIDIAIMEXNodeMetadata(
			ctx,
			r.Client,
			nodeName,
			nvidiaIMEXConfig(&desiredConfig.AgentConfig),
		); err != nil {
			return reconcile.Result{}, fmt.Errorf("synchronize NVIDIA IMEX Node metadata: %w", err)
		}

		if err := markAppliedConfiguration(
			ctx,
			r.Client,
			r.machineName,
			appliedRef,
			desiredConfig.IMEXDRAEnabled(),
		); err != nil {
			return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
		}

		return reconcile.Result{}, nil
	}

	if err := r.nodeOperator.RepaveNode(ctx, r.log, active, desiredConfig); err != nil {
		if desiredConfig.IMEXDRAEnabled() && errors.Is(err, goalstates.ErrNVIDIAHostUnqualified) {
			if statusErr := setNVIDIAIMEXQualificationFailedCondition(
				ctx,
				r.Client,
				r.machineName,
				err,
			); statusErr != nil {
				r.log.Warn("failed to report NVIDIA host qualification failure", "error", statusErr)
			}
		}

		return reconcile.Result{}, fmt.Errorf("update node for repave: %w", err)
	}

	if err := syncNVIDIAIMEXNodeMetadata(
		ctx,
		r.Client,
		nodeName,
		nvidiaIMEXConfig(&desiredConfig.AgentConfig),
	); err != nil {
		return reconcile.Result{}, fmt.Errorf("synchronize NVIDIA IMEX Node metadata: %w", err)
	}

	if err := markAppliedConfiguration(
		ctx,
		r.Client,
		r.machineName,
		appliedRef,
		desiredConfig.IMEXDRAEnabled(),
	); err != nil {
		return reconcile.Result{}, fmt.Errorf("mark applied configuration: %w", err)
	}

	return reconcile.Result{}, nil
}

func nvidiaIMEXConfig(cfg *provision.AgentConfig) *provision.NVIDIAIMEXConfig {
	if cfg == nil || cfg.NVIDIA == nil {
		return nil
	}

	return cfg.NVIDIA.IMEX
}

func syncNVIDIAIMEXNodeMetadata(
	ctx context.Context,
	c client.Client,
	nodeName string,
	imex *provision.NVIDIAIMEXConfig,
) error {
	if nodeName == "" {
		return fmt.Errorf("node name is empty")
	}

	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("get Node %s: %w", nodeName, err)
	}

	if node.Labels == nil {
		node.Labels = map[string]string{}
	}

	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	if imex != nil && imex.Mode == provision.NVIDIAIMEXModeDRA {
		profileVersionLabel := config.NormalizeNVIDIAProfileVersionLabel(imex.HostProfileVersion)
		if node.Labels[provision.NVIDIAIMEXModeLabelKey] == string(provision.NVIDIAIMEXModeDRA) &&
			node.Labels[provision.NVIDIAHostProfileLabelKey] == imex.HostProfile &&
			node.Labels[provision.NVIDIAHostProfileVersionLabelKey] == profileVersionLabel &&
			node.Annotations[provision.NVIDIAHostProfileDigestAnnotationKey] == imex.HostProfileDigest {
			return nil
		}

		node.Labels[provision.NVIDIAIMEXModeLabelKey] = string(provision.NVIDIAIMEXModeDRA)
		node.Labels[provision.NVIDIAHostProfileLabelKey] = imex.HostProfile
		node.Labels[provision.NVIDIAHostProfileVersionLabelKey] = profileVersionLabel
		node.Annotations[provision.NVIDIAHostProfileDigestAnnotationKey] = imex.HostProfileDigest
	} else {
		_, modePresent := node.Labels[provision.NVIDIAIMEXModeLabelKey]
		_, profilePresent := node.Labels[provision.NVIDIAHostProfileLabelKey]
		_, versionPresent := node.Labels[provision.NVIDIAHostProfileVersionLabelKey]

		_, digestPresent := node.Annotations[provision.NVIDIAHostProfileDigestAnnotationKey]
		if !modePresent && !profilePresent && !versionPresent && !digestPresent {
			return nil
		}

		delete(node.Labels, provision.NVIDIAIMEXModeLabelKey)
		delete(node.Labels, provision.NVIDIAHostProfileLabelKey)
		delete(node.Labels, provision.NVIDIAHostProfileVersionLabelKey)
		delete(node.Annotations, provision.NVIDIAHostProfileDigestAnnotationKey)
	}

	if err := c.Update(ctx, &node); err != nil {
		return fmt.Errorf("update Node %s: %w", nodeName, err)
	}

	return nil
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

		provision.ApplyMachineConfigurationTemplate(&desired, mcv.Spec.Template)
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
	imexDRA bool,
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
	setNVIDIAIMEXPreparedCondition(&machine, imexDRA)

	return c.Status().Update(ctx, &machine)
}

func setNVIDIAIMEXPreparedCondition(machine *v1alpha3.Machine, enabled bool) {
	if !enabled {
		apimeta.RemoveStatusCondition(&machine.Status.Conditions, v1alpha3.MachineConditionNVIDIAIMEXPrepared)

		return
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionNVIDIAIMEXPrepared,
		Status:             metav1.ConditionTrue,
		Reason:             "HostQualified",
		Message:            "prequalified host evidence is valid and the nspawn boundary exposes NVIDIA DRA IMEX; this does not assert ComputeDomain readiness",
		ObservedGeneration: machine.Generation,
	})
}

func setNVIDIAIMEXQualificationFailedCondition(
	ctx context.Context,
	c client.Client,
	machineName string,
	qualificationErr error,
) error {
	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		return fmt.Errorf("get Machine %s: %w", machineName, err)
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionNVIDIAIMEXPrepared,
		Status:             metav1.ConditionFalse,
		Reason:             "HostQualificationFailed",
		Message:            truncateBootstrapConditionMessage(qualificationErr.Error()),
		ObservedGeneration: machine.Generation,
	})

	if err := c.Status().Update(ctx, &machine); err != nil {
		return fmt.Errorf("update Machine %s NVIDIA qualification condition: %w", machineName, err)
	}

	return nil
}

func configFromApplied(applied *provision.AgentConfig) provision.UnboundedAgentConfig {
	return provision.UnboundedAgentConfig{
		AgentConfig: *applied.DeepCopy(),
	}
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
