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

func TestBootLoaderDownloadRecorderLatchesFirstDownload(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-boot", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	recorder := &BootLoaderDownloadRecorder{Client: c, Now: fixedNow}

	require.NoError(t, recorder.RecordBootLoaderDownloaded(context.Background(), "machine-1", "shimx64.efi"))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Downloaded", cond.Reason)
	require.Contains(t, cond.Message, "shimx64.efi")
	wantTransition := fixedNow()
	require.True(t, cond.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", cond.LastTransitionTime, wantTransition)

	recorder.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Minute)) }
	require.NoError(t, recorder.RecordBootLoaderDownloaded(context.Background(), "machine-1", "grubx64.efi"))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))

	latched := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, latched)
	require.Contains(t, latched.Message, "shimx64.efi")
	require.True(t, latched.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", latched.LastTransitionTime, wantTransition)
}

func TestBootLoaderDownloadRecorderIgnoresTerminalOperation(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-terminal", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseComplete
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseComplete,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	recorder := &BootLoaderDownloadRecorder{Client: c, Now: fixedNow}

	require.NoError(t, recorder.RecordBootLoaderDownloaded(context.Background(), "machine-1", "shimx64.efi"))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Nil(t, apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded))
}

func TestBootImageWriteRecorderTransitionsStartedToFinished(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-write", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Conditions = []metav1.Condition{{
		Type:               v1alpha3.MachineOperationConditionBootImageWritten,
		Status:             metav1.ConditionUnknown,
		Reason:             "Pending",
		Message:            "waiting for PXE installer to start writing the boot image",
		ObservedGeneration: op.Generation,
	}}
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	recorder := &BootImageWriteRecorder{Client: c, Now: fixedNow}

	require.NoError(t, recorder.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteStarted))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "Writing", cond.Reason)
	require.Contains(t, cond.Message, "started writing")
	wantTransition := fixedNow()
	require.True(t, cond.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", cond.LastTransitionTime, wantTransition)

	recorder.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Minute)) }
	require.NoError(t, recorder.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteFinished))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))

	cond = apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Succeeded", cond.Reason)
	require.Contains(t, cond.Message, "finished writing")
	wantTransition = metav1.NewTime(fixedNow().Add(time.Minute))
	require.True(t, cond.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", cond.LastTransitionTime, wantTransition)
}

func TestBootImageWriteRecorderLatchesFinished(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-write-latched", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	recorder := &BootImageWriteRecorder{Client: c, Now: fixedNow}

	require.NoError(t, recorder.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteFinished))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	wantTransition := fixedNow()

	recorder.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Minute)) }
	require.NoError(t, recorder.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteStarted))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))

	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Succeeded", cond.Reason)
	require.True(t, cond.LastTransitionTime.Equal(&wantTransition), "lastTransitionTime = %s, want %s", cond.LastTransitionTime, wantTransition)
}

func TestBootImageWriteRecorderIgnoresTerminalOperation(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	op := testOperation("op-write-terminal", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseComplete
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "machine-1",
		Phase:      v1alpha3.OperationPhaseComplete,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	recorder := &BootImageWriteRecorder{Client: c, Now: fixedNow}

	require.NoError(t, recorder.RecordBootImageWrite(context.Background(), "machine-1", BootImageWriteStarted))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Nil(t, apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten))
}
