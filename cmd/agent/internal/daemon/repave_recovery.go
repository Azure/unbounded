// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func (t *machineOperationTarget) reconcileRepaveRecovery(ctx context.Context, store agentdaemon.MachineOperationStore[int64], op agentdaemon.MachineOperation) (ctrl.Result, error) {
	if t.worker == nil {
		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("repave recovery unavailable"))
	}

	action, id := op.Parameters["action"], op.Parameters["transitionID"]
	if id == "" || (action != "retry" && action != "cancel" && action != "reselect") {
		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("action retry/cancel/reselect and transitionID are required"))
	}

	observed, err := t.worker.store.Load()
	if err != nil {
		return ctrl.Result{}, err
	}

	if observed == nil || observed.TransitionID != id {
		// The worker may have completed and removed the transition after the
		// operation reconciler fetched a nonterminal snapshot. Do not overwrite
		// its terminal result with a stale-transition failure.
		var latest v1alpha3.MachineOperation
		if err := t.worker.reader.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return ctrl.Result{}, err
		}

		if latest.Status.IsTerminal() {
			return ctrl.Result{}, nil
		}

		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("repave transition no longer matches request"))
	}

	if observed.RecoveryOperation == op.Name {
		t.worker.notify()
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if action != "retry" && observed.Phase != "preparing" {
		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("cancel/reselect is permitted only before source shutdown begins"))
	}

	if action != "retry" {
		t.worker.interrupt()
	}

	lock, err := installstate.AcquireLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.log.Error("release recovery lock", "error", err)
		}
	}()

	s, err := t.worker.store.Load()
	if err != nil {
		return ctrl.Result{}, err
	}

	if s == nil || s.TransitionID != id {
		var latest v1alpha3.MachineOperation
		if err := t.worker.reader.Get(ctx, client.ObjectKey{Name: op.Name}, &latest); err != nil {
			return ctrl.Result{}, err
		}

		if latest.Status.IsTerminal() {
			return ctrl.Result{}, nil
		}

		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("repave transition changed"))
	}

	record, err := installstate.DefaultStore().Load()
	if err != nil && (s.InstallID != "" || !errors.Is(err, installstate.ErrNotFound)) {
		return ctrl.Result{}, err
	}

	if err == nil {
		if err := validateRepaveOwner(s, record); err != nil {
			return finishFailedMachineOperation(ctx, store, op, err)
		}
	}

	if s.RecoveryOperation != "" {
		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("another recovery operation is pending"))
	}

	if action != "retry" && s.Phase != "preparing" {
		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("source shutdown already authorized"))
	}

	if action == "reselect" {
		cfg, ref, err := resolveDesiredRepaveConfig(ctx, t.Client, t.machineName, &s.SourceConfig)
		if err != nil {
			return ctrl.Result{}, err
		}

		s.ReselectedConfig, s.ReselectedRef = cfg, ref
	}

	if s.Version == 1 {
		// Adoption does not invent provenance from current desired configuration.
		s.Version = 2
		if s.Phase == "cleaning" {
			s.Phase = "verifying"
		}
	}

	s.RecoveryOperation, s.RecoveryAction = op.Name, action
	if action != "retry" {
		s.Phase = "canceling"
	}

	if err := t.worker.store.Save(s); err != nil {
		return ctrl.Result{}, err
	}

	if err := store.MarkInProgress(ctx, op, "repave recovery accepted for transition "+id); err != nil {
		return ctrl.Result{}, err
	}

	t.worker.notify()

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
