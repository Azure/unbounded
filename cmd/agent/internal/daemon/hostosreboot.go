// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

const hostOSRebootMessage = "rebooting host operating system"

func readBootID() (string, error) {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read boot ID: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("read boot ID: %s is empty", bootIDPath)
	}

	return id, nil
}

func (t *machineOperationTarget) reconcileHostOSReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	// The host is already going down for a reboot this daemon issued. The
	// daemon that starts after it reports that operation and picks up any
	// other that is still Pending.
	if t.rebootRequested.Load() {
		t.log.Info("not starting a HostOSReboot: this host is already rebooting", "operation", op.Name)
		return ctrl.Result{}, nil
	}

	lock, err := t.installation.AcquireMutationLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	defer releaseInstallationLock(t.log, lock)

	machine, err := getLocalMachine(ctx, t.Client, t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	bootID, err := t.nodeOperator.BootID()
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	// The boot ID must be on the operation before the reboot is issued: the
	// daemon that starts after the reboot reports the operation from it.
	started, err := startHostOSReboot(ctx, t.Client, op.Name, t.machineName, machine.Generation, bootID)
	if err != nil || !started {
		return ctrl.Result{}, err
	}

	t.log.Info("rebooting host for HostOSReboot", "operation", op.Name, "bootID", bootID)
	t.rebootRequested.Store(true)

	if err := t.nodeOperator.RebootHost(ctx, t.log); err != nil {
		// A canceled context means this daemon is stopping, possibly because
		// the reboot has started. Leave the operation InProgress so the next
		// daemon decides from the boot ID whether the reboot happened.
		if ctx.Err() != nil {
			t.log.Warn("daemon stopping while rebooting host; the next daemon reports HostOSReboot",
				"operation", op.Name, "error", err)

			return ctrl.Result{}, nil
		}

		t.rebootRequested.Store(false)

		return ctrl.Result{}, finishHostOSReboot(ctx, t.Client, op.Name, t.machineName, daemon.MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "Failed",
			Message: err.Error(),
		})
	}

	// The operation stays InProgress until the daemon that starts after the
	// reboot reports it.
	return ctrl.Result{}, nil
}

// startHostOSReboot marks the operation InProgress and records bootID in the
// status of the target for machineName. It returns false, without error, if
// the operation is already terminal.
func startHostOSReboot(ctx context.Context, c client.Client, opName, machineName string, generation int64, bootID string) (bool, error) {
	started := false

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: opName}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		if latest.Status.IsTerminal() {
			started = false
			return nil
		}

		now := metav1.Now()

		latest.Status.Phase = v1alpha3.OperationPhaseInProgress
		latest.Status.Message = hostOSRebootMessage
		latest.Status.ObservedMachineGeneration = generation

		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            hostOSRebootMessage,
			ObservedGeneration: latest.Generation,
		})

		target := hostOSRebootTarget(&latest.Status, machineName)
		if target == nil {
			latest.Status.Targets = append(latest.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: machineName})
			target = &latest.Status.Targets[len(latest.Status.Targets)-1]
		}

		target.Phase = v1alpha3.OperationPhaseInProgress
		target.Message = hostOSRebootMessage
		target.StartedAt = &now
		target.CompletedAt = nil
		target.ObservedGeneration = generation
		target.BootID = bootID

		if err := c.Status().Update(ctx, &latest); err != nil {
			return err
		}

		started = true

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record HostOSReboot boot ID: %w", err)
	}

	return started, nil
}

// finishHostOSReboot records result on the operation and on its target for
// machineName.
func finishHostOSReboot(ctx context.Context, c client.Client, opName, machineName string, result daemon.MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: opName}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		now := metav1.Now()
		daemon.SetMachineOperationResult(&latest, result, now)

		if target := hostOSRebootTarget(&latest.Status, machineName); target != nil {
			target.Phase = result.Phase
			target.Message = result.Message
			target.CompletedAt = &now
		}

		return c.Status().Update(ctx, &latest)
	})
}

func hostOSRebootTarget(status *v1alpha3.MachineOperationStatus, machineName string) *v1alpha3.MachineOperationTargetStatus {
	for i := range status.Targets {
		if status.Targets[i].MachineRef == machineName {
			return &status.Targets[i]
		}
	}

	return nil
}

// publishHostOSReboots reports HostOSReboot operations that an earlier daemon
// on this machine issued. Each completes if the host has booted since its
// reboot was issued and fails otherwise, so none stays InProgress.
func publishHostOSReboots(ctx context.Context, log *slog.Logger, c client.Client, machineName string, currentBootID func() (string, error)) error {
	var ops v1alpha3.MachineOperationList
	if err := c.List(ctx, &ops); err != nil {
		return fmt.Errorf("list MachineOperations: %w", err)
	}

	var (
		bootID string
		errs   []error
	)

	for i := range ops.Items {
		op := &ops.Items[i]
		if op.Spec.OperationKind.Canonical() != v1alpha3.OperationHostOSReboot {
			continue
		}

		target := hostOSRebootTarget(&op.Status, machineName)
		if target == nil || target.Phase != v1alpha3.OperationPhaseInProgress || target.BootID == "" {
			continue
		}

		if bootID == "" {
			id, err := currentBootID()
			if err != nil {
				return err
			}

			bootID = id
		}

		result := daemon.MachineOperationResult[int64]{
			Phase:                     v1alpha3.OperationPhaseComplete,
			Reason:                    "Succeeded",
			Message:                   "HostOSReboot completed",
			ObservedMachineGeneration: target.ObservedGeneration,
		}
		if target.BootID == bootID {
			result = daemon.MachineOperationResult[int64]{
				Phase:   v1alpha3.OperationPhaseFailed,
				Reason:  "RebootNotObserved",
				Message: "host did not reboot after HostOSReboot was issued",
			}
		}

		if err := finishHostOSReboot(ctx, c, op.Name, machineName, result); err != nil {
			errs = append(errs, fmt.Errorf("report HostOSReboot %s: %w", op.Name, err))
			continue
		}

		log.Info("reported HostOSReboot", "operation", op.Name, "phase", result.Phase)
	}

	return errors.Join(errs...)
}
