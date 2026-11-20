// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func requireCompletedChildrenRemoved(t *testing.T, operation *unboundedv1alpha3.MachineOperation) *metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(operation.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionCompletedChildrenRemoved)
	require.NotNil(t, condition, "the parent does not say its completed children were removed")
	require.Equal(t, metav1.ConditionTrue, condition.Status)

	return condition
}

// settle reconciles a finished parent until it has nothing left to do.
func (h *harness) settle(name string) *unboundedv1alpha3.MachineOperation {
	h.t.Helper()

	for range 20 {
		if result := h.reconcile(name); result.RequeueAfter == 0 {
			return h.getParent(name)
		}
	}

	h.t.Fatalf("parent %s did not settle", name)

	return nil
}

func allAtOnce(onFailure unboundedv1alpha3.RolloutFailurePolicy, retention unboundedv1alpha3.ChildRetentionPolicy,
) *unboundedv1alpha3.MachineOperationRollout {
	return &unboundedv1alpha3.MachineOperationRollout{
		MaxConcurrent:  intOrString(intstr.FromString("100%")),
		OnFailure:      onFailure,
		ChildRetention: retention,
	}
}

func (h *harness) retentionFixture(rollout *unboundedv1alpha3.MachineOperationRollout,
) (*unboundedv1alpha3.MachineOperation, []string) {
	h.t.Helper()

	labels := map[string]string{"fanout.test/group": h.prefix + "-group"}

	var children []string

	machines := []string{h.prefix + "-a", h.prefix + "-b", h.prefix + "-c"}
	for _, machine := range machines {
		h.createMachine(machine, labels)
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, rollout, nil, nil)
	h.reconcile(parent.Name)

	for _, machine := range machines {
		children = append(children, childName(parent, machine))
	}

	return parent, children
}

func TestCompletedChildrenRemovedWhenAllSucceed(t *testing.T) {
	h := newHarness(t)
	parent, children := h.retentionFixture(allAtOnce("", ""))

	for _, child := range children {
		h.completeChild(child)
	}

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseComplete)

	finished := h.settle(parent.Name)

	require.Empty(t, h.childNames(parent))
	requirePhase(t, finished, unboundedv1alpha3.OperationPhaseComplete)
	requireSummary(t, finished, 3, 0, 0, 3, 0)

	condition := requireCompletedChildrenRemoved(t, finished)
	require.Contains(t, condition.Message, "0 other child operations are kept")

	// Removing its children does not reopen a finished parent.
	stable := finished.ResourceVersion

	h.reconcile(parent.Name)
	require.Equal(t, stable, h.getParent(parent.Name).ResourceVersion)
}

func TestUnsuccessfulChildrenKept(t *testing.T) {
	h := newHarness(t)
	parent, children := h.retentionFixture(allAtOnce(unboundedv1alpha3.RolloutFailurePolicyContinue, ""))

	h.completeChild(children[0])
	h.failChild(children[1], "boom")
	h.completeChild(children[2])

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseFailed)

	finished := h.settle(parent.Name)

	require.Equal(t, []string{children[1]}, h.childNames(parent))
	requirePhase(t, finished, unboundedv1alpha3.OperationPhaseFailed)
	requireSummary(t, finished, 3, 0, 0, 2, 1)

	condition := requireCompletedChildrenRemoved(t, finished)
	require.Contains(t, condition.Message, "1 other child operations are kept")
}

func TestHeldChildrenOfStoppedRolloutKept(t *testing.T) {
	h := newHarness(t)
	// One at a time, stop on the first failure.
	parent, children := h.retentionFixture(nil)

	var released, held []string

	for _, child := range children {
		if h.getChild(child).Spec.Suspend {
			held = append(held, child)
		} else {
			released = append(released, child)
		}
	}

	require.Len(t, released, 1)
	h.failChild(released[0], "boom")

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseFailed)

	finished := h.settle(parent.Name)

	require.ElementsMatch(t, children, h.childNames(parent))
	requireCompletedChildrenRemoved(t, finished)

	for _, child := range held {
		require.True(t, h.getChild(child).Spec.Suspend, "a child the rollout never started stays held")
	}
}

func TestAllChildrenRetainedOnRequest(t *testing.T) {
	h := newHarness(t)
	parent, children := h.retentionFixture(allAtOnce("", unboundedv1alpha3.ChildRetentionAll))

	for _, child := range children {
		h.completeChild(child)
	}

	h.reconcile(parent.Name)

	finished := h.settle(parent.Name)

	requirePhase(t, finished, unboundedv1alpha3.OperationPhaseComplete)
	require.ElementsMatch(t, children, h.childNames(parent))
	require.Nil(t, apimeta.FindStatusCondition(finished.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionCompletedChildrenRemoved))
}

// Metalman reads the boot media record of a finished operation, so a child
// that carries one is not removed.
func TestCompletedChildWithBootMediaKept(t *testing.T) {
	h := newHarness(t)
	parent, children := h.retentionFixture(allAtOnce("", ""))

	for _, child := range children {
		h.completeChild(child)
	}

	h.updateChild(children[1], func(child *unboundedv1alpha3.MachineOperation) {
		child.Status.Targets = []unboundedv1alpha3.MachineOperationTargetStatus{{
			MachineRef: child.Spec.MachineRef,
			BootMedia: &unboundedv1alpha3.MachineOperationBootMediaStatus{
				ID:         "media-1",
				Phase:      "Released",
				MachineUID: "uid-1",
				Netboot:    &unboundedv1alpha3.PXESpec{},
			},
		}}
	})

	h.reconcile(parent.Name)

	finished := h.settle(parent.Name)

	require.Equal(t, []string{children[1]}, h.childNames(parent))
	requireCompletedChildrenRemoved(t, finished)
}

func TestCompletedChildrenRemovedInBatches(t *testing.T) {
	h := newHarness(t)
	h.reconciler.BatchSize = 1

	parent, children := h.retentionFixture(allAtOnce("", ""))

	// Creation is batched too: reconcile until every child exists.
	for range 5 {
		h.reconcile(parent.Name)
	}

	for _, child := range children {
		h.completeChild(child)
	}

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseComplete)

	result := h.reconcile(parent.Name)
	require.Positive(t, result.RequeueAfter)
	require.Len(t, h.childNames(parent), 2, "one pass removes one batch")

	finished := h.settle(parent.Name)

	require.Empty(t, h.childNames(parent))
	requireSummary(t, finished, 3, 0, 0, 3, 0)
	requireCompletedChildrenRemoved(t, finished)
}

// A child the cache still shows as Complete, but that has since been marked
// Failed, is not removed.
func TestChildFailedAfterListingKept(t *testing.T) {
	h := newHarness(t)
	parent, children := h.retentionFixture(allAtOnce("", ""))

	for _, child := range children {
		h.completeChild(child)
	}

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseComplete)

	h.stale(h.children(parent))
	h.failChild(children[1], "rolled back")

	h.reconcile(parent.Name)
	require.Equal(t, []string{children[1]}, h.childNames(parent))

	h.fresh()

	finished := h.settle(parent.Name)

	require.Equal(t, []string{children[1]}, h.childNames(parent))
	require.Equal(t, unboundedv1alpha3.OperationPhaseFailed, h.getChild(children[1]).Status.Phase)
	requireCompletedChildrenRemoved(t, finished)
}

// While a child still appears to be running, the parent is not marked done:
// that child may yet complete and need removing.
func TestRemovalWaitsForRunningChild(t *testing.T) {
	h := newHarness(t)
	// One at a time, stop on the first failure, so two children stay held.
	parent, children := h.retentionFixture(nil)

	var released, held []string

	for _, child := range children {
		if h.getChild(child).Spec.Suspend {
			held = append(held, child)
		} else {
			released = append(released, child)
		}
	}

	h.failChild(released[0], "boom")
	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseFailed)

	// An administrator releases a held child after the rollout stopped.
	late := h.getChild(held[0])
	late.Spec.Suspend = false
	require.NoError(t, h.client.Update(h.ctx, late))

	result := h.reconcile(parent.Name)
	require.Equal(t, runningChildRecheck, result.RequeueAfter)
	require.Nil(t, apimeta.FindStatusCondition(h.getParent(parent.Name).Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionCompletedChildrenRemoved))

	h.completeChild(held[0])

	finished := h.settle(parent.Name)

	require.ElementsMatch(t, []string{released[0], held[1]}, h.childNames(parent))
	requirePhase(t, finished, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedChildrenRemoved(t, finished)
}

func TestParentThatMatchedNothingHasNoRemovalCondition(t *testing.T) {
	h := newHarness(t)

	parent := h.createParent(h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: map[string]string{"fanout.test/group": h.prefix + "-none"}}, nil, nil, nil)

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseFailed)

	finished := h.settle(parent.Name)

	require.Nil(t, apimeta.FindStatusCondition(finished.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionCompletedChildrenRemoved))
}

// An expired parent is deleted at once, without first removing its children
// one by one.
func TestExpiredParentDeletedBeforeChildRemoval(t *testing.T) {
	h := newHarness(t)
	h.reconciler.BatchSize = 1

	labels := map[string]string{"fanout.test/group": h.prefix + "-group"}
	machines := []string{h.prefix + "-a", h.prefix + "-b", h.prefix + "-c"}

	for _, machine := range machines {
		h.createMachine(machine, labels)
	}

	ttl := int32(60)
	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels},
		allAtOnce("", ""), &ttl, nil)

	for range 5 {
		h.reconcile(parent.Name)
	}

	for _, machine := range machines {
		h.completeChild(childName(parent, machine))
	}

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)

	// Before expiry a pass removes a batch and comes back within a second.
	result := h.reconcile(parent.Name)
	require.Positive(t, result.RequeueAfter)
	require.LessOrEqual(t, result.RequeueAfter, time.Second)

	h.now = current.Status.CompletedAt.Add(time.Duration(ttl) * time.Second)
	h.reconcile(parent.Name)

	var deleted unboundedv1alpha3.MachineOperation

	err := h.client.Get(h.ctx, client.ObjectKey{Name: parent.Name}, &deleted)
	require.True(t, apierrors.IsNotFound(err), "the expired parent is still there: %v", err)
}

// Only a child that completed, and is not already going, is removed.
func TestRemovableChild(t *testing.T) {
	t.Parallel()

	now := metav1.Now()

	tests := []struct {
		name  string
		child unboundedv1alpha3.MachineOperation
		want  bool
	}{
		{
			name:  "complete",
			child: unboundedv1alpha3.MachineOperation{Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseComplete}},
			want:  true,
		},
		{
			name:  "failed",
			child: unboundedv1alpha3.MachineOperation{Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseFailed}},
		},
		{
			name:  "in progress",
			child: unboundedv1alpha3.MachineOperation{Status: unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseInProgress}},
		},
		{
			name:  "not started",
			child: unboundedv1alpha3.MachineOperation{},
		},
		{
			name: "already being deleted",
			child: unboundedv1alpha3.MachineOperation{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now},
				Status:     unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseComplete},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, removableChild(&tt.child))
		})
	}
}
