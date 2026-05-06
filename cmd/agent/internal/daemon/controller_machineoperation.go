// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func (r *daemonReconciler) reconcileMachineOperation(ctx context.Context, name string) (reconcile.Result, error) {
	var op v1alpha3.MachineOperation
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		return reconcile.Result{}, nil
	}

	switch op.Spec.OperationKind {
	case v1alpha3.OperationNodeReboot:
		return r.reconcileNodeReboot(ctx, &op)
	case v1alpha3.OperationAgentUpgrade:
		return r.reconcileAgentUpgrade(ctx, &op)
	default:
		return reconcile.Result{}, nil
	}
}

func (r *daemonReconciler) reconcileNodeReboot(ctx context.Context, op *v1alpha3.MachineOperation) (reconcile.Result, error) {
	machine, err := getLocalMachine(ctx, r.Client, r.machineName)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := markOperationInProgress(ctx, r.Client, op, "restarting active nspawn node"); err != nil {
		return reconcile.Result{}, err
	}

	active, err := r.nodeOperator.FindActiveMachine(r.log)
	if err != nil {
		_, finishErr := finishOperation(ctx, r.Client, op.Name, v1alpha3.OperationPhaseFailed, "ExecutionFailed", err.Error(), 0)
		return reconcile.Result{}, finishErr
	}

	if err := r.nodeOperator.RestartNode(ctx, r.log, active); err != nil {
		_, finishErr := finishOperation(ctx, r.Client, op.Name, v1alpha3.OperationPhaseFailed, "ExecutionFailed", err.Error(), 0)
		return reconcile.Result{}, finishErr
	}

	return finishOperation(ctx, r.Client, op.Name, v1alpha3.OperationPhaseComplete, "Succeeded", "NodeReboot completed", machine.Generation)
}

func (r *daemonReconciler) reconcileAgentUpgrade(ctx context.Context, op *v1alpha3.MachineOperation) (reconcile.Result, error) {
	machine, err := getLocalMachine(ctx, r.Client, r.machineName)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := markOperationInProgress(ctx, r.Client, op, "staging upgraded host agent binary"); err != nil {
		return reconcile.Result{}, err
	}

	downloadURL, err := agentUpgradeDownloadURL(op.Spec.Parameters)
	if err != nil {
		_, finishErr := finishOperation(ctx, r.Client, op.Name, v1alpha3.OperationPhaseFailed, "InvalidParameters", err.Error(), 0)
		return reconcile.Result{}, finishErr
	}

	if err := r.nodeOperator.StageAgentUpgrade(ctx, r.log, downloadURL); err != nil {
		_, finishErr := finishOperation(ctx, r.Client, op.Name, v1alpha3.OperationPhaseFailed, "ExecutionFailed", err.Error(), 0)
		return reconcile.Result{}, finishErr
	}

	result, err := finishOperation(ctx, r.Client, op.Name, v1alpha3.OperationPhaseComplete, "Succeeded", "AgentUpgrade completed", machine.Generation)
	if err != nil {
		return reconcile.Result{}, err
	}

	if err := r.nodeOperator.RestartAgentDaemon(ctx, r.log); err != nil {
		r.log.Warn("failed to restart daemon after AgentUpgrade status update", "operation", op.Name, "error", err)
	}

	return result, nil
}

func (r *daemonReconciler) mapMachineOperation(ctx context.Context, obj client.Object) []daemonRequest {
	op, ok := obj.(*v1alpha3.MachineOperation)
	if !ok || !shouldEnqueueMachineOperation(ctx, r.Client, r.log, r.machineName, r.nodeName, op) {
		return nil
	}

	return []daemonRequest{{Kind: queueItemMachineOperation, Name: op.Name}}
}

func shouldEnqueueMachineOperation(ctx context.Context, c client.Client, log *slog.Logger, machineName, nodeName string, op *v1alpha3.MachineOperation) bool {
	if op.Status.Phase != "" && op.Status.Phase != v1alpha3.OperationPhasePending {
		return false
	}

	switch op.Spec.OperationKind {
	case v1alpha3.OperationNodeReboot, v1alpha3.OperationAgentUpgrade:
		// handled below
	default:
		return false
	}

	matches, err := machineOperationMatchesMachine(ctx, c, machineName, nodeName, op)
	if err != nil {
		log.Warn("invalid MachineOperation target selector", "operation", op.Name, "error", err)
		return false
	}

	return matches
}

func machineOperationMatchesMachine(ctx context.Context, c client.Client, machineName, nodeName string, op *v1alpha3.MachineOperation) (bool, error) {
	if op.Spec.MachineRef != "" {
		return op.Spec.MachineRef == machineName, nil
	}

	if op.Spec.MachineSelector == nil {
		return false, nil
	}

	var node corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err == nil {
		return selectorMatches(op.Spec.MachineSelector, node.Labels)
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get Node %s for selector match: %w", nodeName, err)
	}

	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get Machine %s for selector match: %w", machineName, err)
	}

	return selectorMatches(op.Spec.MachineSelector, machine.Labels)
}

func getLocalMachine(ctx context.Context, c client.Client, machineName string) (*v1alpha3.Machine, error) {
	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
		return nil, fmt.Errorf("get Machine %s: %w", machineName, err)
	}

	return &machine, nil
}

func markOperationInProgress(ctx context.Context, c client.Client, op *v1alpha3.MachineOperation, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKeyFromObject(op), &latest); err != nil {
			return err
		}

		if latest.Status.IsTerminal() {
			return nil
		}

		now := metav1.Now()
		latest.Status.Phase = v1alpha3.OperationPhaseInProgress
		latest.Status.Message = message
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            message,
			ObservedGeneration: latest.Generation,
		})

		return c.Status().Update(ctx, &latest)
	})
}

func finishOperation(
	ctx context.Context,
	c client.Client,
	name string,
	phase v1alpha3.OperationPhase,
	reason string,
	message string,
	observedMachineGeneration int64,
) (reconcile.Result, error) {
	return reconcile.Result{}, retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest v1alpha3.MachineOperation
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &latest); err != nil {
			return client.IgnoreNotFound(err)
		}

		now := metav1.Now()
		if latest.Status.StartedAt == nil {
			latest.Status.StartedAt = &now
		}

		latest.Status.Phase = phase
		latest.Status.Message = message
		latest.Status.CompletedAt = &now
		if observedMachineGeneration > 0 {
			latest.Status.ObservedMachineGeneration = observedMachineGeneration
		}

		conditionStatus := metav1.ConditionTrue
		if phase == v1alpha3.OperationPhaseFailed {
			conditionStatus = metav1.ConditionFalse
		}

		apimeta.SetStatusCondition(&latest.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: latest.Generation,
		})

		return c.Status().Update(ctx, &latest)
	})
}

func selectorMatches(selector *metav1.LabelSelector, targetLabels map[string]string) (bool, error) {
	compiled, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false, err
	}

	return compiled.Matches(labels.Set(targetLabels)), nil
}
