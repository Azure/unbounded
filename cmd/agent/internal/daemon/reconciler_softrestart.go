// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
)

// reconcileSoftRestart processes a single operation ConfigMap. It reads the
// ConfigMap, finds the first pending or in_progress operation, executes it,
// and updates the operation state. If more pending operations remain after
// a successful execution, the action is re-enqueued for immediate processing
// via the provided queue.
//
// Operations left in_progress by a crashed process are treated as retriable
// to ensure restart safety.
func (r *reconciler) reconcileSoftRestart(ctx context.Context, log *slog.Logger, source string) error {
	// Parse namespace/name from the source key.
	ns, name, err := parseConfigMapKey(source)
	if err != nil {
		return err
	}

	// Read the ConfigMap.
	var cm corev1.ConfigMap
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cm); err != nil {
		return fmt.Errorf("get ConfigMap %s: %w", source, err)
	}

	ops, err := parseOperations(&cm)
	if err != nil {
		return err
	}

	// Find the first retriable operation (pending or in_progress).
	idx := -1
	for i := range ops {
		if ops[i].State == OpStatePending || ops[i].State == OpStateInProgress {
			idx = i
			break
		}
	}

	if idx < 0 {
		log.Debug("no pending operations in ConfigMap", "configmap", source)
		return nil
	}

	op := &ops[idx]
	log = log.With("op_type", op.Type, "op_index", idx)

	// Mark in_progress and persist before executing.
	op.State = OpStateInProgress
	op.Message = ""

	if err := r.updateConfigMapOps(ctx, &cm, ops); err != nil {
		return fmt.Errorf("update operation to in_progress: %w", err)
	}

	// Execute the operation.
	log.Info("executing operation")

	execErr := r.executeOperation(ctx, log, op)

	// Update state based on result.
	if execErr != nil {
		op.State = OpStateFailed
		op.Message = execErr.Error()
		log.Error("operation failed", "error", execErr)
	} else {
		op.State = OpStateCompleted
		op.Message = ""
		log.Info("operation completed")
	}

	if err := r.updateConfigMapOps(ctx, &cm, ops); err != nil {
		log.Warn("failed to update operation state in ConfigMap", "error", err)
		// Return the original execution error if there was one, otherwise
		// return the update error so the action gets requeued.
		if execErr != nil {
			return execErr
		}

		return fmt.Errorf("update operation state: %w", err)
	}

	if execErr != nil {
		return execErr
	}

	return nil
}

// executeOperation dispatches to the appropriate executor method based on
// the operation type.
func (r *reconciler) executeOperation(ctx context.Context, log *slog.Logger, op *Operation) error {
	switch op.Type {
	case OpTypeReboot:
		// Discover the active nspawn machine at execution time. The name
		// can change after an upgrade (kube1 <-> kube2), so we cannot
		// cache it at daemon startup.
		active, err := r.findActive(log)
		if err != nil {
			return fmt.Errorf("find active machine: %w", err)
		}

		return r.exec.softRestart(ctx, log, active.Name)
	default:
		return fmt.Errorf("unknown operation type: %q", op.Type)
	}
}

// updateConfigMapOps serializes the operations list back into the ConfigMap
// and persists the update.
func (r *reconciler) updateConfigMapOps(ctx context.Context, cm *corev1.ConfigMap, ops []Operation) error {
	if err := serializeOperations(cm, ops); err != nil {
		return err
	}

	return r.client.Update(ctx, cm)
}

// parseConfigMapKey splits a "namespace/name" key into its components.
func parseConfigMapKey(key string) (string, string, error) {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid ConfigMap key %q: expected namespace/name", key)
	}

	return parts[0], parts[1], nil
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
