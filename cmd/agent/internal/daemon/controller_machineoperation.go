// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const agentUpgradeLockRetryDelay = 2 * time.Second

type machineOperationTarget struct {
	installation *installstate.Store
	client.Client
	log                  *slog.Logger
	machineName          string
	nodeOperator         nodeOperator
	agentUpgradeLockPath string
	// requestRestart stops the controller so the daemon restarts from the
	// staged binary; see ErrRestartRequested. It returns at once.
	requestRestart func()
	// restartRequested is set once this daemon has staged an AgentUpgrade and
	// asked to be restarted for it; see reconcileAgentUpgrade.
	restartRequested atomic.Bool
	// rebootRequested is set once this daemon has issued a HostOSReboot;
	// see reconcileHostOSReboot.
	rebootRequested atomic.Bool
}

// handlers returns the MachineOperation handlers this daemon runs.
func (t *machineOperationTarget) handlers() daemon.MachineOperationHandlers {
	return daemon.MachineOperationHandlers{
		v1alpha3.OperationNodeReboot:   t.unlessStopping(t.reconcileNodeReboot),
		v1alpha3.OperationAgentUpgrade: t.unlessStopping(t.reconcileAgentUpgrade),
		v1alpha3.OperationAgentReset:   t.unlessStopping(t.reconcileAgentReset),
		v1alpha3.OperationHostOSReboot: t.unlessStopping(t.reconcileHostOSReboot),
	}
}

// unlessStopping defers every operation once this daemon has issued a host
// reboot or staged an agent restart. systemd stops the daemon shortly after
// either, which would interrupt a handler partway through and could leave its
// operation InProgress with nothing to report it. Deferred operations stay
// Pending, and the next daemon picks them up when it starts watching.
func (t *machineOperationTarget) unlessStopping(handler daemon.MachineOperationHandler[int64]) daemon.MachineOperationHandler[int64] {
	return func(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
		if t.rebootRequested.Load() || t.restartRequested.Load() {
			t.log.Info("deferring MachineOperation: this daemon is stopping for a host reboot or agent restart",
				"operation", op.Name, "kind", op.Kind)

			return ctrl.Result{}, nil
		}

		return handler(ctx, store, op)
	}
}

func (t *machineOperationTarget) reconcileNodeReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	lock, err := t.installation.AcquireMutationLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	defer releaseInstallationLock(t.log, lock)

	if err := store.MarkInProgress(ctx, op, "restarting active nspawn node"); err != nil {
		return ctrl.Result{}, err
	}

	machine, err := getLocalMachine(ctx, t.Client, t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	active, err := t.nodeOperator.FindActiveMachine(t.log)
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	if err := t.nodeOperator.RestartNode(ctx, t.log, active); err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:                     v1alpha3.OperationPhaseComplete,
		Reason:                    "Succeeded",
		Message:                   "NodeReboot completed",
		ObservedMachineGeneration: machine.Generation,
	})
}

func (t *machineOperationTarget) reconcileAgentUpgrade(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	// Once it has staged an AgentUpgrade, this daemon is stopping to restart
	// from the staged binary. Until it has stopped, the operation
	// it staged is InProgress but not finished, so it can be reconciled again,
	// and staging again would switch the slots back. The restarted daemon
	// reports that operation from the pending signal, and picks up any other
	// that is still Pending when it starts watching, so this daemon takes no
	// further AgentUpgrade. It goes by what this process did rather than by the
	// signal file, which a daemon that failed to report it at startup can leave
	// behind.
	if t.restartRequested.Load() {
		t.log.Info("not starting an AgentUpgrade: this daemon is stopping to be restarted for one it staged",
			"operation", op.Name)

		return ctrl.Result{}, nil
	}

	installationLock, err := t.installation.AcquireMutationLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	defer releaseInstallationLock(t.log, installationLock)

	lockPath := t.agentUpgradeLockPath
	if lockPath == "" {
		lockPath = goalstates.DaemonAgentUpgradeLockPath
	}

	lock, err := agentbinary.AcquireHostActivationLock(lockPath)
	if errors.Is(err, agentbinary.ErrActivationInProgress) {
		t.log.Info("waiting for host agent activation lock", "operation", op.Name)
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("acquire host agent activation lock: %w", err)
	}

	defer func() {
		if err := lock.Close(); err != nil {
			t.log.Warn("failed to release host agent activation lock", "error", err)
		}
	}()

	if err := store.MarkInProgress(ctx, op, "staging upgraded host agent binary"); err != nil {
		return ctrl.Result{}, err
	}

	machine, err := getLocalMachine(ctx, t.Client, t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	request, err := parseAgentUpgradeRequest(op.Parameters)
	if err != nil {
		return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{Phase: v1alpha3.OperationPhaseFailed, Reason: "InvalidParameters", Message: err.Error()})
	}

	t.log.Info("staging AgentUpgrade binary", "operation", op.Name)

	if err := t.nodeOperator.StageAgentUpgrade(ctx, t.log, request); err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	signals, err := newAgentUpgradeSignalOperator()
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	if err := signals.RecordPending(op.Name, machine.Generation); err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	// The operation stays InProgress. The daemon that starts in this one's
	// place, from the staged binary, reports it from the pending signal, and
	// recovery reports it if that daemon cannot start.
	t.log.Info("staged AgentUpgrade; restarting the daemon from it", "operation", op.Name)
	t.restartRequested.Store(true)
	t.requestRestart()

	return ctrl.Result{}, nil
}

func (t *machineOperationTarget) reconcileAgentReset(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	lock, err := t.installation.AcquireLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	defer releaseInstallationLock(t.log, lock)

	if err := store.MarkInProgress(ctx, op, "resetting unbounded agent"); err != nil {
		return ctrl.Result{}, err
	}

	machine, err := getLocalMachine(ctx, t.Client, t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := t.nodeOperator.ResetAgentResources(ctx, t.log); err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	if err := store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:                     v1alpha3.OperationPhaseComplete,
		Reason:                    "Succeeded",
		Message:                   "AgentReset completed",
		ObservedMachineGeneration: machine.Generation,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Stop the daemon last because systemctl stop terminates this running process.
	return ctrl.Result{}, t.nodeOperator.StopDaemon(ctx, t.log)
}

func finishFailedMachineOperation(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation, executionErr error) (ctrl.Result, error) {
	err := store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   v1alpha3.OperationPhaseFailed,
		Reason:  "ExecutionFailed",
		Message: executionErr.Error(),
	})

	return ctrl.Result{}, err
}

func publishAndClearAgentUpgradeSignals(ctx context.Context, log *slog.Logger, c client.Client) error {
	signals, err := newAgentUpgradeSignalOperator()
	if err != nil {
		return err
	}

	signal, err := signals.Read()
	if err != nil {
		return fmt.Errorf("read AgentUpgrade signal: %w", err)
	}

	switch {
	case signal == nil:
		return nil
	case signal.FailureMessage != "":
		op := daemon.MachineOperation{Name: signal.OperationName}

		result := daemon.MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "DaemonFailed",
			Message: signal.FailureMessage,
		}
		if err := daemon.FinishMachineOperation(ctx, c, op, result); err != nil {
			return err
		}

		if err := signals.Clear(); err != nil {
			return fmt.Errorf("remove AgentUpgrade failure signal: %w", err)
		}

		log.Info("published AgentUpgrade daemon failure signal", "operation", signal.OperationName)
	default:
		if err := daemon.FinishMachineOperation(
			ctx,
			c,
			daemon.MachineOperation{Name: signal.OperationName},
			daemon.MachineOperationResult[int64]{
				Phase:                     v1alpha3.OperationPhaseComplete,
				Reason:                    "Succeeded",
				Message:                   "AgentUpgrade completed",
				ObservedMachineGeneration: signal.ObservedMachineGeneration,
			},
		); err != nil {
			return err
		}

		if err := signals.Clear(); err != nil {
			log.Warn("failed to clear AgentUpgrade signal", "error", err)
		}

		log.Info("published AgentUpgrade success signal", "operation", signal.OperationName)
	}

	return nil
}
