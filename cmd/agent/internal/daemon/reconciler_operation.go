// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
)

// reconcileOperation processes a single Operation CR. It reads the Operation,
// checks if it is in a retriable phase (Pending or InProgress), executes it,
// and updates the status. Operations left InProgress by a crashed process are
// treated as retriable to ensure restart safety.
func (r *reconciler) reconcileOperation(ctx context.Context, log *slog.Logger, opName string) error {
	var op v1alpha3.Operation
	if err := r.client.Get(ctx, client.ObjectKey{Name: opName}, &op); err != nil {
		return fmt.Errorf("get Operation %q: %w", opName, err)
	}

	// Skip terminal operations.
	if op.Status.IsTerminal() {
		log.Debug("operation already terminal", "operation", opName, "phase", op.Status.Phase)
		return nil
	}

	log = log.With("op_type", op.Spec.Type, "op_phase", op.Status.Phase)

	// Mark InProgress and persist before executing.
	now := metav1.Now()
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Message = ""

	if op.Status.StartedAt == nil {
		op.Status.StartedAt = &now
	}

	if err := r.client.Status().Update(ctx, &op); err != nil {
		return fmt.Errorf("update Operation to InProgress: %w", err)
	}

	// Execute the operation.
	log.Info("executing operation")

	execErr := r.executeOperation(ctx, log, &op)

	// Update status based on result.
	completedAt := metav1.Now()

	if execErr != nil {
		op.Status.Phase = v1alpha3.OperationPhaseFailed
		op.Status.Message = execErr.Error()
		op.Status.CompletedAt = &completedAt
		log.Error("operation failed", "error", execErr)
	} else {
		op.Status.Phase = v1alpha3.OperationPhaseCompleted
		op.Status.Message = ""
		op.Status.CompletedAt = &completedAt
		log.Info("operation completed")
	}

	if err := r.client.Status().Update(ctx, &op); err != nil {
		log.Warn("failed to update Operation status", "error", err)

		if execErr != nil {
			return execErr
		}

		return fmt.Errorf("update Operation status: %w", err)
	}

	// Handle TTL cleanup for terminal operations.
	if op.Spec.TTLSecondsAfterFinished != nil {
		go r.scheduleTTLCleanup(ctx, log, op.Name, *op.Spec.TTLSecondsAfterFinished)
	}

	if execErr != nil {
		return execErr
	}

	return nil
}

// executeOperation dispatches to the appropriate executor method based on
// the operation type.
func (r *reconciler) executeOperation(ctx context.Context, log *slog.Logger, op *v1alpha3.Operation) error {
	switch op.Spec.Type {
	case v1alpha3.OperationTypeSoftReboot:
		// Discover the active nspawn machine at execution time. The name
		// can change after an upgrade (kube1 <-> kube2), so we cannot
		// cache it at daemon startup.
		active, err := r.findActive(log)
		if err != nil {
			return fmt.Errorf("find active machine: %w", err)
		}

		return r.exec.softRestart(ctx, log, active.Name)
	case v1alpha3.OperationTypeHardReboot:
		return fmt.Errorf("HardReboot operations are handled by the machina controller, not the agent")
	default:
		return fmt.Errorf("unknown operation type: %q", op.Spec.Type)
	}
}

// scheduleTTLCleanup waits for the TTL to expire and then deletes the
// Operation CR. This runs in a goroutine and is best-effort.
func (r *reconciler) scheduleTTLCleanup(ctx context.Context, log *slog.Logger, opName string, ttlSeconds int32) {
	delay := time.Duration(ttlSeconds) * time.Second

	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	var op v1alpha3.Operation
	if err := r.client.Get(ctx, client.ObjectKey{Name: opName}, &op); err != nil {
		log.Debug("TTL cleanup: operation already gone", "operation", opName)
		return
	}

	// Only delete if still terminal.
	if !op.Status.IsTerminal() {
		return
	}

	if err := r.client.Delete(ctx, &op); err != nil {
		log.Warn("TTL cleanup: failed to delete operation", "operation", opName, "error", err)
		return
	}

	log.Info("TTL cleanup: deleted operation", "operation", opName)
}

// defaultExecutor is the production implementation of the executor interface.
// It interacts with systemd-nspawn, machinectl, and systemctl on the host.
type defaultExecutor struct{}

func (e *defaultExecutor) softRestart(ctx context.Context, log *slog.Logger, machineName string) error {
	serviceName := fmt.Sprintf("systemd-nspawn@%s.service", machineName)

	log.Info("soft restart: pre-stopping services in machine", "machine", machineName)

	// Gracefully stop kubelet and containerd inside the container so the
	// nspawn restart does not have to force-kill them.
	if _, err := utilexec.MachineRun(ctx, log, machineName,
		"systemctl", "stop", goalstates.SystemdUnitKubelet,
	); err != nil {
		log.Warn("failed to pre-stop kubelet (proceeding anyway)", "machine", machineName, "error", err)
	}

	if _, err := utilexec.MachineRun(ctx, log, machineName,
		"systemctl", "stop", goalstates.SystemdUnitContainerd,
	); err != nil {
		log.Warn("failed to pre-stop containerd (proceeding anyway)", "machine", machineName, "error", err)
	}

	log.Info("soft restart: restarting nspawn service", "service", serviceName)

	// Restart the nspawn service directly. This avoids the machinectl
	// disable/enable cycle that StopNode uses, which tears down the
	// service symlink and can fail to re-enable it.
	if err := utilexec.RunCmd(ctx, log, utilexec.Systemctl(), "restart", serviceName); err != nil {
		return fmt.Errorf("restart %s: %w", serviceName, err)
	}

	// Wait for the machine's D-Bus to become responsive so that
	// subsequent systemd-run commands work.
	if err := waitForMachineReady(ctx, log, machineName); err != nil {
		return fmt.Errorf("wait for machine %s: %w", machineName, err)
	}

	log.Info("soft restart: starting services", "machine", machineName)

	// Re-enable containerd and kubelet inside the machine.
	// systemctl enable --now is idempotent on already-running services.
	if _, err := utilexec.MachineRun(ctx, log, machineName,
		"systemctl", "enable", "--now", goalstates.SystemdUnitContainerd,
	); err != nil {
		return fmt.Errorf("start containerd in %s: %w", machineName, err)
	}

	if _, err := utilexec.MachineRun(ctx, log, machineName,
		"systemctl", "enable", "--now", goalstates.SystemdUnitKubelet,
	); err != nil {
		return fmt.Errorf("start kubelet in %s: %w", machineName, err)
	}

	// Wait for kubelet to report active.
	if err := nodestart.WaitForKubelet(log, machineName).Do(ctx); err != nil {
		return fmt.Errorf("wait for kubelet in %s: %w", machineName, err)
	}

	log.Info("soft restart: completed", "machine", machineName)

	return nil
}

// waitForMachineReady polls the nspawn machine until it is responsive to
// systemd-run commands. This mirrors the wait logic in nodestart but is
// kept here to avoid coupling the soft-restart path to goal-state types.
func waitForMachineReady(ctx context.Context, log *slog.Logger, machine string) error {
	const (
		pollInterval = 500 * time.Millisecond
		timeout      = 30 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		if _, err := utilexec.MachineRun(ctx, log, machine, "/bin/true"); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("machine %s not responsive after %s: %w", machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
