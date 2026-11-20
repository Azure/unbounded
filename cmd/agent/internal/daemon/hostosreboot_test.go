// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

func hostSoftRebootOperation(name string) *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationHostSoftReboot,
		},
	}
}

func getMachineOperation(t *testing.T, c client.Client, name string) v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &op))

	return op
}

func TestReconcileHostOSReboot_CompletesAfterReboot(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.Equal(t, 1, op.rebootCalls)

	updated := getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	assert.Equal(t, int64(4), updated.Status.ObservedMachineGeneration)
	assert.Nil(t, updated.Status.CompletedAt)
	require.Len(t, updated.Status.Targets, 1)
	assert.Equal(t, "test-machine", updated.Status.Targets[0].MachineRef)
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Targets[0].Phase)
	assert.Equal(t, "boot-1", updated.Status.Targets[0].BootID)

	// The daemon that starts after the reboot observes a new boot ID.
	handler, err := agentdaemon.NewHostSoftRebootHandler(c, c, discardLogger(), hostSoftRebootTarget{machineName: "test-machine"}, &fakeNodeOperator{bootID: "boot-2"}, &atomic.Bool{}, false)
	require.NoError(t, err)
	require.NoError(t, handler.PublishStartup(context.Background()))

	updated = getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "HostSoftReboot completed", updated.Status.Message)
	assert.Equal(t, int64(4), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.CompletedAt)
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Targets[0].Phase)
	assert.NotNil(t, updated.Status.Targets[0].CompletedAt)
}

func TestReconcileHostOSReboot_RebootsOnlyOnce(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"), hostSoftRebootOperation("op-2"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-2")
	require.NoError(t, err)
	assert.Equal(t, 1, op.rebootCalls)

	second := getMachineOperation(t, c, "op-2")
	assert.Equal(t, v1alpha3.OperationPhasePending, second.Status.Phase, "the daemon after the reboot picks up op-2")
}

func TestReconcileHostOSReboot_RebootCommandFails(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1", rebootErr: errors.New("systemctl reboot: denied")}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"), hostSoftRebootOperation("op-2"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	updated := getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "systemctl reboot: denied", updated.Status.Message)
	require.Len(t, updated.Status.Targets, 1)
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)

	// A failed reboot does not block later operations, and the next daemon
	// does not report the failed one again.
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-2")
	require.NoError(t, err)
	assert.Equal(t, 2, op.rebootCalls)

	handler, err := agentdaemon.NewHostSoftRebootHandler(c, c, discardLogger(), hostSoftRebootTarget{machineName: "test-machine"}, &fakeNodeOperator{bootID: "boot-2"}, &atomic.Bool{}, false)
	require.NoError(t, err)
	require.NoError(t, handler.PublishStartup(context.Background()))
	updated = getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
}

func TestReconcileHostOSReboot_StatusWriteFailsDoesNotReboot(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(machine, hostSoftRebootOperation("op-1")).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return errors.New("apiserver unavailable")
			},
		}).
		Build()
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.Error(t, err)
	assert.Zero(t, op.rebootCalls, "the host must not reboot before the boot ID is recorded")
}

func TestReconcileHostOSReboot_DefersOtherOperations(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	nodeReboot := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-node-reboot"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}
	op := &fakeNodeOperator{bootID: "boot-1", active: &ActiveMachine{Name: "kube1"}}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"), nodeReboot)
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-node-reboot")
	require.NoError(t, err)
	assert.False(t, op.restartCalled, "a NodeReboot must not start while the host is going down")

	deferred := getMachineOperation(t, c, "op-node-reboot")
	assert.Equal(t, v1alpha3.OperationPhasePending, deferred.Status.Phase, "the daemon after the reboot picks up the NodeReboot")
}

func TestReconcileHostOSReboot_DaemonStoppingLeavesInProgress(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	ctx, cancel := context.WithCancel(context.Background())
	op := &fakeNodeOperator{bootID: "boot-1", rebootErr: errors.New("signal: terminated"), rebootDuring: cancel}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(ctx, "op-1")
	require.NoError(t, err)

	updated := getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase, "the next daemon decides from the boot ID")

	// The reboot happened after all.
	handler, err := agentdaemon.NewHostSoftRebootHandler(c, c, discardLogger(), hostSoftRebootTarget{machineName: "test-machine"}, &fakeNodeOperator{bootID: "boot-2"}, &atomic.Bool{}, false)
	require.NoError(t, err)
	require.NoError(t, handler.PublishStartup(context.Background()))

	updated = getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
}

func TestReconcileHostOSReboot_BootIDUnavailable(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootIDErr: errors.New("read boot ID: missing")}
	c := fakeStatusClient(machine, hostSoftRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.Zero(t, op.rebootCalls)

	updated := getMachineOperation(t, c, "op-1")
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "read boot ID: missing", updated.Status.Message)
}
