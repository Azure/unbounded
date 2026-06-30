// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// BootLoaderDownloadRecorder records the first observed initial boot loader
// download for active metalman MachineOperations.
type BootLoaderDownloadRecorder struct {
	Client client.Client
	Now    func() metav1.Time
}

func (r *BootLoaderDownloadRecorder) RecordBootLoaderDownloaded(ctx context.Context, machineName, filename string) error {
	if r == nil || r.Client == nil {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		op, ok, err := r.activeOperationForMachine(ctx, machineName)
		if err != nil || !ok {
			return err
		}

		if apimeta.IsStatusConditionTrue(op.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded) {
			return nil
		}

		now := r.now()
		apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
			Type:               v1alpha3.MachineOperationConditionBootLoaderDownloaded,
			Status:             metav1.ConditionTrue,
			Reason:             "Downloaded",
			Message:            fmt.Sprintf("Machine %s downloaded initial boot loader %s", machineName, filename),
			ObservedGeneration: op.Generation,
			LastTransitionTime: now,
		})

		return r.Client.Status().Update(ctx, op)
	})
}

const (
	BootImageWriteStarted  = "Started"
	BootImageWriteFinished = "Finished"
)

// BootImageWriteRecorder records PXE installer disk-write progress for active
// metalman MachineOperations.
type BootImageWriteRecorder struct {
	Client client.Client
	Now    func() metav1.Time
}

func (r *BootImageWriteRecorder) RecordBootImageWrite(ctx context.Context, machineName, stage string) error {
	if r == nil || r.Client == nil {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		op, ok, err := r.activeOperationForMachine(ctx, machineName)
		if err != nil || !ok {
			return err
		}

		cond := apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			return nil
		}

		now := r.now()
		condition := metav1.Condition{
			Type:               v1alpha3.MachineOperationConditionBootImageWritten,
			ObservedGeneration: op.Generation,
			LastTransitionTime: now,
		}

		switch stage {
		case BootImageWriteStarted:
			if cond != nil && cond.Status == metav1.ConditionFalse {
				return nil
			}

			condition.Status = metav1.ConditionFalse
			condition.Reason = "Writing"
			condition.Message = fmt.Sprintf("Machine %s booted the PXE installer and started writing the boot image", machineName)
		case BootImageWriteFinished:
			condition.Status = metav1.ConditionTrue
			condition.Reason = "Succeeded"
			condition.Message = fmt.Sprintf("Machine %s finished writing the boot image to disk", machineName)
		default:
			return fmt.Errorf("unknown boot image write stage %q", stage)
		}

		apimeta.SetStatusCondition(&op.Status.Conditions, condition)

		return r.Client.Status().Update(ctx, op)
	})
}

const (
	CloudInitStarted   = "Started"
	CloudInitSucceeded = "Succeeded"
	CloudInitFailed    = "Failed"
)

// CloudInitStatusRecorder records stable first-boot cloud-init progress for
// active metalman MachineOperations.
type CloudInitStatusRecorder struct {
	Client client.Client
	Now    func() metav1.Time
}

func (r *CloudInitStatusRecorder) RecordCloudInitStatus(ctx context.Context, machineName, stage, message string) error {
	if r == nil || r.Client == nil {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		op, ok, err := r.activeOperationForMachine(ctx, machineName)
		if err != nil || !ok {
			return err
		}

		cond := apimeta.FindStatusCondition(op.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
		if cond != nil && (cond.Status == metav1.ConditionTrue || cond.Reason == "Failed") {
			return nil
		}

		condition := metav1.Condition{
			Type:               v1alpha3.MachineOperationConditionCloudInitDone,
			ObservedGeneration: op.Generation,
			LastTransitionTime: r.now(),
		}

		switch stage {
		case CloudInitStarted:
			if cond != nil && cond.Status == metav1.ConditionFalse {
				return nil
			}

			condition.Status = metav1.ConditionFalse
			condition.Reason = "Running"
			condition.Message = fmt.Sprintf("Machine %s started first-boot cloud-init", machineName)
		case CloudInitSucceeded:
			condition.Status = metav1.ConditionTrue
			condition.Reason = "Succeeded"
			condition.Message = fmt.Sprintf("Machine %s completed first-boot cloud-init successfully", machineName)
		case CloudInitFailed:
			condition.Status = metav1.ConditionFalse
			condition.Reason = "Failed"
			condition.Message = cloudInitFailureMessage(machineName, message)
		default:
			return fmt.Errorf("unknown cloud-init stage %q", stage)
		}

		apimeta.SetStatusCondition(&op.Status.Conditions, condition)

		return r.Client.Status().Update(ctx, op)
	})
}

func (r *BootLoaderDownloadRecorder) activeOperationForMachine(ctx context.Context, machineName string) (*v1alpha3.MachineOperation, bool, error) {
	return activeOperationForMachine(ctx, r.Client, machineName)
}

func (r *BootImageWriteRecorder) activeOperationForMachine(ctx context.Context, machineName string) (*v1alpha3.MachineOperation, bool, error) {
	return activeOperationForMachine(ctx, r.Client, machineName)
}

func (r *CloudInitStatusRecorder) activeOperationForMachine(ctx context.Context, machineName string) (*v1alpha3.MachineOperation, bool, error) {
	return activeOperationForMachine(ctx, r.Client, machineName)
}

func activeOperationForMachine(ctx context.Context, c client.Client, machineName string) (*v1alpha3.MachineOperation, bool, error) {
	var list v1alpha3.MachineOperationList
	if err := c.List(ctx, &list); err != nil {
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

func (r *BootLoaderDownloadRecorder) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}

func (r *BootImageWriteRecorder) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}

func (r *CloudInitStatusRecorder) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}

	return metav1.Now()
}

const maxCloudInitFailureMessageLen = 1024

func cloudInitFailureMessage(machineName, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "cloud-init reported failure"
	}

	result := fmt.Sprintf("Machine %s first-boot cloud-init failed: %s", machineName, message)
	if len(result) > maxCloudInitFailureMessageLen {
		result = result[:maxCloudInitFailureMessageLen-3] + "..."
	}

	return result
}
