// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	shared "github.com/Azure/unbounded/pkg/agent/daemon"
)

func hostRebootOperation() *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "host-reboot"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationHostOSReboot,
		},
	}
}

func TestReconcileHostOSReboot(t *testing.T) {
	t.Parallel()

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	op := hostRebootOperation()
	c := fakeStatusClient(machine, op)
	operator := &fakeNodeOperator{hostBootID: "boot-before"}
	operator.hostRebootDuring = func() {
		var persisted v1alpha3.MachineOperation
		require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), &persisted))
		assert.Equal(t, "boot-before", persisted.Status.HostBootID)
		assert.Equal(t, v1alpha3.OperationPhaseInProgress, persisted.Status.Phase)
		assert.Equal(t, int64(7), persisted.Status.ObservedMachineGeneration)
		require.NotNil(t, persisted.Status.StartedAt)
		assert.Nil(t, persisted.Status.CompletedAt)
	}
	r := newTestMachinaMachineOperationReconciler(t, c, operator)
	result, err := r.ReconcileMachineOperation(t.Context(), op.Name)
	require.NoError(t, err)
	assert.Equal(t, hostRebootPollInterval, result.RequeueAfter)
	assert.Equal(t, 1, operator.hostRebootCalls)

	// A fresh daemon on the same host boot must wait, not reboot again.
	operator = &fakeNodeOperator{hostBootID: "boot-before"}
	r = newTestMachinaMachineOperationReconciler(t, c, operator)
	result, err = r.ReconcileMachineOperation(t.Context(), op.Name)
	require.NoError(t, err)
	assert.Equal(t, hostRebootPollInterval, result.RequeueAfter)
	assert.Zero(t, operator.hostRebootCalls)

	machine.Generation = 8
	require.NoError(t, c.Update(t.Context(), machine))

	operator = &fakeNodeOperator{hostBootID: "boot-after"}
	r = newTestMachinaMachineOperationReconciler(t, c, operator)
	result, err = r.ReconcileMachineOperation(t.Context(), op.Name)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	assert.Zero(t, operator.hostRebootCalls)

	var completed v1alpha3.MachineOperation
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), &completed))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Phase)
	assert.Equal(t, "HostOSReboot completed", completed.Status.Message)
	assert.Equal(t, int64(7), completed.Status.ObservedMachineGeneration)
	assert.NotNil(t, completed.Status.CompletedAt)
	assert.True(t, apimeta.IsStatusConditionTrue(completed.Status.Conditions, v1alpha3.MachineOperationConditionCompleted))

	_, err = r.ReconcileMachineOperation(t.Context(), op.Name)
	require.NoError(t, err)
	assert.Zero(t, operator.hostRebootCalls)
}

func TestReconcileHostOSRebootFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		bootErr     error
		rebootErr   error
		statusErr   bool
		pruneBootID bool
		wantPhase   v1alpha3.OperationPhase
		wantCalls   int
	}{
		{name: "boot ID unavailable", bootErr: errors.New("boot ID unavailable")},
		{name: "status write failed", statusErr: true},
		{name: "old CRD prunes boot ID", pruneBootID: true, wantPhase: v1alpha3.OperationPhaseInProgress},
		{name: "reboot command failed", rebootErr: errors.New("reboot denied"), wantPhase: v1alpha3.OperationPhaseFailed, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			op := hostRebootOperation()
			machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}

			c := fakeStatusClient(machine, op)
			if tc.statusErr || tc.pruneBootID {
				c = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						if tc.statusErr {
							return errors.New("status unavailable")
						}

						obj.(*v1alpha3.MachineOperation).Status.HostBootID = ""

						return c.SubResource(name).Update(ctx, obj, opts...)
					},
				})
			}

			operator := &fakeNodeOperator{hostBootID: "boot-before", hostBootIDErr: tc.bootErr, hostRebootErr: tc.rebootErr}
			r := newTestMachinaMachineOperationReconciler(t, c, operator)

			_, err := r.ReconcileMachineOperation(t.Context(), op.Name)
			if tc.bootErr != nil || tc.statusErr || tc.pruneBootID {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tc.wantCalls, operator.hostRebootCalls)

			var updated v1alpha3.MachineOperation
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), &updated))
			assert.Equal(t, tc.wantPhase, updated.Status.Phase)

			if tc.rebootErr != nil {
				assert.Contains(t, updated.Status.Message, tc.rebootErr.Error())
				assert.NotNil(t, updated.Status.CompletedAt)
			}
		})
	}
}

func TestReconcileHostOSRebootUsesUncachedState(t *testing.T) {
	t.Parallel()

	op := hostRebootOperation()
	cached := fakeStatusClient(op)
	op.Status = v1alpha3.MachineOperationStatus{
		Phase: v1alpha3.OperationPhaseInProgress, HostBootID: "boot-before",
		StartedAt: &metav1.Time{Time: time.Now()},
	}
	reader := fakeStatusClient(op)
	operator := &fakeNodeOperator{hostBootID: "boot-before"}
	dir := t.TempDir()
	target := &machineOperationTarget{
		Client: cached, operationReader: reader, nodeOperator: operator,
		machineName: "test-machine", log: discardLogger(),
		installation: installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock")),
	}
	result, err := target.reconcileHostOSReboot(t.Context(), nil, shared.MachineOperation{Name: op.Name})
	require.NoError(t, err)
	assert.Equal(t, hostRebootPollInterval, result.RequeueAfter)
	assert.Zero(t, operator.hostRebootCalls)
}

func TestReconcileHostOSRebootRequiresLocalMachineRef(t *testing.T) {
	t.Parallel()

	for _, machineRef := range []string{"", "another-machine"} {
		t.Run(machineRef, func(t *testing.T) {
			t.Parallel()

			op := hostRebootOperation()
			op.Spec.MachineRef = machineRef
			c := fakeStatusClient(op)
			operator := &fakeNodeOperator{hostBootID: "boot-before"}
			r := newTestMachinaMachineOperationReconciler(t, c, operator)
			_, err := r.ReconcileMachineOperation(t.Context(), op.Name)
			require.NoError(t, err)
			assert.Zero(t, operator.hostRebootCalls)

			var updated v1alpha3.MachineOperation
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), &updated))

			if machineRef == "" {
				assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
			} else {
				assert.Empty(t, updated.Status.Phase)
			}
		})
	}
}

func TestReconcileHostOSRebootTimeout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		startedAt *metav1.Time
	}{
		{name: "expired", startedAt: &metav1.Time{Time: time.Now().Add(-hostRebootTimeout)}},
		{name: "missing start time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			op := hostRebootOperation()
			op.Status = v1alpha3.MachineOperationStatus{
				Phase: v1alpha3.OperationPhaseInProgress, HostBootID: "boot-before", StartedAt: tc.startedAt,
			}
			c := fakeStatusClient(op)
			operator := &fakeNodeOperator{hostBootID: "boot-before"}
			r := newTestMachinaMachineOperationReconciler(t, c, operator)
			_, err := r.ReconcileMachineOperation(t.Context(), op.Name)
			require.NoError(t, err)
			assert.Zero(t, operator.hostRebootCalls)

			var updated v1alpha3.MachineOperation
			require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), &updated))
			assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
			condition := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionCompleted)
			require.NotNil(t, condition)
			assert.Equal(t, "RebootTimeout", condition.Reason)
		})
	}
}

func TestReconcileHostOSRebootRetriesReporting(t *testing.T) {
	t.Parallel()

	op := hostRebootOperation()
	op.Status = v1alpha3.MachineOperationStatus{
		Phase: v1alpha3.OperationPhaseInProgress, HostBootID: "boot-before",
	}
	c := fakeStatusClient(op)
	failUpdate := true
	failingClient := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failUpdate {
				return errors.New("API unavailable")
			}

			return c.SubResource(name).Update(ctx, obj, opts...)
		},
	})
	operator := &fakeNodeOperator{hostBootID: "boot-after"}
	r := newTestMachinaMachineOperationReconciler(t, failingClient, operator)
	_, err := r.ReconcileMachineOperation(t.Context(), op.Name)
	require.Error(t, err)

	failUpdate = false
	_, err = r.ReconcileMachineOperation(t.Context(), op.Name)
	require.NoError(t, err)
	assert.Zero(t, operator.hostRebootCalls)

	var completed v1alpha3.MachineOperation
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(op), &completed))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Phase)
}

func TestNspawnNodeOperatorRebootHost(t *testing.T) {
	for _, tc := range []struct {
		name      string
		script    string
		wantError bool
	}{
		{name: "success", script: "#!/bin/sh\n[ \"$#\" -eq 0 ]\n"},
		{name: "failure", script: "#!/bin/sh\nexit 1\n", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "reboot"), []byte(tc.script), 0o700))
			t.Setenv("PATH", dir)

			err := (nspawnNodeOperator{}).RebootHost(t.Context(), discardLogger())
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
