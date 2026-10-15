// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
			Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{Image: "ghcr.io/test/image:v1"}},
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
	require.NoError(t, queue.RecordOperationCondition(context.Background(), machine.Name, metav1.Condition{
		Type:    v1alpha3.MachineOperationConditionCloudInitDone,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "cloud-init completed successfully",
	}))
	require.NoError(t, queue.RecordPXEDisabled(context.Background(), machine.Name, "ghcr.io/test/image:v1"))

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
	require.Empty(t, updatedMachine.Status.Conditions)

	bootImage = apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, bootImage)
	require.Equal(t, metav1.ConditionTrue, bootImage.Status)
	require.Equal(t, "Machine machine-1 finished writing image ghcr.io/test/image:v1 to disk", bootImage.Message)
}

func TestStatusQueueDropsBestEffortUpdatesWhenFull(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	queue := &StatusQueue{Client: c, Capacity: 1}

	require.NoError(t, queue.RecordBootImageWritten(context.Background(), "machine-1"))
	require.NoError(t, queue.RecordCloudInitDone(context.Background(), "machine-1"))
	require.Len(t, queue.updates, 1)
}

func TestStatusQueueReportsAfterSiteMove(t *testing.T) {
	t.Parallel()

	for _, sites := range []struct{ old, serving string }{
		{old: "old-site", serving: "local-site"},
		{old: "old-site", serving: ""},
		{old: "", serving: "local-site"},
	} {
		for _, state := range []string{"retained", "terminal-retained", "ordinary"} {
			for _, report := range []string{"BootImageWritten", "CloudInitDone", "PXEDisabled"} {
				t.Run(sites.old+"/"+sites.serving+"/"+state+"/"+report, func(t *testing.T) {
					t.Parallel()

					machine := &v1alpha3.Machine{
						ObjectMeta: metav1.ObjectMeta{
							Name: "machine-1", UID: "unchanged-uid",
							Labels: map[string]string{"unbounded-cloud.io/site": sites.serving},
						},
						Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{
							Netboot: &v1alpha3.PXESpec{BootProtocol: v1alpha3.PXEBootProtocolHTTP},
						}},
					}
					foreign := testOperation("old-operation", v1alpha3.OperationHostReplace)
					foreign.CreationTimestamp = fixedNow()
					foreign.Status.Phase = v1alpha3.OperationPhaseInProgress

					foreign.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
						MachineRef: machine.Name, Phase: v1alpha3.OperationPhaseInProgress,
						BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
							Site: sites.old, MachineUID: machine.UID,
							ImageWritten: true, Cleaned: true, Released: true,
							RebootRequestedAt: ptrTo(fixedNow()),
						},
					}}
					if state == "terminal-retained" {
						foreign.Status.Phase = v1alpha3.OperationPhaseFailed
						foreign.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
					}

					if state == "ordinary" {
						foreign.Status.Targets[0].BootMedia.Abandoned = true
					}

					local := testOperation("local-operation", v1alpha3.OperationHostReplace)
					local.CreationTimestamp = metav1.NewTime(fixedNow().Add(time.Hour))
					local.Status.Phase = v1alpha3.OperationPhaseInProgress
					local.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
						MachineRef: machine.Name, Phase: v1alpha3.OperationPhaseInProgress,
					}}
					c := fake.NewClientBuilder().WithScheme(testScheme(t)).
						WithObjects(machine, foreign, local).WithStatusSubresource(foreign, local).Build()
					queue := &StatusQueue{Client: c, Site: sites.serving, Now: fixedNow}

					var before v1alpha3.MachineOperation
					require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(foreign), &before))

					condition := v1alpha3.MachineOperationConditionBootImageWritten

					switch report {
					case "BootImageWritten":
						require.NoError(t, queue.RecordBootImageWritten(t.Context(), machine.Name))
						require.True(t, queue.processNextUpdate(t.Context()))
					case "CloudInitDone":
						condition = v1alpha3.MachineOperationConditionCloudInitDone

						require.NoError(t, queue.RecordCloudInitDone(t.Context(), machine.Name))
						require.True(t, queue.processNextUpdate(t.Context()))
					case "PXEDisabled":
						require.NoError(t, queue.RecordPXEDisabled(t.Context(), machine.Name, "image"))
					}

					var updatedLocal, updatedForeign v1alpha3.MachineOperation
					require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(local), &updatedLocal))
					require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(foreign), &updatedForeign))
					require.Equal(t, before, updatedForeign, "foreign operation must remain untouched")
					require.True(t, apimeta.IsStatusConditionTrue(updatedLocal.Status.Conditions, condition), "local operation must receive %s", condition)
				})
			}
		}
	}
}

func TestStatusQueueRecordsPXEDisabledSynchronously(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 7}}
	op := testOperation("op-pxe-disabled", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(machine, op).Build()
	queue := &StatusQueue{Client: c, Now: fixedNow}

	require.NoError(t, queue.RecordPXEDisabled(context.Background(), machine.Name, "ghcr.io/test/image:v1"))
	require.Len(t, queue.updates, 0)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	bootImage := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, bootImage)
	require.Equal(t, metav1.ConditionTrue, bootImage.Status)
	require.Equal(t, "Machine machine-1 finished writing image ghcr.io/test/image:v1 to disk", bootImage.Message)
}

func TestStatusQueueMediaSiteSelection(t *testing.T) {
	t.Parallel()

	for _, site := range []string{"", "local-site"} {
		for _, state := range []string{"retained", "ordinary", "no-media", "no-targets", "foreign"} {
			t.Run(site+"/"+state, func(t *testing.T) {
				t.Parallel()

				op := testOperation("op-site", v1alpha3.OperationHostReplace)
				op.Spec.MachineRef = "machine-1"
				op.Status.Phase = v1alpha3.OperationPhaseInProgress
				op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
					MachineRef: "machine-1", Phase: v1alpha3.OperationPhaseInProgress,
					BootMedia: &v1alpha3.MachineOperationBootMediaStatus{Site: site},
				}}

				switch state {
				case "retained":
					op.Status.Phase = v1alpha3.OperationPhaseFailed
					op.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
				case "ordinary":
					op.Status.Targets[0].BootMedia.Released = true
				case "no-media":
					op.Status.Targets[0].BootMedia = nil
					op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
						MachineRef: "other-machine", BootMedia: &v1alpha3.MachineOperationBootMediaStatus{Site: "foreign-site"},
					})
				case "no-targets":
					op.Status.Targets = nil
				case "foreign":
					op.Status.Targets[0].BootMedia.Site = "foreign-site"
				}

				c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(op).Build()
				queue := &StatusQueue{Client: c, Site: site}
				selected, ok, err := queue.activeOperationForMachine(t.Context(), "machine-1")
				require.NoError(t, err)

				if state == "foreign" {
					require.False(t, ok)
					require.Nil(t, selected)
				} else {
					require.True(t, ok)
					require.Equal(t, op.Name, selected.Name)
				}
			})
		}
	}
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

func TestStatusQueueCopiesCloudInitProgressToHostReplaceOperation(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 4}}
	op := testOperation("op-cloud-init-progress", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(machine, op).Build()
	queue := &StatusQueue{Client: c, Now: fixedNow}

	require.NoError(t, queue.RecordOperationCondition(context.Background(), machine.Name, metav1.Condition{
		Type:    v1alpha3.MachineOperationConditionCloudInitDone,
		Status:  metav1.ConditionFalse,
		Reason:  "Running",
		Message: "stage \"init\" started",
	}))
	require.True(t, queue.processNextUpdate(context.Background()))

	var updatedOp v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updatedOp))
	cloudInit := apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	require.NotNil(t, cloudInit)
	require.Equal(t, metav1.ConditionFalse, cloudInit.Status)
	require.Equal(t, "Running", cloudInit.Reason)
	require.Equal(t, "stage \"init\" started", cloudInit.Message)

	require.NoError(t, queue.RecordCloudInitDone(context.Background(), machine.Name))
	require.True(t, queue.processNextUpdate(context.Background()))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updatedOp))

	cloudInit = apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	require.NotNil(t, cloudInit)
	require.Equal(t, metav1.ConditionTrue, cloudInit.Status)
	require.Equal(t, "Succeeded", cloudInit.Reason)
}

func TestStatusQueueCopiesCloudInitTimeoutToHostReplaceOperation(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1", Generation: 4}}
	op := testOperation("op-cloud-init-timeout", v1alpha3.OperationHostReplace)
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op).WithStatusSubresource(machine, op).Build()
	queue := &StatusQueue{Client: c, Now: fixedNow}

	require.NoError(t, queue.RecordOperationCondition(context.Background(), machine.Name, metav1.Condition{
		Type:    v1alpha3.MachineOperationConditionCloudInitDone,
		Status:  metav1.ConditionUnknown,
		Reason:  "TimedOut",
		Message: "cloud-init did not complete within 5m0s",
	}))
	require.True(t, queue.processNextUpdate(context.Background()))

	var updatedOp v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updatedOp))
	cloudInit := apimeta.FindStatusCondition(updatedOp.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	require.NotNil(t, cloudInit)
	require.Equal(t, metav1.ConditionUnknown, cloudInit.Status)
	require.Equal(t, "TimedOut", cloudInit.Reason)
	require.Equal(t, "cloud-init did not complete within 5m0s", cloudInit.Message)
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
