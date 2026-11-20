// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type hostSoftRebootTarget struct {
	machineName string
	generation  int64
}

func (t hostSoftRebootTarget) Reference() string { return t.machineName }

func (t hostSoftRebootTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) {
	return t.generation, nil
}

func (t *machineOperationTarget) reconcileHostSoftReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
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

	// Use one live Machine snapshot for the recorded generation.
	machine, err := getLocalMachine(ctx, t.reader(), t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	handler, err := daemon.NewHostSoftRebootHandler(
		t.Client,
		t.reader(),
		t.log,
		hostSoftRebootTarget{machineName: t.machineName, generation: machine.Generation},
		t.nodeOperator,
		&t.rebootRequested,
		t.machineRefSelectable,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	return handler.Reconcile(ctx, store, op)
}

// reader returns the reader for checks that must not act on a stale cache.
func (t *machineOperationTarget) reader() client.Reader {
	if t.apiReader != nil {
		return t.apiReader
	}

	return t.Client
}
