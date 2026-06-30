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

func TestStatusQueueRecordsServerMilestones(t *testing.T) {
	t.Parallel()

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
	queue := &StatusQueue{Client: c, Now: fixedNow}

	require.NoError(t, queue.RecordBootLoaderDownloaded(context.Background(), machine.Name, "shimx64.efi"))
	require.NoError(t, queue.RecordBootImageWritten(context.Background(), machine.Name))
	require.NoError(t, queue.RecordCloudInitDone(context.Background(), machine.Name))
	require.NoError(t, queue.RecordMachineCondition(context.Background(), machine.Name, metav1.Condition{
		Type:    v1alpha3.MachineConditionCloudInitDone,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "cloud-init completed successfully",
	}))
	require.NoError(t, queue.RecordPXEDisabled(context.Background(), machine.Name, 3, "ghcr.io/test/image:v1"))
	for queue.processNextUpdate(context.Background()) {
	}

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

func TestStatusQueueDropsWhenFull(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	queue := &StatusQueue{Client: c, Capacity: 1}

	require.NoError(t, queue.RecordBootImageWritten(context.Background(), "machine-1"))
	require.NoError(t, queue.RecordCloudInitDone(context.Background(), "machine-1"))
	require.Len(t, queue.updates, 1)
}

func TestStatusQueueLatchesTrueOperationConditions(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-latch", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	queue := &StatusQueue{Client: c, Now: fixedNow}

	require.NoError(t, queue.RecordBootLoaderDownloaded(context.Background(), "machine-1", "shimx64.efi"))
	require.True(t, queue.processNextUpdate(context.Background()))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, cond)
	require.Contains(t, cond.Message, "shimx64.efi")
	wantTransition := fixedNow()
	require.True(t, cond.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", cond.LastTransitionTime, wantTransition)

	queue.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Minute)) }
	require.NoError(t, queue.RecordBootLoaderDownloaded(context.Background(), "machine-1", "grubx64.efi"))
	require.True(t, queue.processNextUpdate(context.Background()))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	cond = apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, cond)
	require.Contains(t, cond.Message, "shimx64.efi")
	require.True(t, cond.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", cond.LastTransitionTime, wantTransition)
}

func TestStatusQueueIgnoresTerminalAndNonHostReplaceOperations(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	terminal := testOperation("op-terminal", v1alpha3.OperationHostReplace)
	terminal.Status.Phase = v1alpha3.OperationPhaseComplete
	terminal.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseComplete,
	}}
	nonReplace := testOperation("op-poweron", v1alpha3.OperationHostPowerOn)
	nonReplace.Status.Phase = v1alpha3.OperationPhaseInProgress
	nonReplace.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-2",
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(terminal, nonReplace).WithStatusSubresource(terminal, nonReplace).Build()
	queue := &StatusQueue{Client: c, Now: fixedNow}

	require.NoError(t, queue.RecordBootLoaderDownloaded(context.Background(), "machine-1", "shimx64.efi"))
	require.NoError(t, queue.RecordBootImageWritten(context.Background(), "machine-2"))
	for queue.processNextUpdate(context.Background()) {
	}

	var updatedTerminal v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: terminal.Name}, &updatedTerminal))
	require.Nil(t, apimeta.FindStatusCondition(updatedTerminal.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded))

	var updatedNonReplace v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: nonReplace.Name}, &updatedNonReplace))
	require.Nil(t, apimeta.FindStatusCondition(updatedNonReplace.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten))
}
