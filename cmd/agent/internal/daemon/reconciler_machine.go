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
// It reads the current Machine CR from the API server, checks for operation
// counter drift, and performs a full blue/green node update if needed.
func (r *reconciler) reconcileUpdateMachine(ctx context.Context, log *slog.Logger, machineName string) error {
	// Read the Machine CR from the API server to get the latest state.
	machine := &v1alpha3.Machine{}
	if err := r.client.Get(ctx, client.ObjectKey{Name: machineName}, machine); err != nil {
		return fmt.Errorf("get Machine %q: %w", machineName, err)
	}

	// Re-read the active machine state on each event, because a previous
	// reconciliation may have changed the active nspawn machine name.
	active, err := findActiveMachine(log)
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	// Build the desired config from the Machine CR overlaid on applied config.
	desired := desiredConfigFromMachine(active.Config, machine)

	// Check for operation counter drift. Counter bumps are the only
	// trigger for reconciliation; actual config drift (version, image,
	// etc.) is checked inside updateNode to decide whether the
	// expensive rootfs reprovision is needed.
	if !hasOperationsDrift(machine) {
		log.Debug("no operation counter drift")
		return nil
	}

	log.Info("operation counter drift detected",
		"current_version", active.Config.Cluster.Version,
		"desired_version", specVersion(machine),
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
		if updateErr := updateMachineStatus(ctx, r.client, machine, v1alpha3.MachinePhaseFailed, failMsg, false); updateErr != nil {
			log.Warn("failed to update status after failure", "error", updateErr)
		}

		return fmt.Errorf("node update: %w", err)
	}

	// Acknowledge operation counters.
	acknowledgeOperations(machine)

	// Update status to Joining with success.
	if err := updateMachineStatus(ctx, r.client, machine, v1alpha3.MachinePhaseJoining, "node update completed", true); err != nil {
		log.Warn("failed to update status after success", "error", err)
	}

	log.Info("reconciliation completed",
		"new_version", desired.Cluster.Version,
	)

	return nil
}

// desiredConfigFromMachine builds the desired AgentConfig by overlaying
// fields from the Machine CR onto the applied config. Fields not present in
// the CR (API server, CA cert, cluster DNS, bootstrap token, etc.) are
// preserved from the applied config.
func desiredConfigFromMachine(applied *provision.AgentConfig, machine *v1alpha3.Machine) *provision.AgentConfig {
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

	// Overlay Machine CR fields.
	if machine.Spec.Kubernetes != nil {
		if v := machine.Spec.Kubernetes.Version; v != "" {
			desired.Cluster.Version = strings.TrimPrefix(v, "v")
		}

		if labels := machine.Spec.Kubernetes.NodeLabels; len(labels) > 0 {
			desired.Kubelet.Labels = make(map[string]string, len(labels))
			for k, v := range labels {
				desired.Kubelet.Labels[k] = v
			}
		}

		if taints := machine.Spec.Kubernetes.RegisterWithTaints; len(taints) > 0 {
			desired.Kubelet.RegisterWithTaints = make([]string, len(taints))
			copy(desired.Kubelet.RegisterWithTaints, taints)
		}
	}

	if machine.Spec.Agent != nil && machine.Spec.Agent.Image != "" {
		desired.OCIImage = machine.Spec.Agent.Image
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

// specVersion extracts the kubernetes version from a Machine spec, or returns
// empty string if not set.
func specVersion(machine *v1alpha3.Machine) string {
	if machine.Spec.Kubernetes != nil {
		return machine.Spec.Kubernetes.Version
	}

	return ""
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
// and the NodeUpdated condition via a status update.
func updateMachineStatus(
	ctx context.Context,
	c client.Client,
	machine *v1alpha3.Machine,
	phase v1alpha3.MachinePhase,
	message string,
	success bool,
) error {
	machine.Status.Phase = phase
	machine.Status.Message = message

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
