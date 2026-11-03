// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func isolationMachine(mode unboundedv1alpha3.IsolationMode) *unboundedv1alpha3.Machine {
	m := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "iso"},
		Spec:       unboundedv1alpha3.MachineSpec{Isolation: mode},
	}

	return m
}

func cond(m *unboundedv1alpha3.Machine, t string) *metav1.Condition {
	return apimeta.FindStatusCondition(m.Status.Conditions, t)
}

func TestSyncIsolationConditions_NoFieldNoConditions(t *testing.T) {
	t.Parallel()

	m := isolationMachine("")
	require.False(t, syncIsolationConditions(m))
	require.Empty(t, m.Status.Conditions)

	m.Spec.Isolation = unboundedv1alpha3.IsolationNSpawn
	require.False(t, syncIsolationConditions(m))
	require.Empty(t, m.Status.Conditions)
}

func TestSyncIsolationConditions_Drift(t *testing.T) {
	t.Parallel()

	m := isolationMachine(unboundedv1alpha3.IsolationContainer)

	// Applied not reported: no drift condition.
	syncIsolationConditions(m)
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift))

	// Applied nspawn, declared container: the declaration is pending, and the
	// message says when it takes effect.
	m.Status.Isolation = unboundedv1alpha3.IsolationNSpawn
	require.True(t, syncIsolationConditions(m))

	c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Equal(t, "AppliedModeDiffers", c.Reason)
	require.Contains(t, c.Message, "container")
	require.Contains(t, c.Message, "nspawn")
	require.Contains(t, c.Message, "next repave")
	require.False(t, syncIsolationConditions(m))

	// The node changed mode at its repave: spec matches.
	m.Status.Isolation = unboundedv1alpha3.IsolationContainer
	require.True(t, syncIsolationConditions(m))
	c = cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, "AppliedModeMatches", c.Reason)

	// Applied report removed: condition removed.
	m.Status.Isolation = ""
	require.True(t, syncIsolationConditions(m))
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift))
}

// An empty spec.isolation declares nothing, whatever the node runs in. A repave
// leaves the node alone, so there is no drift to report.
func TestSyncIsolationConditions_NoDeclarationIsNotDrift(t *testing.T) {
	t.Parallel()

	for _, observed := range []unboundedv1alpha3.IsolationMode{
		unboundedv1alpha3.IsolationNSpawn, unboundedv1alpha3.IsolationNone, unboundedv1alpha3.IsolationContainer,
	} {
		m := isolationMachine("")
		m.Status.Isolation = observed

		require.True(t, syncIsolationConditions(m), observed)

		c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
		require.NotNil(t, c, observed)
		require.Equal(t, metav1.ConditionFalse, c.Status, observed)
		require.Equal(t, "NoModeDeclared", c.Reason, observed)
		require.False(t, syncIsolationConditions(m), "idempotent")
	}
}

// The agent reports the latest attempt in status.isolationChange. It changes
// the condition only while it is about the mode the spec declares.
func TestSyncIsolationConditions_ChangeStatus(t *testing.T) {
	t.Parallel()

	change := func(target unboundedv1alpha3.IsolationMode, phase unboundedv1alpha3.IsolationChangePhase, msg string) *unboundedv1alpha3.IsolationChangeStatus {
		return &unboundedv1alpha3.IsolationChangeStatus{TargetMode: target, Phase: phase, Message: msg}
	}

	for _, tt := range []struct {
		name       string
		spec       unboundedv1alpha3.IsolationMode
		observed   unboundedv1alpha3.IsolationMode
		change     *unboundedv1alpha3.IsolationChangeStatus
		wantStatus metav1.ConditionStatus
		wantReason string
		wantInMsg  string
	}{
		{
			name: "failed change", spec: unboundedv1alpha3.IsolationContainer, observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(unboundedv1alpha3.IsolationContainer, unboundedv1alpha3.IsolationChangeFailed, "start container: kubelet not active (rolled back to nspawn)"),
			wantStatus: metav1.ConditionTrue, wantReason: "ChangeFailed", wantInMsg: "rolled back to nspawn",
		},
		{
			name: "change in progress", spec: unboundedv1alpha3.IsolationNone, observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(unboundedv1alpha3.IsolationNone, unboundedv1alpha3.IsolationChangeInProgress, "the host resets"),
			wantStatus: metav1.ConditionTrue, wantReason: "ChangeInProgress", wantInMsg: "none",
		},
		{
			name: "a failure about another mode is stale", spec: unboundedv1alpha3.IsolationContainer, observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(unboundedv1alpha3.IsolationNone, unboundedv1alpha3.IsolationChangeFailed, "old failure"),
			wantStatus: metav1.ConditionTrue, wantReason: "AppliedModeDiffers",
		},
		{
			name: "success reads as a match", spec: unboundedv1alpha3.IsolationContainer, observed: unboundedv1alpha3.IsolationContainer,
			change:     change(unboundedv1alpha3.IsolationContainer, unboundedv1alpha3.IsolationChangeSucceeded, "done"),
			wantStatus: metav1.ConditionFalse, wantReason: "AppliedModeMatches",
		},
		{
			name: "an old failure does not outlive a match", spec: unboundedv1alpha3.IsolationContainer, observed: unboundedv1alpha3.IsolationContainer,
			change:     change(unboundedv1alpha3.IsolationContainer, unboundedv1alpha3.IsolationChangeFailed, "earlier attempt"),
			wantStatus: metav1.ConditionFalse, wantReason: "AppliedModeMatches",
		},
		{
			name: "no declaration ignores a change", spec: "", observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(unboundedv1alpha3.IsolationContainer, unboundedv1alpha3.IsolationChangeFailed, "boom"),
			wantStatus: metav1.ConditionFalse, wantReason: "NoModeDeclared",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := isolationMachine(tt.spec)
			m.Status.Isolation = tt.observed
			m.Status.IsolationChange = tt.change

			require.True(t, syncIsolationConditions(m))

			c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
			require.NotNil(t, c)
			require.Equal(t, tt.wantStatus, c.Status)
			require.Equal(t, tt.wantReason, c.Reason)
			require.Contains(t, c.Message, tt.wantInMsg)
		})
	}
}

func TestMachineReconciler_IsolationConditionsPersisted(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	// No SSH config: still gets conditions, and the spec is never changed.
	machine := isolationMachine(unboundedv1alpha3.IsolationNone)
	machine.Status.Isolation = unboundedv1alpha3.IsolationNSpawn

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).Build()
	reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)

	var got unboundedv1alpha3.Machine
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "iso"}, &got))
	require.Equal(t, unboundedv1alpha3.IsolationNone, got.Spec.Isolation)
	require.Equal(t, metav1.ConditionTrue, cond(&got, unboundedv1alpha3.MachineConditionIsolationDrift).Status)
}

// The agent patches status.isolation at the same time. Losing that race
// is expected, not an error: the reconcile runs again on the fresh object.
func TestMachineReconciler_AStatusConflictRequeuesInsteadOfFailing(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := isolationMachine(unboundedv1alpha3.IsolationNone)
	machine.Status.Isolation = unboundedv1alpha3.IsolationNSpawn

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "unbounded-cloud.io", Resource: "machines"}, "iso", nil)
			},
		}).Build()
	reconciler := &MachineReconciler{Client: c, Scheme: s}

	res, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)
	require.Positive(t, res.RequeueAfter, "the reconcile is retried")
}

func TestMachineReconciler_AnotherStatusErrorIsStillReturned(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := isolationMachine(unboundedv1alpha3.IsolationNone)
	machine.Status.Isolation = unboundedv1alpha3.IsolationNSpawn

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewInternalError(context.DeadlineExceeded)
			},
		}).Build()
	reconciler := &MachineReconciler{Client: c, Scheme: s}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.Error(t, err)
}
