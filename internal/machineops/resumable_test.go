// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const testLongRunningProviderName = "TestResumable"

func TestLongRunningOperationPersistsHandleAndPollsToCompletion(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{
		beginResults: []publicmachineops.BeginResult{{
			Operation: publicmachineops.ProviderOperation{
				OperationID: "provider-op-1",
				ResumeToken: "resume-1",
			},
		}},
		pollResults: []publicmachineops.PollResult{
			{State: publicmachineops.ProviderOperationStateInProgress, Message: "still running"},
			{State: publicmachineops.ProviderOperationStateSucceeded, Message: "provider completed"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	var submitted unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &submitted))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, submitted.Status.Phase)
	require.Len(t, submitted.Status.Targets, 1)
	require.Equal(t, unboundedv1alpha3.OperationStageWaitingProvider, submitted.Status.Targets[0].Stage)
	require.Equal(t, testLongRunningProviderName, submitted.Status.Targets[0].ProviderOperation.Provider)
	require.Equal(t, "provider-op-1", submitted.Status.Targets[0].ProviderOperation.OperationID)
	require.Equal(t, int32(1), submitted.Status.Targets[0].Attempts)
	require.NotNil(t, submitted.Status.Targets[0].LastAttemptAt)

	reconcileOperation(t, reconciler, op.Name)
	reconcileOperation(t, reconciler, op.Name)

	var completed unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &completed))
	require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, completed.Status.Phase)
	require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, completed.Status.Targets[0].Phase)
	require.Equal(t, 1, provider.beginCalls)
	require.Equal(t, []string{"provider-op-1", "provider-op-1"}, provider.pollCalls)
}

func TestLongRunningOperationPollsCurrentMachineProviderID(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	machine.Spec.ProviderID = "test:///nodes/replacement-machine-1"
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		ObservedGeneration: machine.Generation,
		ProviderOperation: &unboundedv1alpha3.ProviderOperationStatus{
			Provider:    testLongRunningProviderName,
			OperationID: "provider-op-1",
		},
	}}
	provider := &recordingLongRunningProvider{
		pollResults: []publicmachineops.PollResult{{State: publicmachineops.ProviderOperationStateInProgress}},
	}

	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	require.Equal(t, []string{machine.Spec.ProviderID}, provider.pollProviderIDs)
}

func TestLongRunningOperationRejectsPersistedHandleFromDifferentProvider(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		ObservedGeneration: machine.Generation,
		ProviderOperation: &unboundedv1alpha3.ProviderOperationStatus{
			Provider:    "DifferentProvider",
			OperationID: "provider-op-1",
		},
	}}
	provider := &recordingLongRunningProvider{}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "DifferentProvider")
	require.Empty(t, provider.pollCalls)
}

func TestLongRunningOperationPollDoesNotRebuildReplacementBootstrapData(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	op.Spec.OperationKind = unboundedv1alpha3.OperationHostReplace
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		ObservedGeneration: machine.Generation,
		ProviderOperation: &unboundedv1alpha3.ProviderOperationStatus{
			Provider:    testLongRunningProviderName,
			OperationID: "provider-op-1",
		},
	}}
	provider := &recordingLongRunningProvider{
		pollResults: []publicmachineops.PollResult{{State: publicmachineops.ProviderOperationStateInProgress}},
	}

	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	require.Equal(t, []string{"provider-op-1"}, provider.pollCalls)
}

func TestLongRunningOperationRecoveryPollsWithoutBeginningAgain(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		ObservedGeneration: machine.Generation,
		ProviderOperation: &unboundedv1alpha3.ProviderOperationStatus{
			Provider:    testLongRunningProviderName,
			OperationID: "provider-op-1",
			ResumeToken: "resume-1",
		},
	}}
	provider := &recordingLongRunningProvider{pollResults: []publicmachineops.PollResult{{State: publicmachineops.ProviderOperationStateSucceeded}}}

	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	require.Zero(t, provider.beginCalls)
	require.Equal(t, []string{"provider-op-1"}, provider.pollCalls)
}

func TestLongRunningOperationRetriesIdempotentBeginUntilHandleIsPersisted(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{
		beginErrs: []error{errors.New("connection reset")},
		beginResults: []publicmachineops.BeginResult{
			{},
			{Operation: publicmachineops.ProviderOperation{OperationID: "provider-op-1"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)
	reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, 2, provider.beginCalls)
	require.Equal(t, []types.UID{op.UID, op.UID}, provider.beginOperationUIDs)
	require.Equal(t, int32(2), updated.Status.Targets[0].Attempts)
	require.Equal(t, "provider-op-1", updated.Status.Targets[0].ProviderOperation.OperationID)
	require.Equal(t, unboundedv1alpha3.OperationStageWaitingProvider, updated.Status.Targets[0].Stage)
}

func TestLongRunningOperationRetriesBeginWhenHandleStatusWriteFails(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{beginResults: []publicmachineops.BeginResult{
		{Operation: publicmachineops.ProviderOperation{OperationID: "provider-op-1"}},
		{Operation: publicmachineops.ProviderOperation{OperationID: "provider-op-1"}},
	}}
	injectedErr := errors.New("injected handle status update failure")
	failHandleWrite := true
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			updated, ok := obj.(*unboundedv1alpha3.MachineOperation)
			if subResourceName == "status" && ok && failHandleWrite && len(updated.Status.Targets) > 0 && updated.Status.Targets[0].ProviderOperation != nil {
				failHandleWrite = false
				return injectedErr
			}

			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}).Build()
	reconciler := newResumableTestReconciler(c, provider)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorIs(t, err, injectedErr)

	var afterFailure unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &afterFailure))
	require.Nil(t, afterFailure.Status.Targets[0].ProviderOperation)

	reconcileOperation(t, reconciler, op.Name)

	var recovered unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &recovered))
	require.Equal(t, 2, provider.beginCalls)
	require.Equal(t, "provider-op-1", recovered.Status.Targets[0].ProviderOperation.OperationID)
}

func TestLongRunningOperationRetriesMalformedIdempotentBeginResult(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{beginResults: []publicmachineops.BeginResult{
		{},
		{Operation: publicmachineops.ProviderOperation{OperationID: "provider-op-1"}},
	}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)
	reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, 2, provider.beginCalls)
	require.Equal(t, int32(2), updated.Status.Targets[0].Attempts)
	require.Equal(t, "provider-op-1", updated.Status.Targets[0].ProviderOperation.OperationID)
}

func TestLongRunningOperationRecordsPermanentBeginFailure(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{beginErrs: []error{&publicmachineops.PermanentError{
		Reason: "InvalidRequest",
		Err:    errors.New("request was invalid"),
	}}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Equal(t, int32(1), updated.Status.Targets[0].Attempts)
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)
	require.Contains(t, updated.Status.Targets[0].Message, "InvalidRequest")
}

func TestLongRunningOperationSanitizesProviderDiagnostics(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{beginErrs: []error{&publicmachineops.PermanentError{
		Reason: "503 HTTP failure!",
		Err:    errors.New(strings.Repeat("é", 20000)),
	}}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))

	completed := apimeta.FindStatusCondition(updated.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, completed)
	require.Empty(t, metav1validation.ValidateCondition(*completed, field.NewPath("status", "conditions")))

	require.LessOrEqual(t, len(updated.Status.Targets[0].Message), maxConditionMessageBytes)
	require.Equal(t, updated.Status.Targets[0].Message, strings.ToValidUTF8(updated.Status.Targets[0].Message, ""))
}

func TestLongRunningOperationRecoversWhenParentStatusWriteAfterPermanentBeginFailureFails(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	provider := &recordingLongRunningProvider{beginErrs: []error{&publicmachineops.PermanentError{
		Reason: "InvalidRequest",
		Err:    errors.New("request was invalid"),
	}}}
	injectedErr := errors.New("injected parent status update failure")
	failNextStatusUpdate := false
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" && failNextStatusUpdate {
				failNextStatusUpdate = false
				return injectedErr
			}

			if err := c.SubResource(subResourceName).Update(ctx, obj, opts...); err != nil {
				return err
			}

			updated, ok := obj.(*unboundedv1alpha3.MachineOperation)
			if !ok || len(updated.Status.Targets) == 0 {
				return nil
			}

			if updated.Status.Targets[0].Phase == unboundedv1alpha3.OperationPhaseFailed {
				failNextStatusUpdate = true
			}

			return nil
		},
	}).Build()
	reconciler := newResumableTestReconciler(c, provider)

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorIs(t, err, injectedErr)

	var afterFailure unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &afterFailure))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, afterFailure.Status.Targets[0].Phase)

	reconcileOperation(t, reconciler, op.Name)

	var recovered unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &recovered))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, recovered.Status.Phase)
	require.Equal(t, 1, provider.beginCalls)
}

func TestLongRunningOperationWaitsForOlderConflictingOperation(t *testing.T) {
	t.Parallel()

	machine, older, credential := newResumableTestObjects("machine-1", "op-a")
	older.CreationTimestamp = metav1.NewTime(fixedOperationNow().Add(-time.Minute))
	older.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	newer := newMachineOperation("op-b", machine.Name, unboundedv1alpha3.OperationHostReboot)
	newer.CreationTimestamp = fixedOperationNow()
	provider := &recordingLongRunningProvider{}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, older, newer, credential).WithStatusSubresource(older, newer).Build()
	reconciler := newResumableTestReconciler(c, provider)

	reconcileOperation(t, reconciler, newer.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: newer.Name}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhasePending, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "waiting for older host operation op-a")
	require.Empty(t, updated.Status.Targets)
	require.Zero(t, provider.beginCalls)
}

func TestMixedOperationStrategiesSerializeConflicts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		olderKind unboundedv1alpha3.OperationKind
		newerKind unboundedv1alpha3.OperationKind
	}{
		{
			name:      "long-running waits for immediate",
			olderKind: unboundedv1alpha3.OperationHostPowerOn,
			newerKind: unboundedv1alpha3.OperationHostReboot,
		},
		{
			name:      "immediate waits for long-running",
			olderKind: unboundedv1alpha3.OperationHostReboot,
			newerKind: unboundedv1alpha3.OperationHostPowerOn,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machine, older, credential := newResumableTestObjects("machine-1", "op-a")
			older.Spec.OperationKind = tt.olderKind
			older.CreationTimestamp = metav1.NewTime(fixedOperationNow().Add(-time.Minute))
			older.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
			newer := newMachineOperation("op-b", machine.Name, tt.newerKind)
			newer.CreationTimestamp = fixedOperationNow()

			immediateCalls := 0
			longRunning := &recordingLongRunningProvider{beginResults: []publicmachineops.BeginResult{{
				Operation: publicmachineops.ProviderOperation{OperationID: "provider-op-1"},
			}}}
			registration, err := publicmachineops.NewProvider(
				testLongRunningProviderName,
				publicmachineops.WithImmediateOperation(
					unboundedv1alpha3.OperationHostPowerOn,
					func(context.Context, publicmachineops.OperationRequest) (publicmachineops.OperationResult, error) {
						immediateCalls++

						return publicmachineops.OperationResult{}, nil
					},
				),
				publicmachineops.WithLongRunningOperation(
					unboundedv1alpha3.OperationHostReboot,
					longRunning.Begin,
					longRunning.Poll,
				),
			)
			require.NoError(t, err)

			c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, older, newer, credential).WithStatusSubresource(older, newer).Build()
			reconciler := &MachineOperationReconciler{
				Client:                      c,
				Providers:                   []*Provider{registration},
				ProviderPollInterval:        time.Millisecond,
				ProviderStallAfter:          time.Hour,
				ProviderStalledPollInterval: 5 * time.Minute,
				Now:                         fixedOperationNow,
			}

			reconcileOperation(t, reconciler, newer.Name)

			var updated unboundedv1alpha3.MachineOperation
			require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: newer.Name}, &updated))
			require.Equal(t, unboundedv1alpha3.OperationPhasePending, updated.Status.Phase)
			require.Contains(t, updated.Status.Message, "waiting for older host operation op-a")
			require.Zero(t, immediateCalls)
			require.Zero(t, longRunning.beginCalls)
		})
	}
}

func TestImmediateConflictWaitPreservesUnknownInProgressOutcome(t *testing.T) {
	t.Parallel()

	machine, older, credential := newResumableTestObjects("machine-1", "op-a")
	older.Spec.OperationKind = unboundedv1alpha3.OperationHostReboot
	older.CreationTimestamp = metav1.NewTime(fixedOperationNow().Add(-time.Minute))
	older.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	current := newMachineOperation("op-b", machine.Name, unboundedv1alpha3.OperationHostPowerOn)
	current.CreationTimestamp = fixedOperationNow()
	current.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress

	immediateCalls := 0
	longRunning := &recordingLongRunningProvider{}
	registration, err := publicmachineops.NewProvider(
		testLongRunningProviderName,
		publicmachineops.WithImmediateOperation(
			unboundedv1alpha3.OperationHostPowerOn,
			func(context.Context, publicmachineops.OperationRequest) (publicmachineops.OperationResult, error) {
				immediateCalls++

				return publicmachineops.OperationResult{}, nil
			},
		),
		publicmachineops.WithLongRunningOperation(
			unboundedv1alpha3.OperationHostReboot,
			longRunning.Begin,
			longRunning.Poll,
		),
	)
	require.NoError(t, err)

	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, older, current, credential).WithStatusSubresource(older, current).Build()
	reconciler := &MachineOperationReconciler{
		Client:               c,
		Providers:            []*Provider{registration},
		ProviderPollInterval: time.Millisecond,
		Now:                  fixedOperationNow,
	}

	reconcileOperation(t, reconciler, current.Name)

	var waiting unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: current.Name}, &waiting))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, waiting.Status.Phase)
	require.Zero(t, immediateCalls)

	var completedOlder unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: older.Name}, &completedOlder))
	completedOlder.Status.Phase = unboundedv1alpha3.OperationPhaseComplete
	require.NoError(t, c.Status().Update(context.Background(), &completedOlder))

	reconcileOperation(t, reconciler, current.Name)
	require.Zero(t, immediateCalls)
}

func TestLongRunningOperationStalledOperationRemainsInProgress(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	startedAt := metav1.NewTime(fixedOperationNow().Add(-2 * time.Hour))
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		StartedAt:          &startedAt,
		ObservedGeneration: machine.Generation,
		ProviderOperation:  &unboundedv1alpha3.ProviderOperationStatus{Provider: testLongRunningProviderName, OperationID: "provider-op-1"},
		LastAttemptAt:      &startedAt,
	}}
	provider := &recordingLongRunningProvider{pollResults: []publicmachineops.PollResult{{State: publicmachineops.ProviderOperationStateInProgress}}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	result := reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, 5*time.Minute, result.RequeueAfter)

	stalled := apimeta.FindStatusCondition(updated.Status.Targets[0].Conditions, unboundedv1alpha3.MachineOperationConditionProviderOperationStalled)
	require.NotNil(t, stalled)
	require.Equal(t, metav1.ConditionTrue, stalled.Status)
}

func TestLongRunningOperationStallTimerStartsAtProviderAcceptance(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	targetStartedAt := metav1.NewTime(fixedOperationNow().Add(-2 * time.Hour))
	acceptedAt := metav1.NewTime(fixedOperationNow().Add(-time.Minute))
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		StartedAt:          &targetStartedAt,
		ObservedGeneration: machine.Generation,
		ProviderOperation:  &unboundedv1alpha3.ProviderOperationStatus{Provider: testLongRunningProviderName, OperationID: "provider-op-1"},
		LastAttemptAt:      &acceptedAt,
	}}
	provider := &recordingLongRunningProvider{pollResults: []publicmachineops.PollResult{{State: publicmachineops.ProviderOperationStateInProgress}}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	result := reconcileOperation(t, reconciler, op.Name)

	require.Equal(t, time.Millisecond, result.RequeueAfter)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	stalled := apimeta.FindStatusCondition(updated.Status.Targets[0].Conditions, unboundedv1alpha3.MachineOperationConditionProviderOperationStalled)
	require.NotNil(t, stalled)
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
}

func TestLongRunningOperationPollFailureDoesNotPrematurelyMarkOperationStalled(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	startedAt := metav1.NewTime(fixedOperationNow().Add(-time.Minute))
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		StartedAt:          &startedAt,
		ObservedGeneration: machine.Generation,
		ProviderOperation:  &unboundedv1alpha3.ProviderOperationStatus{Provider: testLongRunningProviderName, OperationID: "provider-op-1"},
		LastAttemptAt:      &startedAt,
	}}
	provider := &recordingLongRunningProvider{pollErrs: []error{errors.New("temporary poll failure")}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	result := reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, time.Millisecond, result.RequeueAfter)

	stalled := apimeta.FindStatusCondition(updated.Status.Targets[0].Conditions, unboundedv1alpha3.MachineOperationConditionProviderOperationStalled)
	require.NotNil(t, stalled)
	require.Equal(t, metav1.ConditionFalse, stalled.Status)
}

func TestLongRunningOperationPermanentPollFailureFailsOperation(t *testing.T) {
	t.Parallel()

	machine, op, credential := newResumableTestObjects("machine-1", "op-1")
	startedAt := metav1.NewTime(fixedOperationNow().Add(-time.Minute))
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              unboundedv1alpha3.OperationPhaseInProgress,
		Stage:              unboundedv1alpha3.OperationStageWaitingProvider,
		StartedAt:          &startedAt,
		ObservedGeneration: machine.Generation,
		ProviderOperation:  &unboundedv1alpha3.ProviderOperationStatus{Provider: testLongRunningProviderName, OperationID: "provider-op-1"},
		LastAttemptAt:      &startedAt,
	}}
	provider := &recordingLongRunningProvider{pollErrs: []error{fmt.Errorf("provider context: %w", &publicmachineops.PermanentError{Err: errors.New("persisted handle is invalid")})}}
	c := fake.NewClientBuilder().WithScheme(newOperationTestScheme(t)).WithObjects(machine, op, credential).WithStatusSubresource(op).Build()
	reconciler := newResumableTestReconciler(c, provider)

	result := reconcileOperation(t, reconciler, op.Name)

	var updated unboundedv1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, ctrl.Result{}, result)
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)
	require.Contains(t, updated.Status.Message, "persisted handle is invalid")
	require.NotNil(t, updated.Status.Targets[0].ProviderOperation)
}

func newResumableTestObjects(machineName, operationName string) (*unboundedv1alpha3.Machine, *unboundedv1alpha3.MachineOperation, *unboundedv1alpha3.MachineOperationCredential) {
	machine := newExternalMachine(machineName, testLongRunningProviderName)
	machine.Spec.ProviderID = "test:///nodes/" + machineName
	machine.Generation = 4
	op := newMachineOperation(operationName, machineName, unboundedv1alpha3.OperationHostReboot)
	op.UID = types.UID(operationName + "-uid")
	credential := newWorkloadIdentityCredential("test-credential", "site-a", testLongRunningProviderName)

	return machine, op, credential
}

func newResumableTestReconciler(c client.Client, provider *recordingLongRunningProvider) *MachineOperationReconciler {
	registration, err := publicmachineops.NewProvider(
		testLongRunningProviderName,
		publicmachineops.WithLongRunningOperation(
			unboundedv1alpha3.OperationHostReboot,
			provider.Begin,
			provider.Poll,
		),
		publicmachineops.WithLongRunningOperation(
			unboundedv1alpha3.OperationHostReplace,
			provider.Begin,
			provider.Poll,
			publicmachineops.RequiresReplaceUserData(),
		),
	)
	if err != nil {
		panic(err)
	}

	return &MachineOperationReconciler{
		Client:                      c,
		Providers:                   []*Provider{registration},
		ProviderPollInterval:        time.Millisecond,
		ProviderStallAfter:          time.Hour,
		ProviderStalledPollInterval: 5 * time.Minute,
		Now:                         fixedOperationNow,
	}
}

func reconcileOperation(t *testing.T, reconciler *MachineOperationReconciler, name string) ctrl.Result {
	t.Helper()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err)

	return result
}

type recordingLongRunningProvider struct {
	beginResults       []publicmachineops.BeginResult
	beginErrs          []error
	beginOperationUIDs []types.UID
	pollResults        []publicmachineops.PollResult
	pollErrs           []error
	beginCalls         int
	pollCalls          []string
	pollProviderIDs    []string
}

func (p *recordingLongRunningProvider) Begin(_ context.Context, request OperationRequest) (publicmachineops.BeginResult, error) {
	call := p.beginCalls

	p.beginCalls++

	p.beginOperationUIDs = append(p.beginOperationUIDs, request.OperationUID)
	if call < len(p.beginErrs) && p.beginErrs[call] != nil {
		return publicmachineops.BeginResult{}, p.beginErrs[call]
	}

	if call < len(p.beginResults) {
		return p.beginResults[call], nil
	}

	return publicmachineops.BeginResult{}, nil
}

func (p *recordingLongRunningProvider) Poll(_ context.Context, request OperationRequest, operation publicmachineops.ProviderOperation) (publicmachineops.PollResult, error) {
	call := len(p.pollCalls)

	p.pollCalls = append(p.pollCalls, operation.OperationID)

	p.pollProviderIDs = append(p.pollProviderIDs, request.ProviderID)
	if call < len(p.pollErrs) && p.pollErrs[call] != nil {
		return publicmachineops.PollResult{}, p.pollErrs[call]
	}

	if call < len(p.pollResults) {
		return p.pollResults[call], nil
	}

	return publicmachineops.PollResult{State: publicmachineops.ProviderOperationStateInProgress}, nil
}
