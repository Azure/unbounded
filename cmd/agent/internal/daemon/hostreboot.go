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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/executil"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const (
	hostRebootPollInterval = 5 * time.Second
	hostRebootTimeout      = 5 * time.Minute
)

func (nspawnNodeOperator) HostBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read host boot ID: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("host boot ID is empty")
	}

	return id, nil
}

func (nspawnNodeOperator) RebootHost(ctx context.Context, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err := executil.OutputCmd(ctx, log, "reboot")

	return err
}

func (t *machineOperationTarget) reconcileHostOSReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	lock, err := t.installation.AcquireMutationLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	defer releaseInstallationLock(t.log, lock)

	// Read uncached state so retries cannot reissue a reboot while the informer
	// is still catching up with the status written before the command.
	var latest v1alpha3.MachineOperation
	if err := t.operationReader.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if latest.Status.IsTerminal() {
		return ctrl.Result{}, nil
	}

	if latest.Spec.MachineRef == "" {
		return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "InvalidSpec",
			Message: "HostOSReboot requires machineRef",
		})
	}

	if latest.Spec.MachineRef != t.machineName {
		return ctrl.Result{}, nil
	}

	bootID, err := t.nodeOperator.HostBootID()
	if err != nil {
		return ctrl.Result{}, err
	}

	if latest.Status.HostBootID != "" {
		if latest.Status.HostBootID != bootID {
			return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
				Phase:                     v1alpha3.OperationPhaseComplete,
				Reason:                    "Succeeded",
				Message:                   "HostOSReboot completed",
				ObservedMachineGeneration: latest.Status.ObservedMachineGeneration,
			})
		}

		if latest.Status.StartedAt == nil || time.Since(latest.Status.StartedAt.Time) >= hostRebootTimeout {
			return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
				Phase:   v1alpha3.OperationPhaseFailed,
				Reason:  "RebootTimeout",
				Message: "host boot ID did not change after requesting reboot",
			})
		}

		return ctrl.Result{RequeueAfter: hostRebootPollInterval}, nil
	}

	machine, err := getLocalMachine(ctx, t.Client, t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Persist the boot ID and execution intent together before reboot can stop
	// this process. A daemon restart on the same boot must never replay it.
	now := metav1.Now()
	latest.Status.HostBootID = bootID
	latest.Status.Phase = v1alpha3.OperationPhaseInProgress
	latest.Status.StartedAt = &now
	latest.Status.ObservedMachineGeneration = machine.Generation
	latest.Status.Message = "requesting host OS reboot"
	apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineOperationConditionCompleted,
		Status:             metav1.ConditionFalse,
		Reason:             "InProgress",
		Message:            latest.Status.Message,
		ObservedGeneration: latest.Generation,
	})

	if err := t.Status().Update(ctx, &latest); err != nil {
		return ctrl.Result{}, err
	}

	if latest.Status.HostBootID != bootID {
		return ctrl.Result{}, fmt.Errorf("host boot ID was not persisted; update the MachineOperation CRD before using HostOSReboot")
	}

	if err := t.nodeOperator.RebootHost(ctx, t.log); err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	return ctrl.Result{RequeueAfter: hostRebootPollInterval}, nil
}
