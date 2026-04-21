// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// reconcileUpdateMachine processes a single Machine CR reconciliation cycle.
// It reads the current Machine CR from the API server, resolves the
// MachineConfigurationVersion referenced by spec.configurationRef, checks
// for operation counter drift, and performs a full blue/green node update
// if needed.
func (r *reconciler) reconcileUpdateMachine(ctx context.Context, log *slog.Logger, machineName string) error {
	// Read the Machine CR from the API server to get the latest state.
	machine := &v1alpha3.Machine{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: machineName}, machine); err != nil {
		return fmt.Errorf("get Machine %q: %w", machineName, err)
	}

	// Resolve the MachineConfigurationVersion from configurationRef.
	mcv, err := r.resolveMCV(ctx, log, machine)
	if err != nil {
		return fmt.Errorf("resolve MachineConfigurationVersion: %w", err)
	}

	// Re-read the active machine state on each event, because a previous
	// reconciliation may have changed the active nspawn machine name.
	findActive := findActiveMachine
	if r.findActive != nil {
		findActive = r.findActive
	}

	active, err := findActive(log)
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	// Build the desired config from the MCV template overlaid on applied config.
	desired := desiredConfigFromMCV(active.Config, &mcv.Spec.Template)

	// Check for operation counter drift. Counter bumps are the only
	// trigger for reconciliation; actual config drift (version, image,
	// etc.) is checked inside updateNode to decide whether the
	// expensive rootfs reprovision is needed.
	if !hasOperationsDrift(machine) {
		log.Debug("no operation counter drift")
		return nil
	}

	desiredVersion := ""
	if mcv.Spec.Template.Kubernetes != nil {
		desiredVersion = mcv.Spec.Template.Kubernetes.Version
	}

	log.Info("operation counter drift detected",
		"current_version", active.Config.Cluster.Version,
		"desired_version", desiredVersion,
		"mcv", mcv.Name,
	)

	// Set status to Provisioning before starting work.
	if err := updateMachinePhase(ctx, r.client, machine, v1alpha3.MachinePhaseProvisioning, "agent daemon reconciling"); err != nil {
		log.Warn("failed to update phase to Provisioning", "error", err)
		// Continue with reconciliation even if status update fails.
	}

	// Execute the node update with the desired config.
	if err := updateNode(ctx, log, active, desired); err != nil {
		// Update status to Failed.
		failMsg := fmt.Sprintf("node update failed: %v", err)
		if updateErr := updateMachineStatus(ctx, r.client, machine, v1alpha3.MachinePhaseFailed, failMsg, nil, false); updateErr != nil {
			log.Warn("failed to update status after failure", "error", updateErr)
		}

		return fmt.Errorf("node update: %w", err)
	}

	// Acknowledge operation counters.
	acknowledgeOperations(machine)

	// Record the applied configuration version in Machine status.
	configStatus := &v1alpha3.MachineConfigurationRefStatus{
		Name:        machine.Spec.ConfigurationRef.Name,
		Version:     mcv.Spec.Version,
		VersionName: mcv.Name,
	}

	// Update status to Joining with success.
	if err := updateMachineStatus(ctx, r.client, machine, v1alpha3.MachinePhaseJoining, "node update completed", configStatus, true); err != nil {
		log.Warn("failed to update status after success", "error", err)
	}

	log.Info("reconciliation completed",
		"new_version", desired.Cluster.Version,
		"mcv", mcv.Name,
	)

	return nil
}

// resolveMCV looks up the MachineConfigurationVersion referenced by the
// Machine's spec.configurationRef. If configurationRef is nil or the MCV
// cannot be found, an error is returned - the agent requires a valid
// configuration reference to proceed.
func (r *reconciler) resolveMCV(ctx context.Context, log *slog.Logger, machine *v1alpha3.Machine) (*v1alpha3.MachineConfigurationVersion, error) {
	ref := machine.Spec.ConfigurationRef
	if ref == nil {
		return nil, fmt.Errorf("Machine %q has no spec.configurationRef", machine.Name)
	}

	if ref.Version == nil {
		return nil, fmt.Errorf("Machine %q configurationRef has no version set", machine.Name)
	}

	mcvName := fmt.Sprintf("%s-v%d", ref.Name, *ref.Version)
	log.Debug("resolving MachineConfigurationVersion", "mcv", mcvName)

	mcv := &v1alpha3.MachineConfigurationVersion{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: mcvName}, mcv); err != nil {
		return nil, fmt.Errorf("get MachineConfigurationVersion %q: %w", mcvName, err)
	}

	return mcv, nil
}

// desiredConfigFromMCV builds the desired AgentConfig by overlaying fields
// from a MachineConfigurationVersion template onto the applied config.
// Fields not present in the template (API server, CA cert, cluster DNS,
// bootstrap token, etc.) are preserved from the applied config.
func desiredConfigFromMCV(applied *provision.AgentConfig, tmpl *v1alpha3.MachineConfigurationTemplate) *provision.AgentConfig {
	// Deep copy the applied config as the base.
	desired := *applied
	desired.Cluster = applied.Cluster
	desired.Kubelet = applied.Kubelet

	// Copy labels map to avoid aliasing.
	if applied.Kubelet.Labels != nil {
		desired.Kubelet.Labels = make(map[string]string, len(applied.Kubelet.Labels))
		for k, v := range applied.Kubelet.Labels {
			desired.Kubelet.Labels[k] = v
		}
	}

	// Copy taints slice.
	if applied.Kubelet.RegisterWithTaints != nil {
		desired.Kubelet.RegisterWithTaints = make([]string, len(applied.Kubelet.RegisterWithTaints))
		copy(desired.Kubelet.RegisterWithTaints, applied.Kubelet.RegisterWithTaints)
	}

	// Preserve Attest pointer.
	if applied.Attest != nil {
		a := *applied.Attest
		desired.Attest = &a
	}

	// Overlay MCV template fields.
	if tmpl.Kubernetes != nil {
		if v := tmpl.Kubernetes.Version; v != "" {
			desired.Cluster.Version = strings.TrimPrefix(v, "v")
		}

		if labels := tmpl.Kubernetes.NodeLabels; len(labels) > 0 {
			desired.Kubelet.Labels = make(map[string]string, len(labels))
			for k, v := range labels {
				desired.Kubelet.Labels[k] = v
			}
		}

		if taints := tmpl.Kubernetes.RegisterWithTaints; len(taints) > 0 {
			desired.Kubelet.RegisterWithTaints = make([]string, len(taints))
			copy(desired.Kubelet.RegisterWithTaints, taints)
		}
	}

	if tmpl.Agent != nil && tmpl.Agent.Image != "" {
		desired.OCIImage = tmpl.Agent.Image
	}

	return &desired
}

// hasOperationsDrift returns true if any operation counter in spec exceeds
// the corresponding status counter.
func hasOperationsDrift(machine *v1alpha3.Machine) bool {
	if machine.Spec.Operations == nil {
		return false
	}

	specOps := machine.Spec.Operations

	statusOps := machine.Status.Operations
	if statusOps == nil {
		statusOps = &v1alpha3.OperationsStatus{}
	}

	if specOps.RepaveCounter > statusOps.RepaveCounter {
		return true
	}

	return false
}

// acknowledgeOperations copies the spec repave counter to status, marking
// it as acted upon. The reboot counter is not acknowledged here because
// reboots are handled separately.
func acknowledgeOperations(machine *v1alpha3.Machine) {
	if machine.Spec.Operations == nil {
		return
	}

	if machine.Status.Operations == nil {
		machine.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	machine.Status.Operations.RepaveCounter = machine.Spec.Operations.RepaveCounter
}

// updateMachinePhase sets the Machine phase, message, and a corresponding
// NodeUpdated condition via a status update. The condition tracks the
// in-progress state so that phase transitions are always backed by
// observable conditions.
func updateMachinePhase(ctx context.Context, c client.Client, machine *v1alpha3.Machine, phase v1alpha3.MachinePhase, message string) error {
	machine.Status.Phase = phase
	machine.Status.Message = message

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionNodeUpdated,
		Status:             metav1.ConditionFalse,
		Reason:             "InProgress",
		Message:            message,
		ObservedGeneration: machine.Generation,
	})

	return c.Status().Update(ctx, machine)
}

// updateMachineStatus sets the Machine phase, message, operation counters,
// configuration status, and the NodeUpdated condition via a status update.
// configStatus may be nil if no configuration change was applied.
func updateMachineStatus(
	ctx context.Context,
	c client.Client,
	machine *v1alpha3.Machine,
	phase v1alpha3.MachinePhase,
	message string,
	configStatus *v1alpha3.MachineConfigurationRefStatus,
	success bool,
) error {
	machine.Status.Phase = phase
	machine.Status.Message = message

	if configStatus != nil {
		machine.Status.Configuration = configStatus
	}

	condStatus := metav1.ConditionFalse
	condReason := "Failed"

	if success {
		condStatus = metav1.ConditionTrue
		condReason = "Succeeded"
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionNodeUpdated,
		Status:             condStatus,
		Reason:             condReason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})

	return c.Status().Update(ctx, machine)
}
