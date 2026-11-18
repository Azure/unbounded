// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type staleHostReadClient struct {
	client.Client
	reader client.Reader
}

func (c staleHostReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c staleHostReadClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

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
			assert.Equal(t, 10*time.Second, result.RequeueAfter)

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
	assert.Equal(t, 10*time.Second, result.RequeueAfter)

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
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
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
	host := &fakeNodeOperator{bootID: "boot-1"}
	handler, err := newHostSoftRebootHandler(c, target.reader(), target.log, machine.Name, "test-node", host, &atomic.Bool{})
	require.NoError(t, err)

	result, err := handler.Reconcile(context.Background(), agentdaemon.MachineOperation{Name: "op-soft"})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	assert.Equal(t, "waiting for out-of-band host operation op-oob", getMachineOperation(t, c, "op-soft").Status.Message)
	assert.Zero(t, host.rebootCalls)

	target.apiReader = nil
	handler, err = newHostSoftRebootHandler(c, target.reader(), target.log, machine.Name, "test-node", host, &atomic.Bool{})
	require.NoError(t, err)
	result, err = handler.Reconcile(context.Background(), agentdaemon.MachineOperation{Name: "op-soft"})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Equal(t, 1, host.rebootCalls)
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
	cache := fakeStatusClient(cached, hostSoftRebootOperation("op-soft"))
	apiReader := fakeStatusClient(fresh, outOfBand, hostSoftRebootOperation("op-soft"))
	c := staleHostReadClient{Client: apiReader, reader: cache}
	reconciler := newTestMachinaMachineOperationReconcilerWithLockPath(
		t, c, op, filepath.Join(t.TempDir(), "agent-upgrade.lock"), apiReader,
	)

	result, err := reconciler.ReconcileMachineOperation(context.Background(), "op-soft")
	require.NoError(t, err)
	assert.Zero(t, op.rebootCalls, "the host must not reboot while an out-of-band operation is pending")
	assert.Equal(t, 10*time.Second, result.RequeueAfter)

	waiting := getMachineOperation(t, apiReader, "op-soft")
	assert.Equal(t, v1alpha3.OperationPhasePending, waiting.Status.Phase)
	assert.Equal(t, "waiting for out-of-band host operation op-oob", waiting.Status.Message)
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

// A reprovision handoff defers every MachineOperation before its handler runs,
// so a HostSoftReboot that an out-of-band host operation would also hold is
// left as it was: Pending, with no waiting message, and no requeue, for the
// daemon the handoff installs to pick up.
func TestReconcileHostSoftReboot_AHandoffDefersItBeforeTheOutOfBandCheck(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	outOfBand := outOfBandOperation("op-oob", "test-machine", v1alpha3.OperationHostPowerOff)
	outOfBand.Status.Phase = v1alpha3.OperationPhaseInProgress
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, outOfBand, hostSoftRebootOperation("op-soft"))

	handoff := &handoffState{}
	handoff.set()

	target := &machineOperationTarget{
		installation: installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
		Client:       c,
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
		handoff:      handoff,
	}

	result, err := target.handlers()[v1alpha3.OperationHostSoftReboot](context.Background(), nil, agentdaemon.MachineOperation{Name: "op-soft", Kind: v1alpha3.OperationHostSoftReboot})
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Zero(t, op.rebootCalls)

	soft := getMachineOperation(t, c, "op-soft")
	assert.Empty(t, soft.Status.Phase)
	assert.Empty(t, soft.Status.Message, "the out-of-band check does not run during a handoff")

	// Without the handoff, the out-of-band operation holds it.
	target.handoff = &handoffState{}

	result, err = target.handlers()[v1alpha3.OperationHostSoftReboot](context.Background(), nil, agentdaemon.MachineOperation{Name: "op-soft", Kind: v1alpha3.OperationHostSoftReboot})
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)
	assert.Zero(t, op.rebootCalls)
	assert.Equal(t, "waiting for out-of-band host operation op-oob", getMachineOperation(t, c, "op-soft").Status.Message)
}
