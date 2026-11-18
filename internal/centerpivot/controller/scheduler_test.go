// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/centerpivot"
)

// checking returns the nodes the scheduler gave a slot.
func checking(t *testing.T, c client.Client, names ...string) []string {
	t.Helper()

	var got []string

	for _, name := range names {
		if stateOf(t, c, name).Phase == PhaseChecking {
			got = append(got, name)
		}
	}

	return got
}

func schedule(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	c := testClient(t, objects...)
	scheduler := &Scheduler{Client: c, Clock: clocktesting.NewFakePassiveClock(start)}
	require.NoError(t, scheduler.Schedule(t.Context()))

	return c
}

func TestScheduleGivesFreeSlotsCanariesFirst(t *testing.T) {
	t.Parallel()

	checkedLongAgo := &State{Phase: PhaseIdle, LastCheck: start.Add(-3 * time.Hour)}
	checkedRecently := &State{Phase: PhaseIdle, LastCheck: start.Add(-10 * time.Minute)}

	notReady := testNode(t, "not-ready", "edge", running)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse

	canary := testNode(t, "canary", "edge", running)
	canary.Labels[centerpivot.CanaryLabel] = "true"

	paused := testNode(t, "paused", "edge", running)
	paused.Labels[centerpivot.PausedLabel] = "true"

	unmanaged := testNode(t, "unmanaged", "edge", running)
	delete(unmanaged.Labels, centerpivot.ManagedLabel)

	// 50% of 6 managed nodes is 3 slots. The node that isn't Ready uses one,
	// so two are free: the canary first, then the node checked longest ago.
	c := schedule(
		t,
		testSite("edge", ptr.To(intstr.FromString("50%"))),
		notReady, testMachine(t, "not-ready", nil),
		testNode(t, "old", "edge", running), testMachine(t, "old", checkedLongAgo),
		testNode(t, "new", "edge", running), testMachine(t, "new", nil),
		canary, testMachine(t, "canary", &State{Phase: PhaseIdle, LastCheck: start.Add(-2 * time.Hour)}),
		paused, testMachine(t, "paused", nil),
		testNode(t, "recent", "edge", running), testMachine(t, "recent", checkedRecently),
		unmanaged, testMachine(t, "unmanaged", nil),
	)

	assert.ElementsMatch(t, []string{"canary", "new"}, checking(t, c, "not-ready", "old", "new", "canary", "paused", "recent", "unmanaged"),
		"the canary, then the node never checked")
}

func TestScheduleCountsUnavailableNodes(t *testing.T) {
	t.Parallel()

	cordoned := testNode(t, "cordoned", "edge", running)
	cordoned.Spec.Unschedulable = true

	for name, unavailable := range map[string][]client.Object{
		"updating":  {testNode(t, "busy", "edge", running), testMachine(t, "busy", &State{Phase: PhaseRestarting})},
		"cordoned":  {cordoned, testMachine(t, "cordoned", nil)},
		"operation": {testNode(t, "busy", "edge", running), testMachine(t, "busy", nil), machineOperation("busy")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := schedule(t, append(unavailable, testSite("edge", nil),
				testNode(t, "due", "edge", running), testMachine(t, "due", nil))...)

			assert.Empty(t, checking(t, c, "due"), "the only slot is in use")
		})
	}
}

func TestScheduleStopsWhileANodeIsHeld(t *testing.T) {
	t.Parallel()

	c := schedule(
		t,
		testSite("edge", nil), testSite("core", nil),
		testNode(t, "due", "edge", running), testMachine(t, "due", nil),
		testNode(t, "held", "core", running), testMachine(t, "held", &State{Phase: PhaseHeld, Reason: "boot failed"}),
	)

	assert.Empty(t, checking(t, c, "due"), "a node held in any Site stops every Site")
}

func TestScheduleIgnoresSitesWithoutCenterPivot(t *testing.T) {
	t.Parallel()

	site := testSite("edge", nil)
	site.Spec.Components.CenterPivot.Enabled = ptr.To(false)

	c := schedule(t, site, testNode(t, "due", "edge", running), testMachine(t, "due", nil))

	assert.Empty(t, checking(t, c, "due"))
}

func machineOperation(machine string) *unboundedv1alpha3.MachineOperation {
	return &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "reboot-" + machine},
		Spec:       unboundedv1alpha3.MachineOperationSpec{MachineRef: machine, OperationKind: "Reboot"},
		Status:     unboundedv1alpha3.MachineOperationStatus{Phase: unboundedv1alpha3.OperationPhaseInProgress},
	}
}
