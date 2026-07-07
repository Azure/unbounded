// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	defaultStatusQueueCapacity = 256

	statusUpdateBootLoaderDownloaded statusUpdateKind = iota
	statusUpdateBootImageWritten
	statusUpdateCloudInitDone
	statusUpdateMachineCondition
)

// StatusQueue records server-observed status milestones. Most updates are best
// effort, but PXE-disable updates are synchronous because the installer only
// sends that completion signal once.
type StatusQueue struct {
	Client   client.Client
	Now      func() metav1.Time
	Log      *slog.Logger
	Capacity int

	once    sync.Once
	updates chan statusUpdate
}

type statusUpdateKind int

type statusUpdate struct {
	machineName string
	kind        statusUpdateKind
	filename    string
	condition   *metav1.Condition
}

func (q *StatusQueue) NeedLeaderElection() bool { return false }

func (q *StatusQueue) Start(ctx context.Context) error {
	q.ensure()

	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-q.updates:
			q.process(ctx, update)
		}
	}
}

func (q *StatusQueue) RecordBootLoaderDownloaded(_ context.Context, machineName, filename string) error {
	return q.enqueue(statusUpdate{machineName: machineName, kind: statusUpdateBootLoaderDownloaded, filename: filename})
}

func (q *StatusQueue) RecordBootImageWritten(_ context.Context, machineName string) error {
	return q.enqueue(statusUpdate{machineName: machineName, kind: statusUpdateBootImageWritten})
}

func (q *StatusQueue) RecordCloudInitDone(_ context.Context, machineName string) error {
	return q.enqueue(statusUpdate{machineName: machineName, kind: statusUpdateCloudInitDone})
}

func (q *StatusQueue) RecordMachineCondition(_ context.Context, machineName string, condition metav1.Condition) error {
	cond := condition

	return q.enqueue(statusUpdate{machineName: machineName, kind: statusUpdateMachineCondition, condition: &cond})
}

func (q *StatusQueue) RecordPXEDisabled(ctx context.Context, machineName, imageName string) error {
	if q == nil || q.Client == nil || machineName == "" {
		return fmt.Errorf("status queue is not configured")
	}

	message := fmt.Sprintf("Machine %s finished writing image %s to disk", machineName, imageName)
	if imageName == "" {
		message = fmt.Sprintf("Machine %s finished writing the boot image to disk", machineName)
	}

	return q.flushOperationCondition(ctx, machineName, v1alpha3.MachineOperationConditionBootImageWritten, metav1.ConditionTrue, "Succeeded", message)
}

func (q *StatusQueue) enqueue(update statusUpdate) error {
	if q == nil || q.Client == nil || update.machineName == "" {
		return nil
	}

	q.ensure()

	select {
	case q.updates <- update:
	default:
		q.log().Warn("dropping status update", "machine", update.machineName, "kind", update.kind)
	}

	return nil
}

func (q *StatusQueue) ensure() {
	q.once.Do(func() {
		capacity := q.Capacity
		if capacity <= 0 {
			capacity = defaultStatusQueueCapacity
		}

		q.updates = make(chan statusUpdate, capacity)
	})
}

func (q *StatusQueue) processNextUpdate(ctx context.Context) bool {
	q.ensure()

	select {
	case update := <-q.updates:
		q.process(ctx, update)

		return true
	default:
		return false
	}
}

func (q *StatusQueue) process(ctx context.Context, update statusUpdate) {
	if err := q.flush(ctx, update); err != nil {
		q.log().Error("flushing status update", "machine", update.machineName, "kind", update.kind, "err", err)
	}
}

func (q *StatusQueue) flush(ctx context.Context, update statusUpdate) error {
	switch update.kind {
	case statusUpdateBootLoaderDownloaded:
		return q.flushOperationCondition(ctx, update.machineName, v1alpha3.MachineOperationConditionBootLoaderDownloaded, metav1.ConditionTrue, "Downloaded", fmt.Sprintf("Machine %s downloaded initial boot loader %s", update.machineName, update.filename))
	case statusUpdateBootImageWritten:
		return q.flushOperationCondition(ctx, update.machineName, v1alpha3.MachineOperationConditionBootImageWritten, metav1.ConditionTrue, "Succeeded", fmt.Sprintf("Machine %s finished writing the boot image to disk", update.machineName))
	case statusUpdateCloudInitDone:
		return q.flushOperationCondition(ctx, update.machineName, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", fmt.Sprintf("Machine %s completed first-boot cloud-init successfully", update.machineName))
	case statusUpdateMachineCondition:
		if update.condition == nil {
			return nil
		}

		if err := q.flushMachineCondition(ctx, update.machineName, *update.condition); err != nil {
			return err
		}

		if update.condition.Type == v1alpha3.MachineConditionCloudInitDone && update.condition.Status != metav1.ConditionTrue {
			return q.flushOperationCondition(ctx, update.machineName, v1alpha3.MachineOperationConditionCloudInitDone, update.condition.Status, update.condition.Reason, update.condition.Message)
		}

		return nil
	default:
		return fmt.Errorf("unknown status update kind %d", update.kind)
	}
}

func (q *StatusQueue) flushOperationCondition(ctx context.Context, machineName, conditionType string, status metav1.ConditionStatus, reason, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		op, ok, err := q.activeOperationForMachine(ctx, machineName)
		if err != nil || !ok {
			return err
		}

		if apimeta.IsStatusConditionTrue(op.Status.Conditions, conditionType) {
			return nil
		}

		apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: op.Generation,
			LastTransitionTime: q.now(),
		})

		return q.Client.Status().Update(ctx, op)
	})
}

func (q *StatusQueue) flushMachineCondition(ctx context.Context, machineName string, condition metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine v1alpha3.Machine
		if err := q.Client.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}

			return fmt.Errorf("get Machine: %w", err)
		}

		condition.ObservedGeneration = machine.Generation
		apimeta.SetStatusCondition(&machine.Status.Conditions, condition)

		return q.Client.Status().Update(ctx, &machine)
	})
}

func (q *StatusQueue) activeOperationForMachine(ctx context.Context, machineName string) (*v1alpha3.MachineOperation, bool, error) {
	var list v1alpha3.MachineOperationList
	if err := q.Client.List(ctx, &list); err != nil {
		return nil, false, fmt.Errorf("list MachineOperations: %w", err)
	}

	candidates := make([]*v1alpha3.MachineOperation, 0, len(list.Items))
	for i := range list.Items {
		op := &list.Items[i]
		if op.Status.IsTerminal() || op.Spec.OperationKind != v1alpha3.OperationHostReplace || !operationTargetsMachine(op, machineName) {
			continue
		}

		candidates = append(candidates, op)
	}

	if len(candidates) == 0 {
		return nil, false, nil
	}

	sort.Slice(candidates, func(i, j int) bool { return operationBefore(candidates[i], candidates[j]) })

	return candidates[0], true, nil
}

func operationTargetsMachine(op *v1alpha3.MachineOperation, machineName string) bool {
	for _, target := range op.Status.Targets {
		if target.MachineRef != machineName {
			continue
		}

		return target.Phase != v1alpha3.OperationPhaseComplete && target.Phase != v1alpha3.OperationPhaseFailed
	}

	return len(op.Status.Targets) == 0 && op.Spec.MachineRef == machineName
}

func (q *StatusQueue) log() *slog.Logger {
	if q.Log != nil {
		return q.Log
	}

	return slog.Default()
}

func (q *StatusQueue) now() metav1.Time {
	if q.Now != nil {
		return q.Now()
	}

	return metav1.Now()
}
