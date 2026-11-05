// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	"github.com/Azure/unbounded/internal/machineops"
)

// The API of this release declares only nspawn. A newer CRD, or an agent
// reporting a refused change, can still carry other names, so the drift logic
// treats modes as opaque strings.
const (
	isolationNone      unboundedv1alpha3.IsolationMode = "none"
	isolationContainer unboundedv1alpha3.IsolationMode = "container"
)

// testNow is the clock for condition tests.
var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// syncIso runs syncIsolationConditions at testNow and returns whether it
// changed anything.
func syncIso(m *unboundedv1alpha3.Machine) bool {
	changed, _ := syncIsolationConditions(m, testNow)

	return changed
}

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
	require.False(t, syncIso(m))
	require.Empty(t, m.Status.Conditions)

	m.Spec.Isolation = unboundedv1alpha3.IsolationNSpawn
	require.False(t, syncIso(m))
	require.Empty(t, m.Status.Conditions)
}

func TestSyncIsolationConditions_Drift(t *testing.T) {
	t.Parallel()

	m := isolationMachine(isolationContainer)

	// Applied not reported: no drift condition.
	syncIso(m)
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift))

	// Applied nspawn, declared container: the declaration is pending, and the
	// message says when it takes effect.
	m.Status.Isolation = unboundedv1alpha3.IsolationNSpawn
	require.True(t, syncIso(m))

	c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.NotNil(t, c)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Equal(t, "AppliedModeDiffers", c.Reason)
	require.Contains(t, c.Message, "container")
	require.Contains(t, c.Message, "nspawn")
	require.Contains(t, c.Message, "next repave")
	require.False(t, syncIso(m))

	// The node changed mode at its repave: spec matches.
	m.Status.Isolation = isolationContainer
	require.True(t, syncIso(m))
	c = cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.Equal(t, metav1.ConditionFalse, c.Status)
	require.Equal(t, "AppliedModeMatches", c.Reason)

	// Applied report removed: condition removed.
	m.Status.Isolation = ""
	require.True(t, syncIso(m))
	require.Nil(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift))
}

// An empty spec.isolation declares nothing, whatever the node runs in. A repave
// leaves the node alone, so there is no drift to report.
func TestSyncIsolationConditions_NoDeclarationIsNotDrift(t *testing.T) {
	t.Parallel()

	for _, observed := range []unboundedv1alpha3.IsolationMode{
		unboundedv1alpha3.IsolationNSpawn, isolationNone, isolationContainer,
	} {
		m := isolationMachine("")
		m.Status.Isolation = observed

		require.True(t, syncIso(m), observed)

		c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
		require.NotNil(t, c, observed)
		require.Equal(t, metav1.ConditionFalse, c.Status, observed)
		require.Equal(t, "NoModeDeclared", c.Reason, observed)
		require.False(t, syncIso(m), "idempotent")
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
			name: "failed change", spec: isolationContainer, observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(isolationContainer, unboundedv1alpha3.IsolationChangeFailed, "start container: kubelet not active (rolled back to nspawn)"),
			wantStatus: metav1.ConditionTrue, wantReason: "ChangeFailed", wantInMsg: "rolled back to nspawn",
		},
		{
			name: "change in progress", spec: isolationNone, observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(isolationNone, unboundedv1alpha3.IsolationChangeInProgress, "the host resets"),
			wantStatus: metav1.ConditionTrue, wantReason: "ChangeInProgress", wantInMsg: "none",
		},
		{
			name: "a failure about another mode is stale", spec: isolationContainer, observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(isolationNone, unboundedv1alpha3.IsolationChangeFailed, "old failure"),
			wantStatus: metav1.ConditionTrue, wantReason: "AppliedModeDiffers",
		},
		{
			name: "success reads as a match", spec: isolationContainer, observed: isolationContainer,
			change:     change(isolationContainer, unboundedv1alpha3.IsolationChangeSucceeded, "done"),
			wantStatus: metav1.ConditionFalse, wantReason: "AppliedModeMatches",
		},
		{
			name: "an old failure does not outlive a match", spec: isolationContainer, observed: isolationContainer,
			change:     change(isolationContainer, unboundedv1alpha3.IsolationChangeFailed, "earlier attempt"),
			wantStatus: metav1.ConditionFalse, wantReason: "AppliedModeMatches",
		},
		{
			name: "no declaration ignores a change", spec: "", observed: unboundedv1alpha3.IsolationNSpawn,
			change:     change(isolationContainer, unboundedv1alpha3.IsolationChangeFailed, "boom"),
			wantStatus: metav1.ConditionFalse, wantReason: "NoModeDeclared",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := isolationMachine(tt.spec)
			m.Status.Isolation = tt.observed
			m.Status.IsolationChange = tt.change

			require.True(t, syncIso(m))

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
	machine := isolationMachine(isolationNone)
	machine.Status.Isolation = unboundedv1alpha3.IsolationNSpawn

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).Build()
	reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)

	var got unboundedv1alpha3.Machine
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "iso"}, &got))
	require.Equal(t, isolationNone, got.Spec.Isolation)
	require.Equal(t, metav1.ConditionTrue, cond(&got, unboundedv1alpha3.MachineConditionIsolationDrift).Status)
}

// The agent patches status.isolation at the same time. Losing that race
// is expected, not an error: the reconcile runs again on the fresh object.
func TestMachineReconciler_AStatusConflictRequeuesInsteadOfFailing(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := isolationMachine(isolationNone)
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

	machine := isolationMachine(isolationNone)
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

func TestSyncIsolationConditions_StaleInProgress(t *testing.T) {
	t.Parallel()

	inProgress := func(since time.Time) *unboundedv1alpha3.Machine {
		m := isolationMachine(isolationContainer)
		m.Status.Isolation = unboundedv1alpha3.IsolationNSpawn
		m.Status.IsolationChange = &unboundedv1alpha3.IsolationChangeStatus{
			TargetMode: isolationContainer, Phase: unboundedv1alpha3.IsolationChangeInProgress,
			Message: "building the target", LastTransitionTime: metav1.NewTime(since),
		}

		return m
	}

	// Recent: in progress, with a recheck when it would turn stale.
	m := inProgress(testNow.Add(-30 * time.Minute))
	changed, recheck := syncIsolationConditions(m, testNow)
	require.True(t, changed)
	require.Equal(t, "ChangeInProgress", cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Reason)
	require.Equal(t, isolationChangeStaleAfter-30*time.Minute, recheck)

	// Exactly at the threshold and beyond: stale, no more rechecks.
	for _, age := range []time.Duration{isolationChangeStaleAfter, 3 * isolationChangeStaleAfter} {
		m = inProgress(testNow.Add(-age))
		changed, recheck = syncIsolationConditions(m, testNow)
		require.True(t, changed)

		c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
		require.Equal(t, metav1.ConditionTrue, c.Status)
		require.Equal(t, "ChangeStale", c.Reason)
		require.Contains(t, c.Message, "building the target")
		require.Zero(t, recheck)
	}

	// No transition time: cannot age, so it stays in progress without a
	// recheck.
	m = inProgress(time.Time{})
	_, recheck = syncIsolationConditions(m, testNow)
	require.Equal(t, "ChangeInProgress", cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Reason)
	require.Zero(t, recheck)

	// A stale report about another mode is ignored.
	m = inProgress(testNow.Add(-3 * isolationChangeStaleAfter))
	m.Status.IsolationChange.TargetMode = isolationNone
	_, recheck = syncIsolationConditions(m, testNow)
	require.Equal(t, "AppliedModeDiffers", cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Reason)
	require.Zero(t, recheck)
}

func TestSyncIsolationConditions_RefusedChange(t *testing.T) {
	t.Parallel()

	refused := func(reason string) *unboundedv1alpha3.Machine {
		m := isolationMachine(isolationNone)
		m.Status.Isolation = unboundedv1alpha3.IsolationNSpawn
		m.Status.IsolationChange = &unboundedv1alpha3.IsolationChangeStatus{
			TargetMode: isolationNone, Phase: unboundedv1alpha3.IsolationChangeFailed,
			Strategy: unboundedv1alpha3.IsolationChangeReprovision, Reason: reason, Message: "not allowed",
		}

		return m
	}

	// Without the opt-in the message says how to give it.
	m := refused(unboundedv1alpha3.IsolationChangeReasonReprovisionNotAllowed)
	require.True(t, syncIso(m))

	c := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.Equal(t, metav1.ConditionTrue, c.Status)
	require.Equal(t, "ChangeRefused", c.Reason)
	require.Contains(t, c.Message, unboundedv1alpha3.AnnotationAllowReprovision+"=true")
	require.Contains(t, c.Message, "set the annotation")

	// Once given, the message says the next repave applies it.
	m = refused(unboundedv1alpha3.IsolationChangeReasonReprovisionNotAllowed)
	m.Annotations = map[string]string{unboundedv1alpha3.AnnotationAllowReprovision: "true"}
	require.True(t, syncIso(m))

	c = cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.Equal(t, "ChangeRefused", c.Reason)
	require.Contains(t, c.Message, "is now set")

	// Any other value is not an opt-in.
	m = refused(unboundedv1alpha3.IsolationChangeReasonReprovisionNotAllowed)
	m.Annotations = map[string]string{unboundedv1alpha3.AnnotationAllowReprovision: "yes"}
	require.True(t, syncIso(m))
	require.Contains(t, cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Message, "set the annotation")

	// An agent that cannot run the mode.
	m = refused(unboundedv1alpha3.IsolationChangeReasonUnsupportedMode)
	require.True(t, syncIso(m))

	c = cond(m, unboundedv1alpha3.MachineConditionIsolationDrift)
	require.Equal(t, "ChangeRefused", c.Reason)
	require.Contains(t, c.Message, "spec.agent.version")

	// An unknown reason is an ordinary failure.
	m = refused("SomethingNew")
	require.True(t, syncIso(m))
	require.Equal(t, "ChangeFailed", cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Reason)
}

// The agent's message is bounded by the API, but the condition adds to it. The
// condition message must stay within what the API server accepts.
func TestSyncIsolationConditions_MessageIsBounded(t *testing.T) {
	t.Parallel()

	m := isolationMachine(isolationContainer)
	m.Status.Isolation = unboundedv1alpha3.IsolationNSpawn
	m.Status.IsolationChange = &unboundedv1alpha3.IsolationChangeStatus{
		TargetMode: isolationContainer, Phase: unboundedv1alpha3.IsolationChangeFailed,
		Message: strings.Repeat("\u00e9", machineops.MaxConditionMessageBytes),
	}

	require.True(t, syncIso(m))

	msg := cond(m, unboundedv1alpha3.MachineConditionIsolationDrift).Message
	require.LessOrEqual(t, len(msg), machineops.MaxConditionMessageBytes)
	require.True(t, utf8.ValidString(msg))
}

// An InProgress change must be looked at again when it turns stale, even when
// nothing else asks for a requeue.
func TestMachineReconciler_InProgressChangeRequeuesForStaleness(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := isolationMachine(isolationContainer)
	machine.Status.Isolation = unboundedv1alpha3.IsolationNSpawn
	machine.Status.IsolationChange = &unboundedv1alpha3.IsolationChangeStatus{
		TargetMode: isolationContainer, Phase: unboundedv1alpha3.IsolationChangeInProgress,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).WithStatusSubresource(machine).Build()
	reconciler := &MachineReconciler{Client: c, Scheme: s}

	res, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "iso"}})
	require.NoError(t, err)
	require.Positive(t, res.RequeueAfter)
	require.LessOrEqual(t, res.RequeueAfter, isolationChangeStaleAfter)
}
