// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestNoMatchingMachines(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	h.createMachine(h.prefix+"-other", map[string]string{"fanout.test/group": group + "-other"})

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: map[string]string{"fanout.test/group": group}},
		nil,
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonNoMachinesMatched)
	require.NotNil(t, current.Status.CompletedAt)
	require.Nil(t, current.Status.Summary)
	require.Empty(t, current.Status.Targets)
	require.Empty(t, h.children(current))

	h.createMachine(h.prefix+"-late", map[string]string{"fanout.test/group": group})
	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	require.Empty(t, current.Status.Targets)
	require.Empty(t, h.children(current))
	require.False(t, h.childExists(childName(parent, h.prefix+"-late")))
}

func TestInvalidSelector(t *testing.T) {
	h := newHarness(t)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "fanout.test/group",
				Operator: metav1.LabelSelectorOperator("Invalid"),
			}},
		},
		nil,
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, "InvalidSelector")
	require.NotNil(t, current.Status.CompletedAt)
	require.Nil(t, current.Status.Summary)
	require.Empty(t, current.Status.Targets)
	require.Empty(t, h.children(current))
}

func TestSelectorSnapshotIsOneTime(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	machineC := h.createMachine(h.prefix+"-c", labels)
	machineA := h.createMachine(h.prefix+"-a", labels)
	machineB := h.createMachine(h.prefix+"-b", labels)
	h.createDeletingMachine(h.prefix+"-deleting", labels)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		nil,
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseInProgress)
	requireCompletedCondition(t, current, metav1.ConditionFalse, "InProgress")
	requireTargetsResolved(t, current)
	requireSummary(t, current, 3, 2, 1, 0, 0)
	require.NotNil(t, current.Status.StartedAt)
	require.Empty(t, current.Status.Targets)

	require.Equal(t, []string{
		childName(parent, machineA.Name),
		childName(parent, machineB.Name),
		childName(parent, machineC.Name),
	}, h.childNames(current))

	for _, name := range []string{machineA.Name, machineB.Name, machineC.Name} {
		child := h.childForMachine(parent, name)
		require.Equal(t, name == machineA.Name, !child.Spec.Suspend)
	}

	late := h.createMachine(h.prefix+"-late", labels)
	h.reconcile(parent.Name)

	beforeNoop := h.getParent(parent.Name)
	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	require.Equal(t, beforeNoop.ResourceVersion, current.ResourceVersion)
	requireSummary(t, current, 3, 2, 1, 0, 0)
	require.Equal(t, []string{
		childName(parent, machineA.Name),
		childName(parent, machineB.Name),
		childName(parent, machineC.Name),
	}, h.childNames(current))
	require.False(t, h.childExists(childName(parent, late.Name)))
}

func TestResolutionInBatches(t *testing.T) {
	h := newHarness(t)
	h.reconciler.BatchSize = 2
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	machines := make([]*unboundedv1alpha3.Machine, 0, 5)
	for i := range 5 {
		machines = append(machines, h.createMachine(fmt.Sprintf("%s-%c", h.prefix, 'a'+i), labels))
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseInProgress)
	requireNotResolved(t, current)
	require.Equal(t, "creating child operations: 2 of 5", current.Status.Message)
	require.Len(t, h.children(current), 2)
	require.NotNil(t, current.Status.StartedAt)

	// A Machine that already has a child stays in the set even when its
	// label changes or the Machine is deleted.
	machines[0].Labels = map[string]string{"fanout.test/group": group + "-moved"}
	require.NoError(t, h.client.Update(h.ctx, machines[0]))
	require.NoError(t, h.client.Delete(h.ctx, machines[1]))

	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requireNotResolved(t, current)
	require.Equal(t, "creating child operations: 4 of 5", current.Status.Message)
	require.Len(t, h.children(current), 4)
	require.True(t, h.childExists(childName(parent, machines[0].Name)))
	require.True(t, h.childExists(childName(parent, machines[1].Name)))

	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requireTargetsResolved(t, current)
	requireSummary(t, current, 5, 4, 1, 0, 0)
	require.Len(t, h.children(current), 5)
}

func TestDefaultRolloutOneAtATime(t *testing.T) {
	tests := map[string]*unboundedv1alpha3.MachineOperationRollout{
		"unset": nil,
		"one": {
			MaxConcurrent: intOrString(intstr.FromInt(1)),
		},
	}

	for name, rollout := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			group := h.prefix + "-group"
			labels := map[string]string{"fanout.test/group": group}

			machines := []*unboundedv1alpha3.Machine{
				h.createMachine(h.prefix+"-a", labels),
				h.createMachine(h.prefix+"-b", labels),
				h.createMachine(h.prefix+"-c", labels),
			}

			parent := h.createParent(
				h.prefix+"-parent",
				&metav1.LabelSelector{MatchLabels: labels},
				rollout,
				nil,
				nil,
			)

			h.reconcile(parent.Name)

			first := childName(parent, machines[0].Name)
			second := childName(parent, machines[1].Name)
			third := childName(parent, machines[2].Name)

			current := h.getParent(parent.Name)
			requireSummary(t, current, 3, 2, 1, 0, 0)
			require.Equal(t, []string{first, second, third}, h.childNames(current))
			require.False(t, h.getChild(first).Spec.Suspend)
			require.True(t, h.getChild(second).Spec.Suspend)
			require.True(t, h.getChild(third).Spec.Suspend)

			beforeNoop := h.getParent(parent.Name)
			h.reconcile(parent.Name)
			require.Equal(t, beforeNoop.ResourceVersion, h.getParent(parent.Name).ResourceVersion)

			h.completeChild(first)
			h.reconcile(parent.Name)

			current = h.getParent(parent.Name)
			requireSummary(t, current, 3, 1, 1, 1, 0)
			require.False(t, h.getChild(second).Spec.Suspend)
			require.True(t, h.getChild(third).Spec.Suspend)

			h.completeChild(second)
			h.reconcile(parent.Name)
			requireSummary(t, h.getParent(parent.Name), 3, 0, 1, 2, 0)
			require.False(t, h.getChild(third).Spec.Suspend)

			h.completeChild(third)
			h.reconcile(parent.Name)

			current = h.getParent(parent.Name)
			requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)
			requireCompletedCondition(t, current, metav1.ConditionTrue, "Succeeded")
			requireSummary(t, current, 3, 0, 0, 3, 0)
		})
	}
}

func TestMaxConcurrent(t *testing.T) {
	tests := map[string]struct {
		maxConcurrent *intstr.IntOrString
		want          int
	}{
		"integer two": {
			maxConcurrent: intOrString(intstr.FromInt(2)),
			want:          2,
		},
		"fifty percent of five": {
			maxConcurrent: intOrString(intstr.FromString("50%")),
			want:          3,
		},
		"one hundred percent": {
			maxConcurrent: intOrString(intstr.FromString("100%")),
			want:          5,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			group := h.prefix + "-group"
			labels := map[string]string{"fanout.test/group": group}

			names := make([]string, 0, 5)

			for i := range 5 {
				machineName := fmt.Sprintf("%s-%d", h.prefix, i)
				h.createMachine(machineName, labels)
				names = append(names, machineName)
			}

			sort.Strings(names)

			parent := h.createParent(
				h.prefix+"-parent",
				&metav1.LabelSelector{MatchLabels: labels},
				&unboundedv1alpha3.MachineOperationRollout{MaxConcurrent: test.maxConcurrent},
				nil,
				nil,
			)

			writer := &countingWriter{Client: h.client}
			h.reconciler.Writer = writer

			h.reconcile(parent.Name)

			current := h.getParent(parent.Name)
			requireSummary(t, current, 5, 5-test.want, test.want, 0, 0)

			for i, machineName := range names {
				child := h.childForMachine(parent, machineName)
				require.Equal(t, i < test.want, !child.Spec.Suspend, machineName)
			}

			for range 20 {
				children := h.children(h.getParent(parent.Name))

				for i := range children {
					if children[i].Status.IsTerminal() || children[i].IsHeld() {
						continue
					}

					h.completeChild(children[i].Name)
				}

				h.reconcile(parent.Name)

				current = h.getParent(parent.Name)
				require.LessOrEqual(t, releaseCount(h.children(current)), test.want)
				require.Equal(t,
					int(current.Status.Summary.Total),
					int(current.Status.Summary.Held+current.Status.Summary.Active+
						current.Status.Summary.Complete+current.Status.Summary.Failed))

				if current.Status.IsTerminal() {
					break
				}
			}

			current = h.getParent(parent.Name)
			requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)
			requireSummary(t, current, 5, 0, 0, 5, 0)

			_, patches := writer.counts()
			if test.want == 5 {
				require.Zero(t, patches)
			} else {
				require.NotZero(t, patches)
			}
		})
	}
}

func TestReleaseOrderAndStaleReader(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	machineB := h.createMachine(h.prefix+"-b", labels)
	machineC := h.createMachine(h.prefix+"-c", labels)
	machineA := h.createMachine(h.prefix+"-a", labels)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{MaxConcurrent: intOrString(intstr.FromInt(1))},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	require.False(t, h.childForMachine(parent, machineA.Name).Spec.Suspend)
	require.True(t, h.childForMachine(parent, machineB.Name).Spec.Suspend)
	require.True(t, h.childForMachine(parent, machineC.Name).Spec.Suspend)

	h.completeChild(childName(parent, machineA.Name))

	snapshot := h.children(h.getParent(parent.Name))

	h.reconcile(parent.Name)

	released := h.childForMachine(parent, machineB.Name)
	require.False(t, released.Spec.Suspend)
	require.Equal(t, h.now.Format(time.RFC3339),
		released.Annotations[unboundedv1alpha3.MachineOperationReleasedAtAnnotation])
	require.True(t, h.childForMachine(parent, machineC.Name).Spec.Suspend)

	h.reconciler.Reader = staleChildrenReader{Client: h.client, snapshot: snapshot}
	h.reconciler.APIReader = h.client

	h.reconcile(parent.Name)

	require.False(t, h.childForMachine(parent, machineB.Name).Spec.Suspend)
	require.True(t, h.childForMachine(parent, machineC.Name).Spec.Suspend)
	require.NotContains(t,
		h.childForMachine(parent, machineC.Name).Annotations,
		unboundedv1alpha3.MachineOperationReleasedAtAnnotation)
}

func TestStopRolloutOnFailure(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	names := make([]string, 0, 5)

	for i := range 5 {
		machineName := fmt.Sprintf("%s-%d", h.prefix, i)
		h.createMachine(machineName, labels)
		names = append(names, machineName)
	}

	sort.Strings(names)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromInt(2)),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyStop,
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	first := childName(parent, names[0])
	second := childName(parent, names[1])
	third := childName(parent, names[2])
	fourth := childName(parent, names[3])
	fifth := childName(parent, names[4])

	requireSummary(t, h.getParent(parent.Name), 5, 3, 2, 0, 0)

	h.failChild(first, "boom")
	h.runChild(second, "working")
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseInProgress)
	requireCompletedCondition(t, current, metav1.ConditionFalse, "InProgress")
	requireSummary(t, current, 5, 3, 1, 0, 1)
	require.True(t, h.getChild(third).Spec.Suspend)
	require.True(t, h.getChild(fourth).Spec.Suspend)
	require.True(t, h.getChild(fifth).Spec.Suspend)

	h.completeChild(second)
	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonRolloutStopped)
	requireSummary(t, current, 5, 3, 0, 1, 1)

	for _, name := range []string{third, fourth, fifth} {
		child := h.getChild(name)
		require.True(t, child.Spec.Suspend)
		require.NotContains(t, child.Annotations, unboundedv1alpha3.MachineOperationReleasedAtAnnotation)
	}
}

func TestContinueRolloutAfterFailure(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	names := make([]string, 0, 5)

	for i := range 5 {
		machineName := fmt.Sprintf("%s-%d", h.prefix, i)
		h.createMachine(machineName, labels)
		names = append(names, machineName)
	}

	sort.Strings(names)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromInt(2)),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	children := make([]string, 0, 5)
	for _, name := range names {
		children = append(children, childName(parent, name))
	}

	h.failChild(children[0], "boom")
	h.reconcile(parent.Name)
	require.False(t, h.getChild(children[2]).Spec.Suspend)

	h.completeChild(children[1])
	h.reconcile(parent.Name)
	require.False(t, h.getChild(children[3]).Spec.Suspend)

	h.completeChild(children[2])
	h.reconcile(parent.Name)
	require.False(t, h.getChild(children[4]).Spec.Suspend)

	h.completeChild(children[3])
	h.reconcile(parent.Name)
	h.completeChild(children[4])
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonTargetFailed)
	requireSummary(t, current, 5, 0, 0, 4, 1)
	require.Equal(t, []string{names[0]}, current.Status.Summary.FailedMachines)
	require.Len(t, h.childNames(current), 5)
}

func TestContinueRolloutWithoutFailuresCompletes(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	machines := []*unboundedv1alpha3.Machine{
		h.createMachine(h.prefix+"-a", labels),
		h.createMachine(h.prefix+"-b", labels),
		h.createMachine(h.prefix+"-c", labels),
	}

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromInt(2)),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	for range machines {
		for _, child := range h.children(h.getParent(parent.Name)) {
			if !child.Status.IsTerminal() && !child.IsHeld() {
				h.completeChild(child.Name)
			}
		}

		h.reconcile(parent.Name)
	}

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)
	requireCompletedCondition(t, current, metav1.ConditionTrue, "Succeeded")
	requireSummary(t, current, 3, 0, 0, 3, 0)
}

func TestParentCompletesOnlyAfterEveryChild(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	machines := []*unboundedv1alpha3.Machine{
		h.createMachine(h.prefix+"-a", labels),
		h.createMachine(h.prefix+"-b", labels),
		h.createMachine(h.prefix+"-c", labels),
	}

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	children := make([]string, 0, len(machines))
	for i := range machines {
		children = append(children, childName(parent, machines[i].Name))
	}

	h.completeChild(children[0])
	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseInProgress)

	h.completeChild(children[1])
	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseInProgress)

	h.completeChild(children[2])
	h.reconcile(parent.Name)

	finished := h.getParent(parent.Name)
	requirePhase(t, finished, unboundedv1alpha3.OperationPhaseComplete)
	requireCompletedCondition(t, finished, metav1.ConditionTrue, "Succeeded")
	requireSummary(t, finished, 3, 0, 0, 3, 0)

	h.failChild(children[0], "too late")
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	require.Equal(t, finished.Status, current.Status)
	require.Equal(t, finished.ResourceVersion, current.ResourceVersion)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)
	require.Empty(t, current.Status.Targets)
}

func TestPickupTimeout(t *testing.T) {
	h := newHarness(t)
	h.reconciler.PickupTimeout = 10 * time.Minute
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	names := make([]string, 0, 5)

	for i := range 5 {
		machineName := fmt.Sprintf("%s-%d", h.prefix, i)
		h.createMachine(machineName, labels)
		names = append(names, machineName)
	}

	sort.Strings(names)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromInt(3)),
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	first := childName(parent, names[0])
	second := childName(parent, names[1])
	third := childName(parent, names[2])
	fourth := childName(parent, names[3])
	fifth := childName(parent, names[4])

	// A child created released carries the time, like one released later.
	require.NotEmpty(t, h.getChild(second).Annotations[unboundedv1alpha3.MachineOperationReleasedAtAnnotation])

	h.completeChild(first)
	h.reconcile(parent.Name)

	require.NotEmpty(t, h.getChild(fourth).Annotations[unboundedv1alpha3.MachineOperationReleasedAtAnnotation])

	h.updateChild(third, func(child *unboundedv1alpha3.MachineOperation) {
		child.Status.Message = "working"
	})
	h.reconcile(parent.Name)

	// The second and fourth were released at h.now and an executor last took
	// a child at h.now, so both are due ten minutes from it.
	deadline := h.now.Add(10 * time.Minute)

	h.now = deadline.Add(-time.Second)
	result := h.reconcile(parent.Name)
	require.GreaterOrEqual(t, result.RequeueAfter, time.Second)
	require.Empty(t, h.getChild(second).Status.Phase)

	h.now = deadline.Add(time.Second)
	h.reconcile(parent.Name)

	secondChild := h.getChild(second)
	requirePhase(t, secondChild, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, secondChild, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonNotPickedUp)
	require.Contains(t, secondChild.Status.Message, "no agent or provider controller picked it up")
	require.NotNil(t, secondChild.Status.CompletedAt)

	fourthChild := h.getChild(fourth)
	requirePhase(t, fourthChild, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, fourthChild, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonNotPickedUp)

	thirdChild := h.getChild(third)
	require.Empty(t, thirdChild.Status.Phase)
	require.Equal(t, "working", thirdChild.Status.Message)

	fifthChild := h.getChild(fifth)
	require.Empty(t, fifthChild.Status.Phase)
	require.True(t, fifthChild.Spec.Suspend)
}

func TestChildShape(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machine := h.createMachine(h.prefix+"-machine", labels)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		nil,
		nil,
		map[string]string{"service": "unbounded-agent"},
	)

	h.reconcile(parent.Name)

	parent = h.getParent(parent.Name)
	name := childName(parent, machine.Name)
	child := h.getChild(name)

	require.Equal(t, name, child.Name)
	require.Equal(t, string(parent.UID), child.Labels[unboundedv1alpha3.MachineOperationParentUIDLabel])
	require.Len(t, child.OwnerReferences, 1)

	owner := child.OwnerReferences[0]
	require.Equal(t, "MachineOperation", owner.Kind)
	require.Equal(t, unboundedv1alpha3.GroupVersion.String(), owner.APIVersion)
	require.Equal(t, parent.Name, owner.Name)
	require.Equal(t, parent.UID, owner.UID)
	require.NotNil(t, owner.Controller)
	require.True(t, *owner.Controller)
	require.NotNil(t, owner.BlockOwnerDeletion)
	require.False(t, *owner.BlockOwnerDeletion)

	require.Equal(t, machine.Name, child.Spec.MachineRef)
	require.False(t, child.Spec.Suspend)
	require.Equal(t, unboundedv1alpha3.OperationNodeReboot, child.Spec.OperationKind)
	require.Equal(t, map[string]string{"service": "unbounded-agent"}, child.Spec.Parameters)
	require.Nil(t, child.Spec.MachineSelector)
	require.Nil(t, child.Spec.Rollout)
	require.Nil(t, child.Spec.TTLSecondsAfterFinished)
	require.Empty(t, parent.Status.Targets)
	requireSummary(t, parent, 1, 0, 1, 0, 0)
}

func TestLongChildName(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machine := h.createMachine(strings.Repeat("m", 20)+fmt.Sprintf("-%d", testNameIndex.Add(1)), labels)
	parentName := strings.Repeat("p", 230) + fmt.Sprintf("-%d", testNameIndex.Add(1))

	parent := h.createParent(
		parentName,
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	require.Greater(t, len(parent.Name)+1+len(machine.Name), maxChildNameLength)

	current := h.getParent(parent.Name)
	child := childName(current, machine.Name)

	require.LessOrEqual(t, len(child), maxChildNameLength)
	require.True(t, strings.HasPrefix(child, strings.Repeat("p", 230)))
	require.Regexp(t, regexp.MustCompile(`[0-9a-z]-[0-9a-f]{10}$`), child)
	require.True(t, h.childExists(child))
	requireSummary(t, current, 1, 0, 1, 0, 0)
}

func TestForeignChildNameReturnsError(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machines := []*unboundedv1alpha3.Machine{
		h.createMachine(h.prefix+"-a", labels),
		h.createMachine(h.prefix+"-b", labels),
	}

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	foreignName := childName(parent, machines[0].Name)
	foreign := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:   foreignName,
			Labels: map[string]string{"fanout.test/foreign": "true"},
		},
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineRef:    "foreign-machine",
			OperationKind: unboundedv1alpha3.OperationAgentReset,
		},
	}
	require.NoError(t, h.client.Create(h.ctx, foreign))

	_, err := h.reconciler.Reconcile(h.ctx, ctrl.Request{
		NamespacedName: client.ObjectKey{Name: parent.Name},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "taken")

	var currentForeign unboundedv1alpha3.MachineOperation
	require.NoError(t, h.client.Get(h.ctx, client.ObjectKey{Name: foreignName}, &currentForeign))
	require.Equal(t, foreign.UID, currentForeign.UID)
	require.Equal(t, foreign.Spec, currentForeign.Spec)
	require.False(t, currentForeign.Status.IsTerminal())
	require.Empty(t, currentForeign.OwnerReferences)
	require.Equal(t, "true", currentForeign.Labels["fanout.test/foreign"])
}

func TestTTLDeletesFinishedParent(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machine := h.createMachine(h.prefix+"-machine", labels)
	ttl := int32(60)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
		},
		&ttl,
		nil,
	)

	h.reconcile(parent.Name)
	h.completeChild(childName(parent, machine.Name))
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)
	require.NotNil(t, current.Status.CompletedAt)

	expires := current.Status.CompletedAt.Add(time.Duration(ttl) * time.Second)
	h.now = expires.Add(-time.Second)

	result := h.reconcile(parent.Name)
	require.Equal(t, time.Second, result.RequeueAfter)
	require.NotNil(t, h.getParent(parent.Name))

	h.now = expires
	h.reconcile(parent.Name)

	var deleted unboundedv1alpha3.MachineOperation

	err := h.client.Get(h.ctx, client.ObjectKey{Name: parent.Name}, &deleted)
	require.True(t, apierrors.IsNotFound(err))
}

func TestAddToManager(t *testing.T) {
	requireAPIServer(t)

	h := newHarness(t)

	mgr, err := ctrl.NewManager(testRESTConfig, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Controller:             ctrlconfig.Controller{SkipNameValidation: &[]bool{true}[0]},
	})
	require.NoError(t, err)
	require.NoError(t, AddToManager(mgr, Options{PickupTimeout: 5 * time.Minute}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- mgr.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})

	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machine := h.createMachine(h.prefix+"-machine", labels)
	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		nil,
		nil,
		nil,
	)

	require.Eventually(t, func() bool {
		current := h.getParent(parent.Name)

		return h.childExists(childName(parent, machine.Name)) &&
			targetsResolved(current) &&
			current.Status.Summary != nil &&
			current.Status.Summary.Total == 1
	}, 10*time.Second, 100*time.Millisecond)
}

func TestChildEventEnqueuesParent(t *testing.T) {
	h := newHarness(t)
	h.reconciler.MinReconcileInterval = 250 * time.Millisecond

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: map[string]string{"fanout.test/group": h.prefix}},
		nil,
		nil,
		nil,
	)
	child := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:            h.prefix + "-child",
			OwnerReferences: []metav1.OwnerReference{childOwnerReference(parent)},
		},
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineRef:    h.prefix + "-machine",
			OperationKind: unboundedv1alpha3.OperationNodeReboot,
		},
	}

	handler := h.reconciler.eventHandler()

	parentQueue := &recordingQueue{}

	handler.CreateFunc(h.ctx, event.TypedCreateEvent[client.Object]{Object: parent}, parentQueue)
	require.Equal(t, []reconcile.Request{{NamespacedName: client.ObjectKey{Name: parent.Name}}}, parentQueue.added)
	require.Empty(t, parentQueue.delayed)

	childQueue := &recordingQueue{}
	childEvent := event.TypedUpdateEvent[client.Object]{ObjectOld: child, ObjectNew: child}

	handler.UpdateFunc(h.ctx, childEvent, childQueue)
	handler.UpdateFunc(h.ctx, childEvent, childQueue)

	require.Empty(t, childQueue.added)
	require.Len(t, childQueue.delayed, 1)
	require.Equal(t,
		250*time.Millisecond,
		childQueue.delayed[reconcile.Request{NamespacedName: client.ObjectKey{Name: parent.Name}}])
}

func TestDeletedChildCountsAsFailedAndIsNotRecreated(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machines := []*unboundedv1alpha3.Machine{
		h.createMachine(h.prefix+"-a", labels),
		h.createMachine(h.prefix+"-b", labels),
	}

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	first := childName(parent, machines[0].Name)
	h.completeChild(childName(parent, machines[1].Name))

	require.NoError(t, h.client.Delete(h.ctx, h.getChild(first)))

	// The controller created the child a moment ago and has not seen it
	// listed, so for a while it takes the child's absence for a cache that is
	// behind. After that the child counts as deleted.
	h.reconcile(parent.Name)
	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseInProgress)

	h.now = h.now.Add(2 * expectationTTL)
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	require.False(t, h.childExists(first))
	require.NotContains(t, h.childNames(current), first)
	requireSummary(t, current, 2, 0, 0, 1, 1)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonTargetFailed)
}

func TestChildDeletedAfterItFinishedCountsAsFailed(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}
	machines := []*unboundedv1alpha3.Machine{
		h.createMachine(h.prefix+"-a", labels),
		h.createMachine(h.prefix+"-b", labels),
	}

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	first := childName(parent, machines[0].Name)
	second := childName(parent, machines[1].Name)

	h.completeChild(first)
	h.reconcile(parent.Name)
	require.NoError(t, h.client.Delete(h.ctx, h.getChild(first)))

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	require.False(t, h.childExists(first))
	requireSummary(t, current, 2, 0, 1, 0, 1)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseInProgress)

	h.completeChild(second)
	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonTargetFailed)
	requireSummary(t, current, 2, 0, 0, 1, 1)
}

func TestChildOwnerReferenceDoesNotBlockOwnerDeletion(t *testing.T) {
	parent := &unboundedv1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "parent", UID: "uid-1"}}

	reference := childOwnerReference(parent)

	require.Equal(t, "MachineOperation", reference.Kind)
	require.Equal(t, "parent", reference.Name)
	require.NotNil(t, reference.Controller)
	require.True(t, *reference.Controller)
	require.NotNil(t, reference.BlockOwnerDeletion)
	require.False(t, *reference.BlockOwnerDeletion)
}

func TestSummaryCountsAtEachStepAndFailedMachines(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	names := make([]string, 0, 12)

	for i := range 12 {
		machineName := fmt.Sprintf("%s-%02d", h.prefix, i)
		h.createMachine(machineName, labels)
		names = append(names, machineName)
	}

	sort.Strings(names)

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requireSummary(t, current, 12, 0, 12, 0, 0)

	for i := len(names) - 1; i >= 0; i-- {
		h.failChild(childName(parent, names[i]), "boom")
	}

	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonTargetFailed)
	requireSummary(t, current, 12, 0, 0, 0, 12)

	expected := append([]string(nil), names[:10]...)
	sort.Strings(expected)
	require.Equal(t, expected, current.Status.Summary.FailedMachines)
	require.Len(t, current.Status.Summary.FailedMachines, 10)
	require.Empty(t, current.Status.Targets)
}

func TestParentStatusNotRewrittenWhenUnchanged(t *testing.T) {
	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	machines := []*unboundedv1alpha3.Machine{
		h.createMachine(h.prefix+"-a", labels),
		h.createMachine(h.prefix+"-b", labels),
		h.createMachine(h.prefix+"-c", labels),
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)

	writer := &countingWriter{Client: h.client}
	h.reconciler.Writer = writer

	h.reconcile(parent.Name)

	before, _ := writer.counts()
	require.NotZero(t, before)

	stable := h.getParent(parent.Name)
	h.reconcile(parent.Name)

	after, _ := writer.counts()
	require.Equal(t, before, after)
	require.Equal(t, stable.ResourceVersion, h.getParent(parent.Name).ResourceVersion)

	h.failChild(childName(parent, machines[0].Name), "boom")
	h.reconcile(parent.Name)

	failedWrites, _ := writer.counts()
	require.Greater(t, failedWrites, after)

	requirePhase(t, h.getParent(parent.Name), unboundedv1alpha3.OperationPhaseFailed)

	// One more write records that the completed children are gone.
	h.reconcile(parent.Name)

	settledWrites, _ := writer.counts()
	terminal := h.getParent(parent.Name)
	requireCompletedChildrenRemoved(t, terminal)

	h.reconcile(parent.Name)
	h.reconcile(parent.Name)

	finalWrites, _ := writer.counts()
	require.Equal(t, settledWrites, finalWrites)
	require.Equal(t, terminal.Status, h.getParent(parent.Name).Status)
}

func TestObjectSizeStaysBounded(t *testing.T) {
	if testRESTConfig != nil {
		t.Skip("the in-memory client is enough for the object size check")
	}

	h := newHarness(t)
	group := h.prefix + "-group"
	labels := map[string]string{"fanout.test/group": group}

	const machineCount = 2000

	for i := range machineCount {
		h.createMachine(fmt.Sprintf("%s-%04d", h.prefix, i), labels)
	}

	parent := h.createParent(
		h.prefix+"-parent",
		&metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		},
		nil,
		nil,
	)

	requireBounded := func(operation *unboundedv1alpha3.MachineOperation) {
		encoded, err := json.Marshal(operation)
		require.NoError(t, err)
		require.Less(t, len(encoded), 16*1024)
	}

	for range 10 {
		h.reconcile(parent.Name)

		current := h.getParent(parent.Name)
		requireBounded(current)

		if targetsResolved(current) {
			break
		}
	}

	current := h.getParent(parent.Name)
	requireTargetsResolved(t, current)
	requireSummary(t, current, machineCount, 0, machineCount, 0, 0)

	for _, child := range h.children(current) {
		h.completeChild(child.Name)
	}

	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseComplete)
	requireSummary(t, current, machineCount, 0, 0, machineCount, 0)
	requireBounded(current)
}

type countingWriter struct {
	client.Client

	mu            sync.Mutex
	statusUpdates int
	patches       int
}

func (w *countingWriter) Status() client.SubResourceWriter {
	return &countingStatusWriter{
		SubResourceWriter: w.Client.Status(),
		writer:            w,
	}
}

func (w *countingWriter) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	w.mu.Lock()
	w.patches++
	w.mu.Unlock()

	return w.Client.Patch(ctx, object, patch, options...)
}

func (w *countingWriter) counts() (statusUpdates, patches int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.statusUpdates, w.patches
}

type countingStatusWriter struct {
	client.SubResourceWriter

	writer *countingWriter
}

func (w *countingStatusWriter) Update(
	ctx context.Context,
	object client.Object,
	options ...client.SubResourceUpdateOption,
) error {
	w.writer.mu.Lock()
	w.writer.statusUpdates++
	w.writer.mu.Unlock()

	return w.SubResourceWriter.Update(ctx, object, options...)
}

type staleChildrenReader struct {
	client.Client

	snapshot []unboundedv1alpha3.MachineOperation
}

func (s staleChildrenReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if operations, ok := list.(*unboundedv1alpha3.MachineOperationList); ok {
		operations.Items = append([]unboundedv1alpha3.MachineOperation(nil), s.snapshot...)
		operations.Continue = ""

		return nil
	}

	return s.Client.List(ctx, list, options...)
}

type recordingQueue struct {
	workqueue.TypedRateLimitingInterface[reconcile.Request]

	added   []reconcile.Request
	delayed map[reconcile.Request]time.Duration
}

func (q *recordingQueue) Add(item reconcile.Request) {
	q.added = append(q.added, item)
}

func (q *recordingQueue) AddAfter(item reconcile.Request, duration time.Duration) {
	if q.delayed == nil {
		q.delayed = map[reconcile.Request]time.Duration{}
	}

	q.delayed[item] = duration
}

// While children are still being created, a Machine that starts to match and
// sorts before the ones already running gets a child, but that child is held:
// the pace counts what is running, not a position in the list.
func TestMachineAppearingDuringCreationDoesNotExceedThePace(t *testing.T) {
	h := newHarness(t)
	h.reconciler.BatchSize = 1

	// A label value holds 63 bytes; the test name alone can be longer.
	labels := map[string]string{"fanout.test/group": fmt.Sprintf("pace-%d", testNameIndex.Add(1))}
	h.createMachine(h.prefix+"-b", labels)
	h.createMachine(h.prefix+"-c", labels)

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)

	// The first batch creates the child for b, released: the pace is one.
	h.reconcile(parent.Name)

	first := h.getChild(childName(parent, h.prefix+"-b"))
	require.False(t, first.Spec.Suspend)
	require.Nil(t, h.getParent(parent.Name).Status.Summary)

	// Before the rest exist, a Machine that sorts first starts to match.
	h.createMachine(h.prefix+"-a", labels)

	h.reconcile(parent.Name)
	h.reconcile(parent.Name)
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	require.NotNil(t, current.Status.Summary)
	require.Equal(t, int32(3), current.Status.Summary.Total)
	require.Equal(t, int32(1), current.Status.Summary.Active)
	require.Equal(t, int32(2), current.Status.Summary.Held)

	require.True(t, h.getChild(childName(parent, h.prefix+"-a")).Spec.Suspend)
	require.True(t, h.getChild(childName(parent, h.prefix+"-c")).Spec.Suspend)
	require.False(t, h.getChild(childName(parent, h.prefix+"-b")).Spec.Suspend)
}

// shortGroup returns labels whose value fits in a label however long the test
// name is.
func shortGroup() map[string]string {
	return map[string]string{"fanout.test/group": fmt.Sprintf("g-%d", testNameIndex.Add(1))}
}

// stale makes the reconciler read children from a snapshot, as a cache that
// has not caught up with later writes would return them. The parent and the
// exact reads still come from the API server.
func (h *harness) stale(snapshot []unboundedv1alpha3.MachineOperation) {
	h.reconciler.Reader = staleChildrenReader{Client: h.client, snapshot: snapshot}
	h.reconciler.APIReader = h.client
}

func (h *harness) fresh() {
	h.reconciler.Reader = h.client
}

// A failure is visible but the release this controller just made is not yet.
// The released child must count as running: the rollout has stopped, but it
// is not over, and the parent must not end while a child is about to run.
func TestStaleReleaseDoesNotEndAStoppedRolloutEarly(t *testing.T) {
	h := newHarness(t)
	labels := shortGroup()

	for _, name := range []string{"a", "b", "c", "d"} {
		h.createMachine(h.prefix+"-"+name, labels)
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{MaxConcurrent: intOrString(intstr.FromInt(2))}, nil, nil)

	h.reconcile(parent.Name)

	a := childName(parent, h.prefix+"-a")
	b := childName(parent, h.prefix+"-b")
	c := childName(parent, h.prefix+"-c")

	h.completeChild(a)
	h.reconcile(parent.Name)
	require.False(t, h.getChild(c).Spec.Suspend, "c was released when a finished")

	// The cache shows b failed, and still shows c held.
	h.failChild(b, "boom")

	snapshot := h.children(parent)
	for i := range snapshot {
		if snapshot[i].Name == c {
			snapshot[i].Spec.Suspend = true
			delete(snapshot[i].Annotations, unboundedv1alpha3.MachineOperationReleasedAtAnnotation)
		}
	}

	h.stale(snapshot)
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseInProgress)
	requireSummary(t, current, 4, 1, 1, 1, 1)

	// c finishes; now the stopped rollout is over, with d never started.
	h.fresh()
	h.completeChild(c)
	h.reconcile(parent.Name)

	current = h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonRolloutStopped)
	requireSummary(t, current, 4, 1, 0, 2, 1)
	require.True(t, h.getChild(childName(parent, h.prefix+"-d")).Spec.Suspend)
}

// While children are still being created, a cache that shows neither the
// children created a moment ago nor the release just made must not lead to
// more children running than the pace allows, or to a child created twice.
func TestStaleCacheDuringCreationKeepsThePace(t *testing.T) {
	h := newHarness(t)
	h.reconciler.BatchSize = 2

	labels := shortGroup()

	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, name := range names {
		h.createMachine(h.prefix+"-"+name, labels)
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)

	// Every reconcile from here on sees no children at all in the cache.
	h.stale(nil)

	for range 6 {
		h.reconcile(parent.Name)

		released := 0

		for _, child := range h.children(parent) {
			if !child.Spec.Suspend && !child.Status.IsTerminal() {
				released++
			}
		}

		require.LessOrEqual(t, released, 1, "the pace is one at a time")
	}

	current := h.getParent(parent.Name)
	require.Len(t, h.children(parent), 6)
	requireSummary(t, current, 6, 5, 1, 0, 0)
	require.False(t, h.getChild(childName(parent, h.prefix+"-a")).Spec.Suspend)
}

// A parent the controller has resolved is not resolved again from a cache
// that still shows it unresolved: the parent is read from the API server.
func TestStaleParentIsNotResolvedAgain(t *testing.T) {
	h := newHarness(t)
	labels := shortGroup()

	h.createMachine(h.prefix+"-b", labels)

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)
	unresolved := parent.DeepCopy()

	h.reconcile(parent.Name)
	requireSummary(t, h.getParent(parent.Name), 1, 0, 1, 0, 0)

	// A Machine starts to match, and the cache still has the old parent.
	h.createMachine(h.prefix+"-a", labels)

	h.reconciler.Reader = staleParentReader{Client: h.client, parent: unresolved}
	h.reconciler.APIReader = h.client
	h.reconcile(parent.Name)

	require.Len(t, h.children(parent), 1)
	requireSummary(t, h.getParent(parent.Name), 1, 0, 1, 0, 0)
}

type staleParentReader struct {
	client.Client

	parent *unboundedv1alpha3.MachineOperation
}

func (s staleParentReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if operation, ok := object.(*unboundedv1alpha3.MachineOperation); ok && key.Name == s.parent.Name {
		s.parent.DeepCopyInto(operation)
		return nil
	}

	return s.Client.Get(ctx, key, object, options...)
}

// A deleted child is a failure like any other: under Stop nothing further is
// released, and the parent ends as stopped with the rest still held.
func TestDeletedChildStopsAStopRollout(t *testing.T) {
	h := newHarness(t)
	labels := shortGroup()

	for _, name := range []string{"a", "b", "c"} {
		h.createMachine(h.prefix+"-"+name, labels)
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)

	h.reconcile(parent.Name)
	h.reconcile(parent.Name)

	a := childName(parent, h.prefix+"-a")
	h.runChild(a, "restarting")
	h.reconcile(parent.Name)

	require.NoError(t, h.client.Delete(h.ctx, h.getChild(a)))
	h.reconcile(parent.Name)

	current := h.getParent(parent.Name)
	requirePhase(t, current, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, current, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonRolloutStopped)
	requireSummary(t, current, 3, 2, 0, 0, 1)
	require.True(t, h.getChild(childName(parent, h.prefix+"-b")).Spec.Suspend)
	require.True(t, h.getChild(childName(parent, h.prefix+"-c")).Spec.Suspend)
}

// A provider controller takes thousands of released children a few at a
// time. The ones still waiting in its queue are not failed as long as it
// keeps taking others; a child nothing serves is failed once it stops.
func TestPickupTimeoutWaitsWhileExecutorsAreTakingChildren(t *testing.T) {
	h := newHarness(t)
	h.reconciler.PickupTimeout = 10 * time.Minute

	labels := shortGroup()

	names := []string{"a", "b", "c", "d"}
	for _, name := range names {
		h.createMachine(h.prefix+"-"+name, labels)
	}

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels},
		&unboundedv1alpha3.MachineOperationRollout{
			MaxConcurrent: intOrString(intstr.FromString("100%")),
			OnFailure:     unboundedv1alpha3.RolloutFailurePolicyContinue,
		}, nil, nil)

	h.reconcile(parent.Name)

	// The executor takes one child every eight minutes: slower than the
	// timeout allows for the last ones, but never idle for ten minutes.
	for _, name := range names[:3] {
		h.now = h.now.Add(8 * time.Minute)
		h.runChild(childName(parent, h.prefix+"-"+name), "working")
		h.reconcile(parent.Name)

		for _, child := range h.children(parent) {
			require.NotEqual(t, unboundedv1alpha3.OperationPhaseFailed, child.Status.Phase, child.Name)
		}
	}

	// Nothing takes the last one. Ten minutes after the last pickup it fails.
	last := childName(parent, h.prefix+"-d")

	h.now = h.now.Add(9 * time.Minute)
	result := h.reconcile(parent.Name)
	require.Empty(t, h.getChild(last).Status.Phase)
	require.Greater(t, result.RequeueAfter, time.Duration(0))

	h.now = h.now.Add(2 * time.Minute)
	h.reconcile(parent.Name)

	failed := h.getChild(last)
	requirePhase(t, failed, unboundedv1alpha3.OperationPhaseFailed)
	requireCompletedCondition(t, failed, metav1.ConditionFalse, unboundedv1alpha3.OperationReasonNotPickedUp)
}

// A child someone released by hand carries no release time from this
// controller, which does not time it out.
func TestHandReleasedChildIsNotTimedOut(t *testing.T) {
	h := newHarness(t)
	labels := shortGroup()

	h.createMachine(h.prefix+"-a", labels)
	h.createMachine(h.prefix+"-b", labels)

	parent := h.createParent(h.prefix+"-parent", &metav1.LabelSelector{MatchLabels: labels}, nil, nil, nil)

	h.reconcile(parent.Name)
	h.runChild(childName(parent, h.prefix+"-a"), "working")
	h.reconcile(parent.Name)

	b := h.getChild(childName(parent, h.prefix+"-b"))
	require.True(t, b.Spec.Suspend)

	b.Spec.Suspend = false
	require.NoError(t, h.client.Update(h.ctx, b))

	h.now = h.now.Add(time.Hour)
	h.reconcile(parent.Name)

	require.Empty(t, h.getChild(b.Name).Status.Phase)
}
