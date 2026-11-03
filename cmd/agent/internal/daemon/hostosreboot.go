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
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

const hostSoftRebootMessage = "rebooting host operating system"

// hostSoftRebootWaitInterval is how often a HostSoftReboot that is waiting for
// an out-of-band host operation checks again. Nothing enqueues it when that
// operation finishes.
const hostSoftRebootWaitInterval = 10 * time.Second

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

func (t *machineOperationTarget) reconcileHostSoftReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	// The host is already going down for a reboot this daemon issued. The
	// daemon that starts after it reports that operation and picks up any
	// other that is still Pending.
	if t.rebootRequested.Load() {
		t.log.Info("not starting a HostSoftReboot: this host is already rebooting", "operation", op.Name)
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

	// Read the Machine from the API server: stale cached labels could hide
	// a selector-based out-of-band operation.
	machine, err := getLocalMachine(ctx, t.reader(), t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Out-of-band host operations take priority over a HostSoftReboot: do
	// not reboot the host while a provider or BMC may be powering it off,
	// power cycling it, or replacing it. The check runs after the lock and
	// right before the reboot to keep the window to a newly created
	// operation small.
	blocker, err := unfinishedOutOfBandHostOperation(ctx, t.reader(), machine)
	if err != nil {
		return ctrl.Result{}, err
	}

	if blocker != "" {
		message := fmt.Sprintf("waiting for out-of-band host operation %s", blocker)
		t.log.Info("not starting a HostSoftReboot: an out-of-band host operation on this machine is unfinished",
			"operation", op.Name, "blockedBy", blocker)

		return ctrl.Result{RequeueAfter: hostSoftRebootWaitInterval}, markHostSoftRebootWaiting(ctx, t.Client, op.Name, message)
	}

	bootID, err := t.nodeOperator.BootID()
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	// The boot ID must be on the operation before the reboot is issued: the
	// daemon that starts after the reboot reports the operation from it.
	started, err := startHostSoftReboot(ctx, t.Client, op.Name, t.machineName, machine.Generation, bootID)
	if err != nil || !started {
		return ctrl.Result{}, err
	}

	t.log.Info("rebooting host for HostSoftReboot", "operation", op.Name, "bootID", bootID)
	t.rebootRequested.Store(true)

	if err := t.nodeOperator.RebootHost(ctx, t.log); err != nil {
		// A canceled context means this daemon is stopping, possibly because
		// the reboot has started. Leave the operation InProgress so the next
		// daemon decides from the boot ID whether the reboot happened.
		if ctx.Err() != nil {
			t.log.Warn("daemon stopping while rebooting host; the next daemon reports HostSoftReboot",
				"operation", op.Name, "error", err)

			return ctrl.Result{}, nil
		}

		t.rebootRequested.Store(false)

		return ctrl.Result{}, finishHostSoftReboot(ctx, t.Client, op.Name, t.machineName, daemon.MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "Failed",
			Message: err.Error(),
		})
	}

	// The operation stays InProgress until the daemon that starts after the
	// reboot reports it.
	return ctrl.Result{}, nil
}

// startHostSoftReboot marks the operation InProgress and records bootID in the
// status of the target for machineName. It returns false, without error, if
// the operation is already terminal.
func startHostSoftReboot(ctx context.Context, c client.Client, opName, machineName string, generation int64, bootID string) (bool, error) {
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
		latest.Status.Message = hostSoftRebootMessage
		latest.Status.ObservedMachineGeneration = generation

		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            hostSoftRebootMessage,
			ObservedGeneration: latest.Generation,
		})

		target := operationTargetStatus(&latest.Status, machineName)
		if target == nil {
			latest.Status.Targets = append(latest.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: machineName})
			target = &latest.Status.Targets[len(latest.Status.Targets)-1]
		}

		target.Phase = v1alpha3.OperationPhaseInProgress
		target.Message = hostSoftRebootMessage
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
		return false, fmt.Errorf("record HostSoftReboot boot ID: %w", err)
	}

	return started, nil
}

// finishHostSoftReboot records result on the operation and on its target for
// machineName.
func finishHostSoftReboot(ctx context.Context, c client.Client, opName, machineName string, result daemon.MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: opName}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		now := metav1.Now()
		daemon.SetMachineOperationResult(&latest, result, now)

		if target := operationTargetStatus(&latest.Status, machineName); target != nil {
			target.Phase = result.Phase
			target.Message = result.Message
			target.CompletedAt = &now
		}

		return c.Status().Update(ctx, &latest)
	})
}

// operationTargetStatus returns the target in status for machineName, or nil.
func operationTargetStatus(status *v1alpha3.MachineOperationStatus, machineName string) *v1alpha3.MachineOperationTargetStatus {
	for i := range status.Targets {
		if status.Targets[i].MachineRef == machineName {
			return &status.Targets[i]
		}
	}

	return nil
}

// reader returns the reader for checks that must not act on a stale cache.
func (t *machineOperationTarget) reader() client.Reader {
	if t.apiReader != nil {
		return t.apiReader
	}

	return t.Client
}

// isOutOfBandHostOperation reports whether kind is a host operation that a
// provider or BMC executes rather than the agent on the host. It matches the
// operations machine-ops-controller and metalman serialize among themselves.
// HostAllocate and HostRelease are not out-of-band host operations, because
// they act on a MachineAllocation rather than on an existing host.
func isOutOfBandHostOperation(kind v1alpha3.OperationKind) bool {
	return strings.HasPrefix(string(kind), "Host") && kind != v1alpha3.OperationHostSoftReboot && !kind.IsAllocationOperation()
}

// unfinishedOutOfBandHostOperation returns the name of the oldest out-of-band
// host operation that has not finished on machine, or "" if there is none.
//
// Every such operation counts, whatever its age, so an out-of-band operation
// never waits for a HostSoftReboot and a HostSoftReboot never starts while one
// may run. Out-of-band operations are how an unresponsive host is recovered,
// so they must not depend on the agent on that host.
func unfinishedOutOfBandHostOperation(ctx context.Context, r client.Reader, machine *v1alpha3.Machine) (string, error) {
	var ops v1alpha3.MachineOperationList
	if err := r.List(ctx, &ops); err != nil {
		return "", fmt.Errorf("list MachineOperations: %w", err)
	}

	var oldest *v1alpha3.MachineOperation

	for i := range ops.Items {
		candidate := &ops.Items[i]
		if !isOutOfBandHostOperation(candidate.Spec.OperationKind) || !hostOperationUnfinishedOn(candidate, machine) {
			continue
		}

		if oldest == nil || operationBefore(candidate, oldest) {
			oldest = candidate
		}
	}

	if oldest == nil {
		return "", nil
	}

	return oldest.Name, nil
}

// hostOperationUnfinishedOn reports whether op has not finished on machine.
// A target for machine that still holds boot media, or whose media boot is
// unconfirmed, keeps the operation unfinished even after it reaches a
// terminal phase, matching metalman.
// Once a controller has recorded targets, they are authoritative: the
// operation is unfinished on machine only while its target for machine is.
// Before that, the operation is unfinished on machine if its spec selects it,
// the way the controllers resolve targets: by spec.machineRef, or otherwise by
// matching spec.machineSelector against the Machine labels.
func hostOperationUnfinishedOn(op *v1alpha3.MachineOperation, machine *v1alpha3.Machine) bool {
	target := operationTargetStatus(&op.Status, machine.Name)
	// A finished HostReplace may still hold boot media while cleanup retries.
	if target != nil && target.HasOutstandingBootMedia() {
		return true
	}

	if op.Status.IsTerminal() {
		return false
	}

	if len(op.Status.Targets) > 0 {
		return target != nil && target.Phase != v1alpha3.OperationPhaseComplete && target.Phase != v1alpha3.OperationPhaseFailed
	}

	if op.Spec.MachineRef != "" {
		return op.Spec.MachineRef == machine.Name
	}

	if op.Spec.MachineSelector == nil {
		return false
	}

	// No controller runs an operation whose selector is invalid.
	selector, err := metav1.LabelSelectorAsSelector(op.Spec.MachineSelector)
	if err != nil {
		return false
	}

	return selector.Matches(labels.Set(machine.Labels))
}

// operationBefore orders operations by creation time, then name, as the
// out-of-band host operation controllers do.
func operationBefore(a, b *v1alpha3.MachineOperation) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}

	return a.Name < b.Name
}

// markHostSoftRebootWaiting records message on a HostSoftReboot that is waiting
// to start. It leaves an operation that has started or finished alone, and
// does not write a status that would not change.
func markHostSoftRebootWaiting(ctx context.Context, c client.Client, opName, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: opName}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		if latest.Status.IsTerminal() || latest.Status.Phase == v1alpha3.OperationPhaseInProgress {
			return nil
		}

		if latest.Status.Phase == v1alpha3.OperationPhasePending && latest.Status.Message == message {
			return nil
		}

		latest.Status.Phase = v1alpha3.OperationPhasePending
		latest.Status.Message = message

		return c.Status().Update(ctx, &latest)
	})
}

// publishHostSoftReboots reports HostSoftReboot operations that an earlier daemon
// on this machine issued. Each completes if the host has booted since its
// reboot was issued and fails otherwise, so none stays InProgress.
func publishHostSoftReboots(ctx context.Context, log *slog.Logger, c client.Client, machineName string, currentBootID func() (string, error)) error {
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
		if op.Spec.OperationKind != v1alpha3.OperationHostSoftReboot {
			continue
		}

		target := operationTargetStatus(&op.Status, machineName)
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
			Message:                   "HostSoftReboot completed",
			ObservedMachineGeneration: target.ObservedGeneration,
		}
		if target.BootID == bootID {
			result = daemon.MachineOperationResult[int64]{
				Phase:   v1alpha3.OperationPhaseFailed,
				Reason:  "RebootNotObserved",
				Message: "host did not reboot after HostSoftReboot was issued",
			}
		}

		if err := finishHostSoftReboot(ctx, c, op.Name, machineName, result); err != nil {
			errs = append(errs, fmt.Errorf("report HostSoftReboot %s: %w", op.Name, err))
			continue
		}

		log.Info("reported HostSoftReboot", "operation", op.Name, "phase", result.Phase)
	}

	return errors.Join(errs...)
}
