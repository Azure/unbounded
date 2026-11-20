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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func requireChildrenPruned(t *testing.T, operation *unboundedv1alpha3.MachineOperation) *metav1.Condition {
	t.Helper()

	condition := apimeta.FindStatusCondition(operation.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionChildrenPruned)
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

func TestChildrenPrunedWhenAllSucceed(t *testing.T) {
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

	condition := requireChildrenPruned(t, finished)
	require.Contains(t, condition.Message, "kept 0 child operations")

	// Removing its children does not reopen a finished parent.
	stable := finished.ResourceVersion

	h.reconcile(parent.Name)
	require.Equal(t, stable, h.getParent(parent.Name).ResourceVersion)
}

func TestFailedChildrenKept(t *testing.T) {
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

	condition := requireChildrenPruned(t, finished)
	require.Contains(t, condition.Message, "kept 1 child operations")
}

// The held children of a stopped rollout carry nothing but a machine name,
// and could still be released after the parent has finished. They go; the
// parent keeps their count.
func TestHeldChildrenOfStoppedRolloutRemoved(t *testing.T) {
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

	stopped := h.getParent(parent.Name)
	requireCompletedCondition(t, stopped, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonRolloutStopped)
	requireSummary(t, stopped, 3, 2, 0, 0, 1)
	require.Len(t, held, 2)

	finished := h.settle(parent.Name)

	require.Equal(t, []string{released[0]}, h.childNames(parent))
	requireSummary(t, finished, 3, 2, 0, 0, 1)
	requireChildrenPruned(t, finished)
}

// Under Continue the rollout carries on past a failure, but only until
// maxFailures machines have failed.
func TestFailureLimitStopsContinueRollout(t *testing.T) {
	h := newHarness(t)

	limit := int32(2)
	parent, children := h.retentionFixture(&unboundedv1alpha3.MachineOperationRollout{
		OnFailure:   unboundedv1alpha3.RolloutFailurePolicyContinue,
		MaxFailures: &limit,
	})

	// One at a time: the first failure does not stop it.
	h.failChild(children[0], "boom")
	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseInProgress)
	require.False(t, h.getChild(children[1]).Spec.Suspend, "the rollout continues after one failure")

	h.failChild(children[1], "boom")
	h.reconcile(parent.Name)

	stopped := h.getParent(parent.Name)
	requirePhase(t, stopped, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, stopped, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonFailureLimitReached)
	requireSummary(t, stopped, 3, 1, 0, 0, 2)
	require.True(t, h.getChild(children[2]).Spec.Suspend, "nothing is released once the limit is reached")

	finished := h.settle(parent.Name)

	require.Equal(t, []string{children[0], children[1]}, h.childNames(parent))
	requireChildrenPruned(t, finished)
}

// A finished parent keeps a bounded number of failed children, the first by
// machine name. Its counts still say how many failed.
func TestFailedChildrenKeptUpToLimit(t *testing.T) {
	h := newHarness(t)
	h.reconciler.keepFailed = 2

	parent, children := h.retentionFixture(allAtOnce(unboundedv1alpha3.RolloutFailurePolicyContinue, ""))

	for _, child := range children {
		h.failChild(child, "boom")
	}

	h.reconcile(parent.Name)

	failed := h.getParent(parent.Name)
	requireCompletedCondition(t, failed, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonTargetFailed)

	finished := h.settle(parent.Name)

	require.Equal(t, []string{children[0], children[1]}, h.childNames(parent))
	requireSummary(t, finished, 3, 0, 0, 0, 3)
	requireChildrenPruned(t, finished)
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
		unboundedv1alpha3.MachineOperationConditionChildrenPruned))
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
	requireChildrenPruned(t, finished)
}

func TestChildrenPrunedInBatches(t *testing.T) {
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
	requireChildrenPruned(t, finished)
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
	requireChildrenPruned(t, finished)
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

	// An administrator releases a held child after the rollout stopped,
	// before it has been removed.
	late := h.getChild(held[0])
	late.Spec.Suspend = false
	require.NoError(t, h.client.Update(h.ctx, late))

	// The other held child goes; the released one is waited for.
	var result ctrl.Result

	for range 5 {
		if result = h.reconcile(parent.Name); result.RequeueAfter == runningChildRecheck {
			break
		}
	}

	require.Equal(t, runningChildRecheck, result.RequeueAfter)
	require.ElementsMatch(t, []string{released[0], held[0]}, h.childNames(parent))
	require.Nil(t, apimeta.FindStatusCondition(h.getParent(parent.Name).Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionChildrenPruned))

	h.completeChild(held[0])

	finished := h.settle(parent.Name)

	require.Equal(t, []string{released[0]}, h.childNames(parent))
	requirePhase(t, finished, unboundedv1alpha3.OperationPhaseFailed)
	requireChildrenPruned(t, finished)
}

func TestParentThatMatchedNothingHasNoRemovalCondition(t *testing.T) {
	h := newHarness(t)

	parent := h.createParent(h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: map[string]string{"fanout.test/group": h.prefix + "-none"}}, nil, nil, nil)

	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseFailed)

	finished := h.settle(parent.Name)

	require.Nil(t, apimeta.FindStatusCondition(finished.Status.Conditions,
		unboundedv1alpha3.MachineOperationConditionChildrenPruned))
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

func TestHasBootMedia(t *testing.T) {
	t.Parallel()

	media := &unboundedv1alpha3.MachineOperationBootMediaStatus{ID: "media-1"}

	tests := []struct {
		name    string
		targets []unboundedv1alpha3.MachineOperationTargetStatus
		want    bool
	}{
		{name: "no targets"},
		{name: "target without media", targets: []unboundedv1alpha3.MachineOperationTargetStatus{{MachineRef: "a"}}},
		{name: "target with media", targets: []unboundedv1alpha3.MachineOperationTargetStatus{{MachineRef: "a", BootMedia: media}}, want: true},
		{
			name: "second target with media",
			targets: []unboundedv1alpha3.MachineOperationTargetStatus{
				{MachineRef: "a"},
				{MachineRef: "b", BootMedia: media},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			child := &unboundedv1alpha3.MachineOperation{
				Status: unboundedv1alpha3.MachineOperationStatus{Targets: tt.targets},
			}
			require.Equal(t, tt.want, hasBootMedia(child))
		})
	}
}
