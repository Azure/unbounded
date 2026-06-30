// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestPXEStatusQueueFlushesServerProgress(t *testing.T) {
	s := testScheme(t)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 7},
		Spec: v1alpha3.MachineSpec{
			PXE:        &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"},
			Operations: &v1alpha3.OperationsSpec{RepaveCounter: 3},
		},
	}
	op := testOperation("op-queue", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, op).
		WithStatusSubresource(machine, op).
		Build()
	queue := &PXEStatusQueue{Client: c, Now: fixedNow, Debounce: -1}
	t.Cleanup(queue.shutdown)

	require.NoError(t, queue.RecordBootLoaderDownloaded(context.Background(), machine.Name, "shimx64.efi"))
	require.NoError(t, queue.RecordBootImageWrite(context.Background(), machine.Name, BootImageWriteStarted))
	require.NoError(t, queue.RecordBootImageWrite(context.Background(), machine.Name, BootImageWriteFinished))
	require.NoError(t, queue.RecordCloudInitStatus(context.Background(), machine.Name, CloudInitStarted, ""))
	require.NoError(t, queue.RecordCloudInitStatus(context.Background(), machine.Name, CloudInitSucceeded, ""))
	require.NoError(t, queue.RecordMachineCondition(context.Background(), machine.Name, metav1.Condition{
		Type:    v1alpha3.MachineConditionCloudInitDone,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "cloud-init completed successfully",
	}))
	require.NoError(t, queue.RecordPXEDisabled(context.Background(), machine.Name, 3, "ghcr.io/test/image:v1"))
	require.True(t, queue.processNextWorkItem(context.Background()))

	var updatedOp v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updatedOp))

	bootLoader := apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, bootLoader)
	require.Equal(t, metav1.ConditionTrue, bootLoader.Status)
	require.Contains(t, bootLoader.Message, "shimx64.efi")

	bootImage := apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, bootImage)
	require.Equal(t, metav1.ConditionTrue, bootImage.Status)
	require.Equal(t, "Succeeded", bootImage.Reason)

	cloudInit := apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	require.NotNil(t, cloudInit)
	require.Equal(t, metav1.ConditionTrue, cloudInit.Status)
	require.Equal(t, "Succeeded", cloudInit.Reason)

	var updatedMachine v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &updatedMachine))

	machineCloudInit := apimeta.FindStatusCondition(updatedMachine.Status.Conditions, v1alpha3.MachineConditionCloudInitDone)
	require.NotNil(t, machineCloudInit)
	require.Equal(t, metav1.ConditionTrue, machineCloudInit.Status)
	require.Equal(t, machine.Generation, machineCloudInit.ObservedGeneration)

	require.NotNil(t, updatedMachine.Status.Operations)
	require.Equal(t, int64(3), updatedMachine.Status.Operations.RepaveCounter)
	repaved := apimeta.FindStatusCondition(updatedMachine.Status.Conditions, v1alpha3.MachineConditionRepaved)
	require.NotNil(t, repaved)
	require.Equal(t, metav1.ConditionTrue, repaved.Status)
	require.Equal(t, "Succeeded", repaved.Reason)
	require.Equal(t, "image=ghcr.io/test/image:v1", repaved.Message)
}

func TestPXEStatusQueueDebouncesPendingUpdates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	queue := &PXEStatusQueue{Client: c, Debounce: time.Hour}
	t.Cleanup(queue.shutdown)

	require.NoError(t, queue.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteStarted))
	require.NoError(t, queue.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteFinished))
	require.Zero(t, queue.workqueue.Len())

	pending := queue.popPending("machine-1")
	require.NotNil(t, pending)
	require.Equal(t, BootImageWriteFinished, pending.bootImageStage)
}

func TestPXEStatusQueueShutdownFlushesPendingUpdates(t *testing.T) {
	s := testScheme(t)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 7},
		Spec: v1alpha3.MachineSpec{
			PXE:        &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"},
			Operations: &v1alpha3.OperationsSpec{RepaveCounter: 3},
		},
	}
	op := testOperation("op-queue-shutdown", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, op).
		WithStatusSubresource(machine, op).
		Build()
	queue := &PXEStatusQueue{Client: c, Now: fixedNow, Debounce: time.Hour}

	require.NoError(t, queue.RecordMachineCondition(context.Background(), machine.Name, metav1.Condition{
		Type:    v1alpha3.MachineConditionCloudInitDone,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "cloud-init completed successfully",
	}))
	require.NoError(t, queue.RecordPXEDisabled(context.Background(), machine.Name, 3, "ghcr.io/test/image:v1"))
	require.Zero(t, queue.workqueue.Len())

	queue.shutdown()

	var updated v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &updated))
	require.NotNil(t, updated.Status.Operations)
	require.Equal(t, int64(3), updated.Status.Operations.RepaveCounter)
	require.Equal(t, metav1.ConditionTrue, apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionCloudInitDone).Status)
	require.Equal(t, metav1.ConditionTrue, apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineConditionRepaved).Status)
}
