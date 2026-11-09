// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func outOfBandOperation(name, machineRef string, kind v1alpha3.OperationKind) *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    machineRef,
			OperationKind: kind,
		},
	}
}

func setMachineOperationPhase(t *testing.T, c client.Client, name string, phase v1alpha3.OperationPhase) {
	t.Helper()

	op := getMachineOperation(t, c, name)
	op.Status.Phase = phase
	require.NoError(t, c.Status().Update(context.Background(), &op))
}

func TestReconcileHostSoftReboot_WaitsForOutOfBandOperation(t *testing.T) {
	for _, kind := range []v1alpha3.OperationKind{
		v1alpha3.OperationHostHardReboot,
		v1alpha3.OperationHostPowerOff,
		v1alpha3.OperationHostPowerOn,
		v1alpha3.OperationHostReplace,
	} {
		t.Run(string(kind), func(t *testing.T) {
			machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
			outOfBand := outOfBandOperation("op-oob", "test-machine", kind)
			outOfBand.Status.Phase = v1alpha3.OperationPhaseInProgress
			op := &fakeNodeOperator{bootID: "boot-1"}
			c := fakeStatusClient(machine, outOfBand, hostSoftRebootOperation("op-soft"))
			reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

			result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
			require.NoError(t, err)
			assert.Zero(t, op.rebootCalls, "the host must not reboot while an out-of-band operation runs")
			assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)

			waiting := getMachineOperation(t, c, "op-soft")
			assert.Equal(t, v1alpha3.OperationPhasePending, waiting.Status.Phase)
			assert.Equal(t, "waiting for out-of-band host operation op-oob", waiting.Status.Message)
			assert.Empty(t, waiting.Status.Targets)

			setMachineOperationPhase(t, c, "op-oob", v1alpha3.OperationPhaseComplete)

			result, err = reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
			require.NoError(t, err)
			assert.Zero(t, result.RequeueAfter)
			assert.Equal(t, 1, op.rebootCalls)

			started := getMachineOperation(t, c, "op-soft")
			assert.Equal(t, v1alpha3.OperationPhaseInProgress, started.Status.Phase)
		})
	}
}

func TestReconcileHostSoftReboot_WaitsForFailedReplaceMediaCleanup(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	replace := outOfBandOperation("op-oob", "test-machine", v1alpha3.OperationHostReplace)
	replace.Status.Phase = v1alpha3.OperationPhaseFailed
	replace.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: "test-machine",
		Phase:      v1alpha3.OperationPhaseFailed,
		BootMedia:  &v1alpha3.MachineOperationBootMediaStatus{Phase: "Installing", ImageWritten: true},
	}}
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, replace, hostSoftRebootOperation("op-soft"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.NoError(t, err)
	assert.Zero(t, op.rebootCalls, "the host must not reboot while HostReplace boot media is outstanding")
	assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)

	// Media cleanup finishes and the installed OS reports cloud-init.
	current := getMachineOperation(t, c, "op-oob")
	current.Status.Targets[0].BootMedia.Cleaned = true
	current.Status.Targets[0].BootMedia.Released = true
	current.Status.Targets[0].Conditions = []metav1.Condition{{
		Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue,
		Reason: "Done", LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, c.Status().Update(context.Background(), &current))

	result, err = reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Equal(t, 1, op.rebootCalls)
}

func TestReconcileHostSoftReboot_WaitsForNewerOutOfBandOperation(t *testing.T) {
	// Out-of-band operations never wait for a HostSoftReboot, so a newer one
	// may already be running; the HostSoftReboot still waits for it.
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	soft := hostSoftRebootOperation("op-soft")
	soft.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	outOfBand := outOfBandOperation("op-oob", "test-machine", v1alpha3.OperationHostPowerOff)
	outOfBand.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, soft, outOfBand)
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.NoError(t, err)
	assert.Zero(t, op.rebootCalls)
	assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)
}

func TestReconcileHostSoftReboot_WaitingDoesNotRewriteStatus(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	updates := 0
	c := fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(machine, outOfBandOperation("op-oob", "test-machine", v1alpha3.OperationHostHardReboot), hostSoftRebootOperation("op-soft")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				updates++
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		Build()
	op := &fakeNodeOperator{bootID: "boot-1"}
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	for range 3 {
		_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
		require.NoError(t, err)
	}

	assert.Zero(t, op.rebootCalls)
	assert.Equal(t, 1, updates, "the waiting message is written once")
}

func TestReconcileHostSoftReboot_ListFailsDoesNotReboot(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	c := fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(machine, hostSoftRebootOperation("op-soft")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("apiserver unavailable")
			},
		}).
		Build()
	op := &fakeNodeOperator{bootID: "boot-1"}
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.ErrorContains(t, err, "apiserver unavailable")
	assert.Zero(t, op.rebootCalls)

	unchanged := getMachineOperation(t, c, "op-soft")
	assert.Empty(t, unchanged.Status.Phase)
}

func TestReconcileHostSoftReboot_IgnoresUnrelatedOperations(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}

	otherMachine := outOfBandOperation("op-other-machine", "other-machine", v1alpha3.OperationHostHardReboot)
	finished := outOfBandOperation("op-finished", "test-machine", v1alpha3.OperationHostReplace)
	finished.Status.Phase = v1alpha3.OperationPhaseFailed
	nodeReboot := outOfBandOperation("op-node-reboot", "other-machine", v1alpha3.OperationNodeReboot)
	otherSoft := hostSoftRebootOperation("op-other-soft")
	otherSoft.Spec.MachineRef = "other-machine"
	otherSoft.Status.Phase = v1alpha3.OperationPhaseInProgress

	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, otherMachine, finished, nodeReboot, otherSoft, hostSoftRebootOperation("op-soft"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.NoError(t, err)
	assert.Equal(t, 1, op.rebootCalls)
}

func TestReconcileHostSoftReboot_UsesAPIReader(t *testing.T) {
	// The cache may not have seen an out-of-band operation that was just
	// created; the check reads from the API server.
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-soft"))
	apiReader := fakeStatusClient(machine, hostSoftRebootOperation("op-soft"), outOfBandOperation("op-oob", "test-machine", v1alpha3.OperationHostHardReboot))
	target := &machineOperationTarget{Client: c, apiReader: apiReader, log: discardLogger()}

	blocker, err := unfinishedOutOfBandHostOperation(context.Background(), target.reader(), machine)
	require.NoError(t, err)
	assert.Equal(t, "op-oob", blocker)

	target.apiReader = nil
	blocker, err = unfinishedOutOfBandHostOperation(context.Background(), target.reader(), machine)
	require.NoError(t, err)
	assert.Empty(t, blocker)
}

func TestReconcileHostSoftReboot_ReadsMachineFromAPIReader(t *testing.T) {
	// The cached Machine lacks the label that the API server has, so only
	// the fresh Machine matches the pending selector-based operation.
	cached := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	fresh := cached.DeepCopy()
	fresh.Labels = map[string]string{"rack": "a"}
	outOfBand := outOfBandOperation("op-oob", "", v1alpha3.OperationHostPowerOff)
	outOfBand.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}

	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(cached, hostSoftRebootOperation("op-soft"))
	apiReader := fakeStatusClient(fresh, outOfBand)
	reconciler := newTestMachinaMachineOperationReconcilerWithLockPath(
		t, c, op, filepath.Join(t.TempDir(), "agent-upgrade.lock"), apiReader,
	)

	result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.NoError(t, err)
	assert.Zero(t, op.rebootCalls, "the host must not reboot while an out-of-band operation is pending")
	assert.Equal(t, hostSoftRebootWaitInterval, result.RequeueAfter)

	waiting := getMachineOperation(t, c, "op-soft")
	assert.Equal(t, v1alpha3.OperationPhasePending, waiting.Status.Phase)
	assert.Equal(t, "waiting for out-of-band host operation op-oob", waiting.Status.Message)
}

func TestUnfinishedOutOfBandHostOperation_ReturnsOldest(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}
	newer := outOfBandOperation("op-a-newer", "test-machine", v1alpha3.OperationHostPowerOn)
	newer.CreationTimestamp = metav1.NewTime(time.Unix(300, 0))
	older := outOfBandOperation("op-z-older", "test-machine", v1alpha3.OperationHostPowerOff)
	older.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))
	tieB := outOfBandOperation("op-tie-b", "test-machine", v1alpha3.OperationHostHardReboot)
	tieB.CreationTimestamp = metav1.NewTime(time.Unix(50, 0))
	tieA := outOfBandOperation("op-tie-a", "test-machine", v1alpha3.OperationHostHardReboot)
	tieA.CreationTimestamp = metav1.NewTime(time.Unix(50, 0))

	c := fakeStatusClient(newer, older)
	blocker, err := unfinishedOutOfBandHostOperation(context.Background(), c, machine)
	require.NoError(t, err)
	assert.Equal(t, "op-z-older", blocker)

	c = fakeStatusClient(newer, older, tieB, tieA)
	blocker, err = unfinishedOutOfBandHostOperation(context.Background(), c, machine)
	require.NoError(t, err)
	assert.Equal(t, "op-tie-a", blocker, "operations created at the same time are ordered by name")
}

func TestIsOutOfBandHostOperation(t *testing.T) {
	tests := map[v1alpha3.OperationKind]bool{
		v1alpha3.OperationHostHardReboot: true,
		v1alpha3.OperationHostPowerOff:   true,
		v1alpha3.OperationHostPowerOn:    true,
		v1alpha3.OperationHostReplace:    true,
		v1alpha3.OperationHostSoftReboot: false,
		v1alpha3.OperationNodeReboot:     false,
		v1alpha3.OperationAgentUpgrade:   false,
		v1alpha3.OperationAgentReset:     false,
		v1alpha3.OperationHostAllocate:   false,
		v1alpha3.OperationHostRelease:    false,
	}

	for kind, want := range tests {
		assert.Equal(t, want, isOutOfBandHostOperation(kind), kind)
	}
}

func TestReconcileHostSoftReboot_IgnoresAllocationOperations(t *testing.T) {
	for _, kind := range []v1alpha3.OperationKind{v1alpha3.OperationHostAllocate, v1alpha3.OperationHostRelease} {
		for _, phase := range []v1alpha3.OperationPhase{"", v1alpha3.OperationPhaseInProgress} {
			name := string(phase)
			if name == "" {
				name = "NoPhase"
			}

			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
				allocation := outOfBandOperation("op-allocation", "test-machine", kind)
				allocation.Spec.AllocationRef = &v1alpha3.UIDReference{Name: "alloc-1", UID: "alloc-1-uid"}
				allocation.Status.Phase = phase
				op := &fakeNodeOperator{bootID: "boot-1"}
				c := fakeStatusClient(machine, allocation, hostSoftRebootOperation("op-soft"))
				reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

				before := getMachineOperation(t, c, "op-allocation")

				result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
				require.NoError(t, err)
				assert.Zero(t, result.RequeueAfter)
				assert.Equal(t, 1, op.rebootCalls, "an allocation operation must not hold off a HostSoftReboot")

				started := getMachineOperation(t, c, "op-soft")
				assert.Equal(t, v1alpha3.OperationPhaseInProgress, started.Status.Phase)
				assert.NotContains(t, started.Status.Message, "waiting for out-of-band host operation")

				after := getMachineOperation(t, c, "op-allocation")
				assert.Equal(t, before.Status, after.Status)
			})
		}
	}
}

func TestUnfinishedOutOfBandHostOperation_IgnoresAllocationOperations(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}

	for _, kind := range []v1alpha3.OperationKind{v1alpha3.OperationHostAllocate, v1alpha3.OperationHostRelease} {
		t.Run(string(kind), func(t *testing.T) {
			allocation := outOfBandOperation("op-allocation", "test-machine", kind)
			allocation.Spec.AllocationRef = &v1alpha3.UIDReference{Name: "alloc-1", UID: "alloc-1-uid"}
			allocation.CreationTimestamp = metav1.NewTime(time.Unix(100, 0))

			blocker, err := unfinishedOutOfBandHostOperation(context.Background(), fakeStatusClient(allocation), machine)
			require.NoError(t, err)
			assert.Empty(t, blocker)

			powerOff := outOfBandOperation("op-power-off", "test-machine", v1alpha3.OperationHostPowerOff)
			powerOff.CreationTimestamp = metav1.NewTime(time.Unix(200, 0))

			blocker, err = unfinishedOutOfBandHostOperation(context.Background(), fakeStatusClient(allocation, powerOff), machine)
			require.NoError(t, err)
			assert.Equal(t, "op-power-off", blocker)
		})
	}
}

func TestHostOperationUnfinishedOn(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{
		Name:   "test-machine",
		Labels: map[string]string{"unbounded-cloud.io/site": "lab", "rack": "a"},
	}}
	selectorOperation := func(selector *metav1.LabelSelector) *v1alpha3.MachineOperation {
		op := outOfBandOperation("op", "", v1alpha3.OperationHostHardReboot)
		op.Spec.MachineSelector = selector

		return op
	}
	withTargets := func(op *v1alpha3.MachineOperation, targets ...v1alpha3.MachineOperationTargetStatus) *v1alpha3.MachineOperation {
		op.Status.Phase = v1alpha3.OperationPhaseInProgress
		op.Status.Targets = targets

		return op
	}
	siteSelector := &metav1.LabelSelector{MatchLabels: map[string]string{"unbounded-cloud.io/site": "lab"}}
	failedReplace := func(targets ...v1alpha3.MachineOperationTargetStatus) *v1alpha3.MachineOperation {
		op := outOfBandOperation("op", "test-machine", v1alpha3.OperationHostReplace)
		op.Status.Phase = v1alpha3.OperationPhaseFailed
		op.Status.Targets = targets

		return op
	}

	tests := []struct {
		name string
		op   *v1alpha3.MachineOperation
		want bool
	}{
		{name: "machineRef pending", op: outOfBandOperation("op", "test-machine", v1alpha3.OperationHostHardReboot), want: true},
		{name: "machineRef other machine", op: outOfBandOperation("op", "other-machine", v1alpha3.OperationHostHardReboot), want: false},
		{
			name: "operation complete",
			op: func() *v1alpha3.MachineOperation {
				op := outOfBandOperation("op", "test-machine", v1alpha3.OperationHostHardReboot)
				op.Status.Phase = v1alpha3.OperationPhaseComplete

				return op
			}(),
			want: false,
		},
		{name: "selector matches", op: selectorOperation(siteSelector), want: true},
		{name: "selector does not match", op: selectorOperation(&metav1.LabelSelector{MatchLabels: map[string]string{"rack": "b"}}), want: false},
		{
			name: "invalid selector",
			op: selectorOperation(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "rack", Operator: "Bogus",
			}}}),
			want: false,
		},
		{name: "no machineRef or selector", op: outOfBandOperation("op", "", v1alpha3.OperationHostHardReboot), want: false},
		{
			name: "target for machine in progress",
			op: withTargets(selectorOperation(siteSelector),
				v1alpha3.MachineOperationTargetStatus{MachineRef: "test-machine", Phase: v1alpha3.OperationPhaseInProgress}),
			want: true,
		},
		{
			name: "target for machine pending",
			op: withTargets(selectorOperation(siteSelector),
				v1alpha3.MachineOperationTargetStatus{MachineRef: "test-machine"}),
			want: true,
		},
		{
			name: "target for machine finished while others run",
			op: withTargets(selectorOperation(siteSelector),
				v1alpha3.MachineOperationTargetStatus{MachineRef: "test-machine", Phase: v1alpha3.OperationPhaseComplete},
				v1alpha3.MachineOperationTargetStatus{MachineRef: "other-machine", Phase: v1alpha3.OperationPhaseInProgress}),
			want: false,
		},
		{
			name: "target for machine failed",
			op: withTargets(outOfBandOperation("op", "test-machine", v1alpha3.OperationHostReplace),
				v1alpha3.MachineOperationTargetStatus{MachineRef: "test-machine", Phase: v1alpha3.OperationPhaseFailed}),
			want: false,
		},
		{
			name: "failed replace with unreleased media",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "test-machine", Phase: v1alpha3.OperationPhaseFailed,
				BootMedia: &v1alpha3.MachineOperationBootMediaStatus{Phase: "Installing"},
			}),
			want: true,
		},
		{
			name: "failed replace with released media and confirmed boot",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "test-machine", Phase: v1alpha3.OperationPhaseFailed,
				BootMedia:  &v1alpha3.MachineOperationBootMediaStatus{ImageWritten: true, Cleaned: true, Released: true},
				Conditions: []metav1.Condition{{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue}},
			}),
			want: false,
		},
		{
			name: "terminal replace with released media and unconfirmed boot",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "test-machine", Phase: v1alpha3.OperationPhaseFailed,
				BootMedia: &v1alpha3.MachineOperationBootMediaStatus{ImageWritten: true, Cleaned: true, Released: true},
			}),
			want: true,
		},
		{
			name: "outstanding media on another machine",
			op: failedReplace(v1alpha3.MachineOperationTargetStatus{
				MachineRef: "other-machine", Phase: v1alpha3.OperationPhaseFailed,
				BootMedia: &v1alpha3.MachineOperationBootMediaStatus{Phase: "Installing"},
			}),
			want: false,
		},
		{
			name: "targets resolved without machine",
			op: withTargets(selectorOperation(siteSelector),
				v1alpha3.MachineOperationTargetStatus{MachineRef: "other-machine", Phase: v1alpha3.OperationPhaseInProgress}),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hostOperationUnfinishedOn(tt.op, machine))
		})
	}
}

func TestMarkHostSoftRebootWaiting_LeavesStartedOperation(t *testing.T) {
	started := issuedHostSoftRebootOperation("op-soft", "test-machine", "boot-1")
	started.Status.Message = hostSoftRebootMessage
	c := fakeStatusClient(started)

	require.NoError(t, markHostSoftRebootWaiting(context.Background(), c, "op-soft", "waiting for out-of-band host operation op-oob"))

	unchanged := getMachineOperation(t, c, "op-soft")
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, unchanged.Status.Phase)
	assert.Equal(t, hostSoftRebootMessage, unchanged.Status.Message)
}

func TestMarkHostSoftRebootWaiting_IgnoresDeletedOperation(t *testing.T) {
	c := fakeStatusClient()

	require.NoError(t, markHostSoftRebootWaiting(context.Background(), c, "op-soft", "waiting"))
}
