// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func setHostOSRebootSignalPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "host-os-reboot-signal")
	t.Setenv(goalstates.EnvDaemonHostOSRebootSignalPath, path)

	return path
}

func hostOSRebootOperation(name string) *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationHostOSReboot,
		},
	}
}

func staticBootID(id string) func() (string, error) {
	return func() (string, error) { return id, nil }
}

func TestReconcileHostOSReboot_CompletesAfterReboot(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, hostOSRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.Equal(t, 1, op.rebootCalls)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	assert.Nil(t, updated.Status.CompletedAt)

	data, err := os.ReadFile(signalPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","observedMachineGeneration":4,"bootID":"boot-1"}`, string(data))

	// The daemon that starts after the reboot observes a new boot ID.
	require.NoError(t, publishAndClearHostOSRebootSignal(context.Background(), discardLogger(), c, staticBootID("boot-2")))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "HostOSReboot completed", updated.Status.Message)
	assert.Equal(t, int64(4), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.CompletedAt)
	assert.NoFileExists(t, signalPath)
}

func TestReconcileHostOSReboot_RebootsOnlyOnce(t *testing.T) {
	setHostOSRebootSignalPath(t)

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1"}
	c := fakeStatusClient(machine, hostOSRebootOperation("op-1"), hostOSRebootOperation("op-2"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-2")
	require.NoError(t, err)
	assert.Equal(t, 1, op.rebootCalls)

	var second v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-2"}, &second))
	assert.Empty(t, second.Status.Phase, "the daemon after the reboot picks up op-2")
}

func TestReconcileHostOSReboot_RebootCommandFails(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootID: "boot-1", rebootErr: errors.New("systemctl reboot: denied")}
	c := fakeStatusClient(machine, hostOSRebootOperation("op-1"), hostOSRebootOperation("op-2"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "systemctl reboot: denied", updated.Status.Message)
	assert.NoFileExists(t, signalPath)

	// A failed reboot does not block later operations.
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-2")
	require.NoError(t, err)
	assert.Equal(t, 2, op.rebootCalls)
}

func TestReconcileHostOSReboot_DefersOtherOperations(t *testing.T) {
	setHostOSRebootSignalPath(t)

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	nodeReboot := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-node-reboot"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}
	op := &fakeNodeOperator{bootID: "boot-1", active: &ActiveMachine{Name: "kube1"}}
	c := fakeStatusClient(machine, hostOSRebootOperation("op-1"), nodeReboot)
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	_, err = reconciler.ReconcileMachineOperation(context.Background(), "op-node-reboot")
	require.NoError(t, err)
	assert.False(t, op.restartCalled, "a NodeReboot must not start while the host is going down")

	var deferred v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-node-reboot"}, &deferred))
	assert.Empty(t, deferred.Status.Phase, "the daemon after the reboot picks up the NodeReboot")
}

func TestReconcileHostOSReboot_DaemonStoppingKeepsSignal(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	ctx, cancel := context.WithCancel(context.Background())
	op := &fakeNodeOperator{bootID: "boot-1", rebootErr: errors.New("signal: terminated"), rebootDuring: cancel}
	c := fakeStatusClient(machine, hostOSRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(ctx, "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	assert.FileExists(t, signalPath, "the next daemon decides from the boot ID")

	// The reboot happened after all.
	require.NoError(t, publishAndClearHostOSRebootSignal(context.Background(), discardLogger(), c, staticBootID("boot-2")))
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
}

func TestReconcileHostOSReboot_BootIDUnavailable(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 4}}
	op := &fakeNodeOperator{bootIDErr: errors.New("read boot ID: missing")}
	c := fakeStatusClient(machine, hostOSRebootOperation("op-1"))
	reconciler := newTestMachinaMachineOperationReconciler(t, c, op)

	_, err := reconciler.ReconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.Zero(t, op.rebootCalls)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "read boot ID: missing", updated.Status.Message)
	assert.NoFileExists(t, signalPath)
}

func TestPublishAndClearHostOSRebootSignal_NoSignal(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)

	require.NoError(t, publishAndClearHostOSRebootSignal(context.Background(), discardLogger(), fakeStatusClient(), staticBootID("boot-1")))
	assert.NoFileExists(t, signalPath)
}

func TestPublishAndClearHostOSRebootSignal_SameBootFails(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)
	machineOp := hostOSRebootOperation("op-1")
	machineOp.Status.Phase = v1alpha3.OperationPhaseInProgress
	c := fakeStatusClient(machineOp)

	require.NoError(t, newHostOSRebootSignalStore().Write(hostOSRebootSignal{OperationName: "op-1", BootID: "boot-1"}))

	require.NoError(t, publishAndClearHostOSRebootSignal(context.Background(), discardLogger(), c, staticBootID("boot-1")))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	condition := apimeta.FindStatusCondition(updated.Status.Conditions, "Completed")
	require.NotNil(t, condition)
	assert.Equal(t, "RebootNotObserved", condition.Reason)
	assert.NoFileExists(t, signalPath)
}

func TestPublishAndClearHostOSRebootSignal_KeepsSignalWhenBootIDUnavailable(t *testing.T) {
	signalPath := setHostOSRebootSignalPath(t)
	machineOp := hostOSRebootOperation("op-1")
	machineOp.Status.Phase = v1alpha3.OperationPhaseInProgress
	c := fakeStatusClient(machineOp)

	require.NoError(t, newHostOSRebootSignalStore().Write(hostOSRebootSignal{OperationName: "op-1", BootID: "boot-1"}))

	err := publishAndClearHostOSRebootSignal(context.Background(), discardLogger(), c, func() (string, error) {
		return "", errors.New("boot ID unavailable")
	})
	require.Error(t, err)
	assert.FileExists(t, signalPath)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
}
