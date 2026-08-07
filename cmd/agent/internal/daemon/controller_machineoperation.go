// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type machineOperationTarget struct {
	client.Client
	log          *slog.Logger
	machineName  string
	nodeOperator nodeOperator
}

func (t *machineOperationTarget) reconcileNodeReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
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

	if err := t.nodeOperator.RestartAgentDaemon(ctx, t.log); err != nil {
		if clearErr := signals.Clear(); clearErr != nil {
			t.log.Warn("failed to clear AgentUpgrade signal", "error", clearErr)
		}

		return finishFailedMachineOperation(ctx, store, op, err)
	}

	return ctrl.Result{}, nil
}

func (t *machineOperationTarget) reconcileAgentReset(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
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
