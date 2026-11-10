// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	publicmachineops "github.com/Azure/unbounded/pkg/machineops"
)

const (
	engineTestPollInterval    = 7 * time.Second
	engineTestStalledInterval = 5 * time.Minute
)

func TestAllocationOperationBeginsRecordedOperation(t *testing.T) {
	t.Parallel()

	e := newEngineHarness(t, nil)
	allocation := e.allocation()
	machine := e.machine()
	op := e.operation(allocTestNames.allocateOperation)
	token := e.secret(allocTestNames.tokenSecret)

	result := e.reconcile(allocTestNames.allocateOperation)
	require.Equal(t, ctrl.Result{RequeueAfter: engineTestPollInterval}, result)

	begins, polls, _, _ := e.provider.calls()
	require.Len(t, begins, 1)
	require.Empty(t, polls)

	request := begins[0]
	require.Equal(t, machine.Name, request.MachineName)
	require.Equal(t, machine.UID, request.MachineUID)
	require.Equal(t, op.Name, request.OperationName)
	require.Equal(t, op.UID, request.OperationUID)
	require.Equal(t, "example:///hosts/"+allocTestNames.machine, request.ProviderID)
	require.Equal(t, allocTestHostImage, request.HostImage)
	require.Equal(t, unboundedv1alpha3.OperationHostAllocate, request.Operation)
	require.Empty(t, request.Parameters)
	require.Empty(t, request.ReplaceUserData)
	require.NotNil(t, request.Auth)
	require.Equal(t, unboundedv1alpha3.MachineOperationCredentialAuthWorkloadIdentity, request.Auth.Mode)

	require.NotNil(t, request.Allocation)
	userData := request.Allocation.UserData
	request.Allocation.UserData = ""
	require.Equal(t, &publicmachineops.AllocationRequest{
		Name:           allocTestName,
		UID:            allocTestUID,
		ClassName:      classTestName,
		ClassUID:       "class-a-uid",
		HostName:       allocTestNames.machine,
		ProviderConfig: publicmachineops.ProviderConfig{Data: newAllocationFixture(t).providerConfig.Data},
		Offering:       allocation.Status.Snapshot.Offering,
	}, request.Allocation)

	require.True(t, strings.HasPrefix(userData, "#cloud-config\n"))
	require.Contains(t, userData, `"providerID": "unbounded:///machineallocations/`+string(allocTestUID)+`"`)
	require.Contains(t, userData, string(token.Data["token-id"])+"."+string(token.Data["token-secret"]))
	require.Contains(t, userData, "/var/lib/unbounded/bootstrap/run")
	require.Contains(t, userData, "AGENT_SHA256=")
	require.Contains(t, userData, `"NodeName": "`+allocTestNames.machine+`"`)

	updated := e.operation(allocTestNames.allocateOperation)
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Len(t, updated.Status.Targets, 1)
	require.Equal(t, &unboundedv1alpha3.ProviderOperationStatus{Provider: classTestProvider, OperationID: "allocate-" + string(op.UID)},
		updated.Status.Targets[0].ProviderOperation)
	require.Nil(t, updated.Status.Allocation)
	require.Equal(t, machine.Spec, e.machine().Spec)
}

func TestAllocationOperationWaitsForRecord(t *testing.T) {
	t.Parallel()

	e := newEngineHarness(t, func(h *allocationHarness) {
		h.failStatusWrites(true)
		_, _ = h.reconcileResult()
		h.failStatusWrites(false)
	})
	require.Nil(t, e.allocation().Status.Operations.Allocate)
	require.NotNil(t, e.operation(allocTestNames.allocateOperation))

	e.resetWrites()

	result := e.reconcile(allocTestNames.allocateOperation)
	require.Equal(t, ctrl.Result{RequeueAfter: engineTestPollInterval}, result)
	e.requireNoProviderCalls(t)
	require.Empty(t, e.writes())
}

func TestAllocationOperationFailsUnrecordable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// operation is the operation to reconcile.
		operation string
		prepare   func(*allocationHarness)
		mutate    func(*testing.T, *engineHarness)
	}{
		{
			name:      "another UID recorded",
			operation: allocTestNames.allocateOperation,
			mutate: func(t *testing.T, e *engineHarness) {
				e.updateAllocationStatus(t, func(status *unboundedv1alpha3.MachineAllocationStatus) {
					status.Operations.Allocate.UID = "another-uid"
				})
			},
		},
		{
			name:      "non-deterministic name",
			operation: "custom-allocate",
			mutate: func(t *testing.T, e *engineHarness) {
				op := e.operation(allocTestNames.allocateOperation)
				op.ObjectMeta = metav1.ObjectMeta{Name: "custom-allocate", UID: "custom-uid"}
				require.NoError(t, e.raw.Create(context.Background(), op))
				e.updateAllocationStatus(t, func(status *unboundedv1alpha3.MachineAllocationStatus) { status.Operations.Allocate = nil })
			},
		},
		{
			name:      "retired allocation",
			operation: allocTestNames.allocateOperation,
			prepare:   stopBeforeRecord,
			mutate: func(t *testing.T, e *engineHarness) {
				e.updateAllocationSpec(t, func(spec *unboundedv1alpha3.MachineAllocationSpec) {
					spec.Lifecycle = unboundedv1alpha3.MachineAllocationReleased
				})
			},
		},
		{
			name:      "failed allocation",
			operation: allocTestNames.allocateOperation,
			prepare:   stopBeforeRecord,
			mutate: func(t *testing.T, e *engineHarness) {
				e.updateAllocationStatus(t, func(status *unboundedv1alpha3.MachineAllocationStatus) {
					status.FailureReason = unboundedv1alpha3.FailureInsufficientCapacity
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, tt.prepare)
			tt.mutate(t, e)

			result := e.reconcile(tt.operation)
			require.Equal(t, ctrl.Result{}, result)
			e.requireNoProviderCalls(t)
			e.requireCompleted(t, tt.operation, unboundedv1alpha3.OperationPhaseFailed, allocationReasonNotRecorded)
		})
	}
}

func TestAllocationOperationFencedBeforeBegin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, *engineHarness)
	}{
		{name: "machine fenced", mutate: func(t *testing.T, e *engineHarness) { e.fenceMachine(t) }},
		{
			name: "machine deleting",
			mutate: func(t *testing.T, e *engineHarness) {
				require.NoError(t, e.raw.Delete(context.Background(), e.machine()))
			},
		},
		{
			name: "allocation released",
			mutate: func(t *testing.T, e *engineHarness) {
				e.updateAllocationSpec(t, func(spec *unboundedv1alpha3.MachineAllocationSpec) {
					spec.Lifecycle = unboundedv1alpha3.MachineAllocationReleased
				})
			},
		},
		{
			name: "allocation deleting",
			mutate: func(t *testing.T, e *engineHarness) {
				require.NoError(t, e.raw.Delete(context.Background(), e.allocation()))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			tt.mutate(t, e)

			e.reconcile(allocTestNames.allocateOperation)
			e.requireNoProviderCalls(t)
			e.requireCompleted(t, allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseFailed, allocationReasonFenced)
		})
	}
}

func TestAllocationOperationUncertainBeginHolds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, *engineHarness)
		reason string
		// transient reaches the InProgress state through a transient Begin
		// error instead of a crash.
		transient bool
	}{
		{name: "fenced", mutate: func(t *testing.T, e *engineHarness) { e.fenceMachine(t) }, reason: allocationReasonBeginOutcomeUnknown},
		{
			name:      "fenced after a transient Begin error",
			mutate:    func(t *testing.T, e *engineHarness) { e.fenceMachine(t) },
			reason:    allocationReasonBeginOutcomeUnknown,
			transient: true,
		},
		{
			name: "token missing",
			mutate: func(t *testing.T, e *engineHarness) {
				require.NoError(t, e.raw.Delete(context.Background(), e.secret(allocTestNames.tokenSecret)))
			},
			reason: allocationReasonBeginOutcomeUnknown,
		},
		{
			name: "ConfigMap hash changed",
			mutate: func(t *testing.T, e *engineHarness) {
				configMap := e.configMap("bootstrap-files")
				configMap.Data["run"] = "echo changed"
				require.NoError(t, e.raw.Update(context.Background(), configMap))
			},
			reason: allocationReasonBeginOutcomeUnknown,
		},
		{name: "credential replaced", mutate: replaceCredential, reason: allocationReasonAuthorityReplaced},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)

			if tt.transient {
				e.provider.setBeginErr(errors.New("request timed out"))
				e.reconcile(allocTestNames.allocateOperation)
				e.provider.setBeginErr(nil)
				e.provider.reset()

				op := e.operation(allocTestNames.allocateOperation)
				require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, op.Status.Targets[0].Phase)
				require.Equal(t, int32(1), op.Status.Targets[0].Attempts)
				require.Nil(t, op.Status.Targets[0].ProviderOperation)
			} else {
				e.markInProgressWithoutHandle(t)
			}

			tt.mutate(t, e)

			result := e.reconcile(allocTestNames.allocateOperation)
			require.Equal(t, ctrl.Result{RequeueAfter: engineTestStalledInterval}, result)
			e.requireNoProviderCalls(t)

			op := e.operation(allocTestNames.allocateOperation)
			require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, op.Status.Phase)
			require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, op.Status.Targets[0].Phase)
			require.True(t, strings.HasPrefix(op.Status.Targets[0].Message, tt.reason+": "), op.Status.Targets[0].Message)

			completed := apimeta.FindStatusCondition(op.Status.Conditions, OperationConditionCompleted)
			require.NotNil(t, completed)
			require.Equal(t, metav1.ConditionFalse, completed.Status)
			require.Equal(t, tt.reason, completed.Reason)
			require.Nil(t, op.Status.CompletedAt)
		})
	}
}

func TestAllocationOperationReplaysBegin(t *testing.T) {
	t.Parallel()

	e := newEngineHarness(t, nil)

	// The status write that persists the handle fails, as if the engine
	// crashed after Begin returned.
	e.failHandleWrites(true)
	_, err := e.reconcileResult(allocTestNames.allocateOperation)
	require.Error(t, err)
	e.failHandleWrites(false)

	op := e.operation(allocTestNames.allocateOperation)
	require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, op.Status.Targets[0].Phase)
	require.Nil(t, op.Status.Targets[0].ProviderOperation)

	e.reconcile(allocTestNames.allocateOperation)

	begins, _, _, _ := e.provider.calls()
	require.Len(t, begins, 2)
	require.Equal(t, begins[0].OperationUID, begins[1].OperationUID)
	require.NotEmpty(t, begins[0].Allocation.UserData)
	require.Equal(t, begins[0].Allocation.UserData, begins[1].Allocation.UserData)
	require.Equal(t, begins[0].Allocation, begins[1].Allocation)
	require.NotNil(t, e.operation(allocTestNames.allocateOperation).Status.Targets[0].ProviderOperation)
}

func TestAllocationOperationAuthorityReplaced(t *testing.T) {
	t.Parallel()

	for _, replace := range []struct {
		name   string
		mutate func(*testing.T, *engineHarness)
	}{
		{name: "credential", mutate: replaceCredential},
		{
			name: "site",
			mutate: func(t *testing.T, e *engineHarness) {
				var site unboundedv1alpha3.Site
				require.NoError(t, e.raw.Get(context.Background(), client.ObjectKey{Name: classTestSite}, &site))
				require.NoError(t, e.raw.Delete(context.Background(), &site))
				require.NoError(t, e.raw.Create(context.Background(), newClassTestSite("site-a-new-uid")))
			},
		},
	} {
		t.Run(replace.name+" before Begin fails", func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			replace.mutate(t, e)

			e.reconcile(allocTestNames.allocateOperation)
			e.requireNoProviderCalls(t)
			e.requireCompleted(t, allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseFailed, allocationReasonAuthorityReplaced)
		})

		t.Run(replace.name+" during Poll holds", func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			e.reconcile(allocTestNames.allocateOperation)
			e.provider.reset()
			replace.mutate(t, e)

			result := e.reconcile(allocTestNames.allocateOperation)
			require.Equal(t, ctrl.Result{RequeueAfter: engineTestStalledInterval}, result)
			e.requireNoProviderCalls(t)

			op := e.operation(allocTestNames.allocateOperation)
			require.Equal(t, unboundedv1alpha3.OperationPhaseInProgress, op.Status.Phase)
			require.NotNil(t, op.Status.Targets[0].ProviderOperation)
			require.Equal(t, allocationReasonAuthorityReplaced, apimeta.FindStatusCondition(op.Status.Conditions, OperationConditionCompleted).Reason)
		})
	}
}

func TestAllocationOperationAppliesResult(t *testing.T) {
	t.Parallel()

	owned := []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-native-1"}}

	tests := []struct {
		name   string
		poll   publicmachineops.PollResult
		phase  unboundedv1alpha3.OperationPhase
		reason string
	}{
		{
			name: "succeeded",
			poll: publicmachineops.PollResult{State: publicmachineops.ProviderOperationStateSucceeded, Result: publicmachineops.OperationResult{
				ProviderID:        "example:///hosts/replaced",
				CleanupProviderID: "example:///hosts/old",
				Allocation: &publicmachineops.AllocationResult{
					ProviderResourceID: "native-1", InstanceType: "Standard_D2", Zone: "zone-1", Image: "image-native", OwnedResources: owned,
				},
			}},
			phase:  unboundedv1alpha3.OperationPhaseComplete,
			reason: "Succeeded",
		},
		{
			name: "failed with owned resources",
			poll: publicmachineops.PollResult{
				State:  publicmachineops.ProviderOperationStateFailed,
				Reason: "QuotaExceeded",
				Result: publicmachineops.OperationResult{Allocation: &publicmachineops.AllocationResult{OwnedResources: owned}},
			},
			phase:  unboundedv1alpha3.OperationPhaseFailed,
			reason: "QuotaExceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			e.reconcile(allocTestNames.allocateOperation)
			machineBefore := e.machine()
			e.provider.setPoll(tt.poll, nil)
			e.recordStatusUpdates(true)

			e.reconcile(allocTestNames.allocateOperation)

			_, polls, _, _ := e.provider.calls()
			require.Len(t, polls, 1)
			require.Empty(t, polls[0].Allocation.UserData)
			require.Empty(t, polls[0].Allocation.ProviderConfig)

			updates := e.statusUpdates()
			first := slices.IndexFunc(updates, func(op *unboundedv1alpha3.MachineOperation) bool {
				return len(op.Status.Targets) == 1 && (op.Status.Targets[0].Phase == unboundedv1alpha3.OperationPhaseComplete ||
					op.Status.Targets[0].Phase == unboundedv1alpha3.OperationPhaseFailed)
			})
			require.GreaterOrEqual(t, first, 0)
			require.NotNil(t, updates[first].Status.Allocation, "the first update with a terminal target carries status.allocation")
			require.False(t, updates[first].Status.IsTerminal(), "the operation phase turns terminal after the result is written")
			require.Equal(t, owned, updates[first].Status.Allocation.OwnedResources)

			op := e.operation(allocTestNames.allocateOperation)
			require.Equal(t, tt.phase, op.Status.Phase)
			require.Equal(t, tt.reason, apimeta.FindStatusCondition(op.Status.Conditions, OperationConditionCompleted).Reason)
			require.Equal(t, owned, op.Status.Allocation.OwnedResources)
			require.Equal(t, machineBefore, e.machine(), "the Machine is never patched")
		})
	}
}

func TestAllocationOperationAppliesResultWithoutOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		poll publicmachineops.PollResult
		err  error
	}{
		{name: "succeeded without a report", poll: publicmachineops.PollResult{State: publicmachineops.ProviderOperationStateSucceeded}},
		{name: "failed without a report", poll: publicmachineops.PollResult{State: publicmachineops.ProviderOperationStateFailed, Reason: "SkuNotAvailable"}},
		{name: "canceled without a report", poll: publicmachineops.PollResult{State: publicmachineops.ProviderOperationStateCanceled}},
		{name: "poll PermanentError", err: &publicmachineops.PermanentError{Reason: "HandleExpired", Err: errors.New("operation handle is gone")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			e.reconcile(allocTestNames.allocateOperation)
			e.provider.setPoll(tt.poll, tt.err)
			e.recordStatusUpdates(true)

			e.reconcile(allocTestNames.allocateOperation)

			updates := e.statusUpdates()
			first := slices.IndexFunc(updates, func(op *unboundedv1alpha3.MachineOperation) bool {
				return len(op.Status.Targets) == 1 && op.Status.Targets[0].Phase == unboundedv1alpha3.OperationPhaseFailed
			})
			require.GreaterOrEqual(t, first, 0)

			completed := apimeta.FindStatusCondition(updates[first].Status.Conditions, OperationConditionCompleted)
			require.NotNil(t, completed, "the update that fails the target sets Completed")
			require.Equal(t, metav1.ConditionFalse, completed.Status)
			require.Equal(t, allocationReasonOwnershipUnknown, completed.Reason)

			op := e.operation(allocTestNames.allocateOperation)
			e.requireCompleted(t, op.Name, unboundedv1alpha3.OperationPhaseFailed, allocationReasonOwnershipUnknown)
			require.Nil(t, op.Status.Allocation)
		})
	}
}

func TestAllocationOperationBeginPermanentError(t *testing.T) {
	t.Parallel()

	refusal := &publicmachineops.PermanentError{Reason: "QuotaExceeded", Err: errors.New("quota exhausted")}

	tests := []struct {
		name    string
		prepare func(*testing.T, *engineHarness)
		reason  string
	}{
		{name: "first Begin", reason: "QuotaExceeded"},
		{
			name: "after a transient Begin error",
			prepare: func(_ *testing.T, e *engineHarness) {
				e.provider.setBeginErr(errors.New("request timed out"))
				e.reconcile(allocTestNames.allocateOperation)
			},
			reason: allocationReasonOwnershipUnknown,
		},
		{
			name:    "after a crash left the target InProgress",
			prepare: func(t *testing.T, e *engineHarness) { e.markInProgressWithoutHandle(t) },
			reason:  allocationReasonOwnershipUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			if tt.prepare != nil {
				tt.prepare(t, e)
			}

			e.provider.setBeginErr(refusal)
			e.reconcile(allocTestNames.allocateOperation)

			e.requireCompleted(t, allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseFailed, tt.reason)

			op := e.operation(allocTestNames.allocateOperation)
			require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, op.Status.Targets[0].Phase)
			require.Nil(t, op.Status.Allocation)
		})
	}
}

func TestReleaseOperationWaitsAndRequests(t *testing.T) {
	t.Parallel()

	waits := []struct {
		name   string
		mutate func(*testing.T, *engineHarness)
	}{
		{
			name: "lifecycle not released",
			mutate: func(t *testing.T, e *engineHarness) {
				e.updateAllocationSpec(t, func(spec *unboundedv1alpha3.MachineAllocationSpec) {
					spec.Lifecycle = unboundedv1alpha3.MachineAllocationActive
				})
			},
		},
		{
			name: "machine not fenced",
			mutate: func(t *testing.T, e *engineHarness) {
				machine := e.machine()
				machine.Spec.Fenced = false
				require.NoError(t, e.raw.Update(context.Background(), machine))
			},
		},
		{
			name: "recorded HostAllocate not terminal",
			mutate: func(t *testing.T, e *engineHarness) {
				op := e.operation(allocTestNames.allocateOperation)
				op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
				require.NoError(t, e.raw.Status().Update(context.Background(), op))
			},
		},
		{
			name: "recorded HostAllocate ownership unknown",
			mutate: func(t *testing.T, e *engineHarness) {
				op := e.operation(allocTestNames.allocateOperation)
				op.Status.Phase = unboundedv1alpha3.OperationPhaseFailed
				op.Status.Allocation = nil
				op.Status.Conditions = []metav1.Condition{{
					Type: OperationConditionCompleted, Status: metav1.ConditionFalse, Reason: allocationReasonOwnershipUnknown,
					LastTransitionTime: metav1.NewTime(allocTestNow),
				}}
				require.NoError(t, e.raw.Status().Update(context.Background(), op))
			},
		},
		{
			name: "recorded HostAllocate missing",
			mutate: func(t *testing.T, e *engineHarness) {
				op := e.operation(allocTestNames.allocateOperation)
				op.Finalizers = nil
				require.NoError(t, e.raw.Update(context.Background(), op))
				require.NoError(t, e.raw.Delete(context.Background(), op))
			},
		},
	}

	for _, wait := range waits {
		t.Run("waits while "+wait.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, prepareReleasedAllocation)
			require.NotNil(t, e.allocation().Status.Operations.Release)
			wait.mutate(t, e)
			e.resetWrites()

			result := e.reconcile(allocTestNames.releaseOperation)
			require.Equal(t, ctrl.Result{RequeueAfter: engineTestPollInterval}, result)
			e.requireNoProviderCalls(t)
			require.Empty(t, e.writes())
		})
	}

	t.Run("begins with the merged owned resources", func(t *testing.T) {
		t.Parallel()

		e := newEngineHarness(t, prepareReleasedAllocation)
		e.updateAllocationStatus(t, func(status *unboundedv1alpha3.MachineAllocationStatus) {
			status.OwnedResources = []unboundedv1alpha3.ProviderOwnedResource{{ID: "vm", NativeID: "vm-1"}, {ID: "disk", NativeID: "disk-1"}}
		})

		e.reconcile(allocTestNames.releaseOperation)

		allocateBegins, _, releaseBegins, _ := e.provider.calls()
		require.Empty(t, allocateBegins)
		require.Len(t, releaseBegins, 1)

		request := releaseBegins[0]
		require.Equal(t, unboundedv1alpha3.OperationHostRelease, request.Operation)
		require.Equal(t, []unboundedv1alpha3.ProviderOwnedResource{
			{ID: "disk", NativeID: "disk-1"},
			{ID: "nic", NativeID: "nic-1"},
			{ID: "vm", NativeID: "vm-1"},
		}, request.Allocation.OwnedResources)
		require.Equal(t, publicmachineops.ProviderConfig{}, request.Allocation.ProviderConfig)
		require.Empty(t, request.Allocation.UserData)

		e.provider.setPoll(publicmachineops.PollResult{
			State:  publicmachineops.ProviderOperationStateSucceeded,
			Result: publicmachineops.OperationResult{Allocation: &publicmachineops.AllocationResult{ProviderResourceID: "ignored"}},
		}, nil)
		e.reconcile(allocTestNames.releaseOperation)

		release := e.operation(allocTestNames.releaseOperation)
		require.Equal(t, unboundedv1alpha3.OperationPhaseComplete, release.Status.Phase)
		require.Nil(t, release.Status.Allocation)
	})
}

func TestReleaseOperationAuthFailureRecovery(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name             string
		replaceAuthority bool
	}{
		{name: "secret restored"},
		{name: "credential replaced", replaceAuthority: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, prepareReleasedAllocation)
			fixture := newClassFixture()
			ctx := context.Background()

			var credential unboundedv1alpha3.MachineOperationCredential
			require.NoError(t, e.raw.Get(ctx, client.ObjectKey{Name: fixture.credential.Name}, &credential))
			credential.Spec.Auth = fixture.credential.Spec.Auth
			require.NoError(t, e.raw.Update(ctx, &credential))

			releaseUID := e.operation(allocTestNames.releaseOperation).UID
			releaseRef := *e.allocation().Status.Operations.Release

			for range 2 {
				result := e.reconcile(allocTestNames.releaseOperation)
				require.Equal(t, ctrl.Result{RequeueAfter: engineTestStalledInterval}, result)
				e.requireNoProviderCalls(t)
				e.requireCompleted(t, allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseInProgress, authReasonSecretNotFound)

				op := e.operation(allocTestNames.releaseOperation)
				completed := apimeta.FindStatusCondition(op.Status.Conditions, OperationConditionCompleted)
				require.Equal(t, metav1.ConditionFalse, completed.Status)
				require.Contains(t, op.Finalizers, unboundedv1alpha3.MachineAllocationFinalizer)
				require.True(t, e.machine().Spec.Fenced)
				require.Equal(t, releaseRef, *e.allocation().Status.Operations.Release)
			}

			if tt.replaceAuthority {
				replaceCredential(t, e)

				result := e.reconcile(allocTestNames.releaseOperation)
				require.Equal(t, ctrl.Result{RequeueAfter: engineTestStalledInterval}, result)
				e.requireNoProviderCalls(t)
				e.requireCompleted(t, allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseInProgress, allocationReasonAuthorityReplaced)

				return
			}

			require.NoError(t, e.raw.Create(ctx, fixture.secret))

			e.reconcile(allocTestNames.releaseOperation)

			_, _, begins, _ := e.provider.calls()
			require.Len(t, begins, 1)
			require.Equal(t, releaseUID, begins[0].OperationUID)
			require.Equal(t, map[string]string{"token": classTestSecretValue}, begins[0].Auth.SecretData)

			e.provider.setPoll(publicmachineops.PollResult{State: publicmachineops.ProviderOperationStateSucceeded}, nil)
			e.reconcile(allocTestNames.releaseOperation)
			e.requireCompleted(t, allocTestNames.releaseOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded")
			require.Equal(t, releaseUID, e.operation(allocTestNames.releaseOperation).UID)
			require.Equal(t, releaseRef, *e.allocation().Status.Operations.Release)
		})
	}
}

func prepareReleasedAllocation(h *allocationHarness) {
	h.settle()
	h.setOperationHandle(allocTestNames.allocateOperation)
	h.completeOperation(allocTestNames.allocateOperation, unboundedv1alpha3.OperationPhaseComplete, "Succeeded",
		&unboundedv1alpha3.MachineOperationAllocationResult{
			OwnedResources: []unboundedv1alpha3.ProviderOwnedResource{
				{ID: "vm", NativeID: "vm-1"},
				{ID: "nic", NativeID: "nic-1"},
			},
		})
	h.settle()
	h.setLifecycle(unboundedv1alpha3.MachineAllocationReleased)
	h.settle()
}

func TestAllocationOperationIgnoresUnboundOps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*testing.T, *engineHarness)
	}{
		{
			name: "terminal",
			mutate: func(t *testing.T, e *engineHarness) {
				op := e.operation(allocTestNames.allocateOperation)
				op.Status.Phase = unboundedv1alpha3.OperationPhaseFailed
				require.NoError(t, e.raw.Status().Update(context.Background(), op))
			},
		},
		{
			name: "machine missing",
			mutate: func(t *testing.T, e *engineHarness) {
				machine := e.machine()
				machine.Finalizers = nil
				require.NoError(t, e.raw.Update(context.Background(), machine))
				require.NoError(t, e.raw.Delete(context.Background(), machine))
			},
		},
		{name: "machine out of scope", mutate: func(_ *testing.T, e *engineHarness) { e.reconciler.SiteName = "site-b" }},
		{
			name: "provider not registered",
			mutate: func(t *testing.T, e *engineHarness) {
				e.reconciler.Providers = []*Provider{newAllocationTestProvider(t, "OtherCloud", e.provider)}
			},
		},
		{
			name: "provider without capacity",
			mutate: func(t *testing.T, e *engineHarness) {
				e.reconciler.Providers = []*Provider{newClassTestPlainProvider(t, classTestProvider)}
			},
		},
		{
			name: "allocationRef differs from the Machine's",
			mutate: func(t *testing.T, e *engineHarness) {
				op := e.operation(allocTestNames.allocateOperation)
				op.Finalizers = nil
				require.NoError(t, e.raw.Update(context.Background(), op))
				require.NoError(t, e.raw.Delete(context.Background(), op))

				op.ObjectMeta = metav1.ObjectMeta{Name: allocTestNames.allocateOperation, UID: op.UID}
				op.Spec.AllocationRef = &unboundedv1alpha3.UIDReference{Name: allocTestName, UID: "another-incarnation"}
				op.Status = unboundedv1alpha3.MachineOperationStatus{}
				require.NoError(t, e.raw.Create(context.Background(), op))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			tt.mutate(t, e)
			before := e.operation(allocTestNames.allocateOperation)
			e.resetWrites()

			result := e.reconcile(allocTestNames.allocateOperation)
			require.Equal(t, ctrl.Result{}, result)
			e.requireNoProviderCalls(t)
			require.Empty(t, e.writes())
			require.Equal(t, before, e.operation(allocTestNames.allocateOperation))
		})
	}
}

func TestShouldReconcileAllocationOperation(t *testing.T) {
	t.Parallel()

	predicate := allocationOperationPredicate()

	for _, kind := range []unboundedv1alpha3.OperationKind{unboundedv1alpha3.OperationHostAllocate, unboundedv1alpha3.OperationHostRelease} {
		op := newAllocationOperation("op-1", "machine-1", kind)
		op.Generation = 1

		terminal := op.DeepCopy()
		terminal.Status.Phase = unboundedv1alpha3.OperationPhaseComplete

		statusOnly := op.DeepCopy()
		statusOnly.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress

		newGeneration := op.DeepCopy()
		newGeneration.Generation = 2

		require.True(t, shouldReconcileAllocationOperation(op), kind)
		require.False(t, shouldReconcileAllocationOperation(terminal), kind)
		require.True(t, predicate.Create(event.CreateEvent{Object: op}), kind)
		require.False(t, predicate.Create(event.CreateEvent{Object: terminal}), kind)
		require.True(t, predicate.Generic(event.GenericEvent{Object: op}), kind)
		require.False(t, predicate.Update(event.UpdateEvent{ObjectOld: op, ObjectNew: statusOnly}), kind)
		require.True(t, predicate.Update(event.UpdateEvent{ObjectOld: op, ObjectNew: newGeneration}), kind)
		require.False(t, predicate.Delete(event.DeleteEvent{Object: op}), kind)
	}

	hostOperation := newMachineOperation("reboot", "machine-1", unboundedv1alpha3.OperationHostHardReboot)
	require.False(t, shouldReconcileAllocationOperation(hostOperation))
	require.False(t, predicate.Create(event.CreateEvent{Object: hostOperation}))
}

func TestHostOperationsOnAllocationMachines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		kind   unboundedv1alpha3.OperationKind
		mutate func(*testing.T, *engineHarness)
		phase  unboundedv1alpha3.OperationPhase
		reason string
	}{
		{name: "HostReplace", kind: unboundedv1alpha3.OperationHostReplace, phase: unboundedv1alpha3.OperationPhaseFailed, reason: reasonUnsupportedOperation},
		{name: "HostPowerOn", kind: unboundedv1alpha3.OperationHostPowerOn, phase: unboundedv1alpha3.OperationPhaseFailed, reason: reasonUnsupportedOperation},
		{name: "HostPowerOff", kind: unboundedv1alpha3.OperationHostPowerOff, phase: unboundedv1alpha3.OperationPhaseFailed, reason: reasonUnsupportedOperation},
		{
			name:   "HostHardReboot on a fenced Machine",
			kind:   unboundedv1alpha3.OperationHostHardReboot,
			mutate: func(t *testing.T, e *engineHarness) { e.fenceMachine(t) },
			phase:  unboundedv1alpha3.OperationPhaseFailed,
			reason: reasonMachineFenced,
		},
		{
			name: "HostHardReboot on a deleting Machine",
			kind: unboundedv1alpha3.OperationHostHardReboot,
			mutate: func(t *testing.T, e *engineHarness) {
				require.NoError(t, e.raw.Delete(context.Background(), e.machine()))
			},
			phase:  unboundedv1alpha3.OperationPhaseFailed,
			reason: reasonMachineFenced,
		},
		{name: "HostHardReboot on an unfenced Machine", kind: unboundedv1alpha3.OperationHostHardReboot, phase: unboundedv1alpha3.OperationPhaseComplete, reason: "Succeeded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			if tt.mutate != nil {
				tt.mutate(t, e)
			}

			op := newMachineOperation("host-op", allocTestNames.machine, tt.kind)
			op.UID = "host-op-uid"
			require.NoError(t, e.raw.Create(context.Background(), op))

			_, err := e.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
			require.NoError(t, err)
			e.requireCompleted(t, op.Name, tt.phase, tt.reason)
		})
	}
}

// stopBeforeRecord leaves the allocation with an unrecorded HostAllocate:
// the record write fails after the op is created.
func stopBeforeRecord(h *allocationHarness) {
	h.failStatusWrites(true)
	_, _ = h.reconcileResult()
	h.failStatusWrites(false)
}

func replaceCredential(t *testing.T, e *engineHarness) {
	t.Helper()

	var credential unboundedv1alpha3.MachineOperationCredential
	require.NoError(t, e.raw.Get(context.Background(), client.ObjectKey{Name: "cred-a"}, &credential))
	require.NoError(t, e.raw.Delete(context.Background(), &credential))

	replacement := newWorkloadIdentityCredential("cred-a", classTestSite, classTestProvider)
	replacement.UID = "cred-a-new-uid"
	require.NoError(t, e.raw.Create(context.Background(), replacement))
}

// engineHarness runs the allocation path of a MachineOperationReconciler
// against one fake store, the direct client the engine uses.
type engineHarness struct {
	raw        client.WithWatch
	provider   *fakeCapacityProvider
	reconciler *MachineOperationReconciler

	mu             sync.Mutex
	calls          []string
	failHandle     bool
	recordStatuses bool
	statuses       []*unboundedv1alpha3.MachineOperation
}

// newEngineHarness launches an allocation with the allocation controller up
// to its recorded HostAllocate, lets prepare change that, and copies every
// object into one store for the engine.
func newEngineHarness(t *testing.T, prepare func(*allocationHarness)) *engineHarness {
	t.Helper()

	h := newAllocationHarness(t, newAllocationFixture(t))
	h.reconcile() // snapshot
	h.reconcile() // finalizer

	if prepare == nil {
		h.settle()
	} else {
		prepare(h)
	}

	ctx := context.Background()

	var objects []client.Object

	for _, list := range []client.ObjectList{
		&unboundedv1alpha3.MachineAllocationList{},
		&unboundedv1alpha3.MachineClassList{},
		&unboundedv1alpha3.MachineList{},
		&unboundedv1alpha3.MachineOperationList{},
		&unboundedv1alpha3.MachineOperationCredentialList{},
		&unboundedv1alpha3.MachineConfigurationVersionList{},
		&unboundedv1alpha3.SiteList{},
		&corev1.NamespaceList{},
	} {
		require.NoError(t, h.store.List(ctx, list))
		objects = append(objects, listObjects(t, list)...)
	}

	configMaps := &corev1.ConfigMapList{}
	require.NoError(t, h.configMaps.List(ctx, configMaps))
	objects = append(objects, listObjects(t, configMaps)...)

	secrets := &corev1.SecretList{}
	require.NoError(t, h.secrets.List(ctx, secrets))
	objects = append(objects, listObjects(t, secrets)...)

	for _, object := range objects {
		object.SetResourceVersion("")
	}

	scheme := newOperationTestScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))

	e := &engineHarness{provider: newFakeCapacityProvider()}
	e.raw = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&unboundedv1alpha3.MachineAllocation{}, &unboundedv1alpha3.MachineOperation{}, &unboundedv1alpha3.Machine{}).
		Build()

	e.reconciler = &MachineOperationReconciler{
		Client:                      interceptor.NewClient(e.raw, e.funcs()),
		Providers:                   []*Provider{newAllocationTestProvider(t, classTestProvider, e.provider)},
		ProviderPollInterval:        engineTestPollInterval,
		ProviderStallAfter:          time.Hour,
		ProviderStalledPollInterval: engineTestStalledInterval,
		Now:                         fixedOperationNow,
		ClusterInfo:                 testClusterInfo(),
	}

	return e
}

func listObjects(t *testing.T, list client.ObjectList) []client.Object {
	t.Helper()

	items, err := apimeta.ExtractList(list)
	require.NoError(t, err)

	objects := make([]client.Object, 0, len(items))
	for _, item := range items {
		object, ok := item.(client.Object)
		require.True(t, ok)

		objects = append(objects, object)
	}

	return objects
}

func (e *engineHarness) funcs() interceptor.Funcs {
	write := func(verb string, obj client.Object) {
		e.mu.Lock()
		defer e.mu.Unlock()

		e.calls = append(e.calls, verb+" "+allocationTestKind(obj)+" "+obj.GetName())
	}

	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			write("create", obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			write("update", obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			write("patch", obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			write("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			write("status", obj)

			e.mu.Lock()
			failHandle := e.failHandle
			record := e.recordStatuses
			e.mu.Unlock()

			op, isOperation := obj.(*unboundedv1alpha3.MachineOperation)
			if isOperation && failHandle && hasProviderHandle(op) {
				return errors.New("status write lost")
			}

			if isOperation && record {
				e.mu.Lock()
				e.statuses = append(e.statuses, op.DeepCopy())
				e.mu.Unlock()
			}

			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			write("status-patch", obj)
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}
}

func (e *engineHarness) reconcile(name string) ctrl.Result {
	result, err := e.reconcileResult(name)
	if err != nil {
		panic(err)
	}

	return result
}

func (e *engineHarness) reconcileResult(name string) (ctrl.Result, error) {
	return e.reconciler.ReconcileAllocationOperation(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
}

func (e *engineHarness) writes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.calls)
}

func (e *engineHarness) resetWrites() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.calls = nil
}

func (e *engineHarness) failHandleWrites(fail bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.failHandle = fail
}

func (e *engineHarness) recordStatusUpdates(record bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.recordStatuses = record
}

func (e *engineHarness) statusUpdates() []*unboundedv1alpha3.MachineOperation {
	e.mu.Lock()
	defer e.mu.Unlock()

	return slices.Clone(e.statuses)
}

func (e *engineHarness) requireNoProviderCalls(t *testing.T) {
	t.Helper()

	allocateBegins, allocatePolls, releaseBegins, releasePolls := e.provider.calls()
	require.Empty(t, allocateBegins, "HostAllocate Begin calls")
	require.Empty(t, allocatePolls, "HostAllocate Poll calls")
	require.Empty(t, releaseBegins, "HostRelease Begin calls")
	require.Empty(t, releasePolls, "HostRelease Poll calls")
}

func (e *engineHarness) requireCompleted(t *testing.T, name string, phase unboundedv1alpha3.OperationPhase, reason string) {
	t.Helper()

	op := e.operation(name)
	require.Equal(t, phase, op.Status.Phase, op.Status.Message)

	completed := apimeta.FindStatusCondition(op.Status.Conditions, OperationConditionCompleted)
	require.NotNil(t, completed)
	require.Equal(t, reason, completed.Reason, completed.Message)
}

func (e *engineHarness) allocation() *unboundedv1alpha3.MachineAllocation {
	var allocation unboundedv1alpha3.MachineAllocation
	if err := e.raw.Get(context.Background(), client.ObjectKey{Name: allocTestName}, &allocation); err != nil {
		panic(err)
	}

	return &allocation
}

func (e *engineHarness) machine() *unboundedv1alpha3.Machine {
	var machine unboundedv1alpha3.Machine
	if err := e.raw.Get(context.Background(), client.ObjectKey{Name: allocTestNames.machine}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &machine
}

func (e *engineHarness) operation(name string) *unboundedv1alpha3.MachineOperation {
	var operation unboundedv1alpha3.MachineOperation
	if err := e.raw.Get(context.Background(), client.ObjectKey{Name: name}, &operation); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		panic(err)
	}

	return &operation
}

func (e *engineHarness) secret(name string) *corev1.Secret {
	var secret corev1.Secret
	if err := e.raw.Get(context.Background(), client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: name}, &secret); err != nil {
		panic(err)
	}

	return &secret
}

func (e *engineHarness) configMap(name string) *corev1.ConfigMap {
	var configMap corev1.ConfigMap
	if err := e.raw.Get(context.Background(), client.ObjectKey{Namespace: classTestConfigNamespace, Name: name}, &configMap); err != nil {
		panic(err)
	}

	return &configMap
}

func (e *engineHarness) fenceMachine(t *testing.T) {
	t.Helper()

	machine := e.machine()
	machine.Spec.Fenced = true
	require.NoError(t, e.raw.Update(context.Background(), machine))
}

func (e *engineHarness) updateAllocationSpec(t *testing.T, mutate func(*unboundedv1alpha3.MachineAllocationSpec)) {
	t.Helper()

	allocation := e.allocation()
	mutate(&allocation.Spec)
	require.NoError(t, e.raw.Update(context.Background(), allocation))
}

func (e *engineHarness) updateAllocationStatus(t *testing.T, mutate func(*unboundedv1alpha3.MachineAllocationStatus)) {
	t.Helper()

	allocation := e.allocation()
	mutate(&allocation.Status)
	require.NoError(t, e.raw.Status().Update(context.Background(), allocation))
}

// markInProgressWithoutHandle leaves the HostAllocate as a crash between
// marking it InProgress and persisting the provider handle does.
func (e *engineHarness) markInProgressWithoutHandle(t *testing.T) {
	t.Helper()

	op := e.operation(allocTestNames.allocateOperation)
	op.Status.Phase = unboundedv1alpha3.OperationPhaseInProgress
	op.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
		MachineRef: allocTestNames.machine,
		Phase:      unboundedv1alpha3.OperationPhaseInProgress,
		Input:      &unboundedv1alpha3.MachineOperationTargetInput{},
	}}
	require.NoError(t, e.raw.Status().Update(context.Background(), op))
}

func TestAllocationOperationFinishesTerminalTarget(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		target    unboundedv1alpha3.OperationPhase
		completed string
		phase     unboundedv1alpha3.OperationPhase
		reason    string
	}{
		{name: "Complete", target: unboundedv1alpha3.OperationPhaseComplete, phase: unboundedv1alpha3.OperationPhaseComplete, reason: "Succeeded"},
		{name: "Failed", target: unboundedv1alpha3.OperationPhaseFailed, phase: unboundedv1alpha3.OperationPhaseFailed, reason: "TargetFailed"},
		{
			name:      "Failed with the reason on the Completed condition",
			target:    unboundedv1alpha3.OperationPhaseFailed,
			completed: allocationReasonOwnershipUnknown,
			phase:     unboundedv1alpha3.OperationPhaseFailed,
			reason:    allocationReasonOwnershipUnknown,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEngineHarness(t, nil)
			e.markInProgressWithoutHandle(t)

			op := e.operation(allocTestNames.allocateOperation)
			op.Status.Targets[0].Phase = tt.target
			op.Status.Targets[0].Message = "finished before the operation recorded it"

			if tt.completed != "" {
				op.Status.Conditions = []metav1.Condition{{
					Type: OperationConditionCompleted, Status: metav1.ConditionFalse, Reason: tt.completed,
					LastTransitionTime: metav1.NewTime(allocTestNow),
				}}
			}

			require.NoError(t, e.raw.Status().Update(context.Background(), op))

			e.reconcile(allocTestNames.allocateOperation)
			e.requireNoProviderCalls(t)
			e.requireCompleted(t, allocTestNames.allocateOperation, tt.phase, tt.reason)
		})
	}
}
