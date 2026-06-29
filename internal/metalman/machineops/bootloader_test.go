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
