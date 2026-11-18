// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/contract"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/drain"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/omaha"
)

// harness is one ACL node, acl-1, in Site edge, with a fake Nebraska, a fake
// drain, and the test playing the node's agent.
type harness struct {
	t        *testing.T
	client   client.Client
	clock    *clocktesting.FakePassiveClock
	nebraska *fakeNebraska
	drainer  *fakeDrainer
	r        *Reconciler
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		t:        t,
		client:   testClient(t, testSite("edge", nil), testNode(t, "acl-1", "edge", running), testMachine(t, "acl-1", &State{Phase: PhaseChecking})),
		clock:    clocktesting.NewFakePassiveClock(start),
		nebraska: &fakeNebraska{result: omaha.Result{Kind: omaha.Granted, Version: newer}},
		drainer:  &fakeDrainer{workloads: []drain.Workload{{Kind: "Deployment", Namespace: "apps", Name: "web", Available: 3}}},
	}
	h.r = &Reconciler{
		Client: h.client, Reader: h.client, Namespace: "unbounded-system",
		Nebraska: h.nebraska, Drainer: h.drainer, Clock: h.clock, Recorder: events.NewFakeRecorder(100),
	}

	return h
}

// reconcile runs one pass, and returns the node's state after it.
func (h *harness) reconcile() State {
	h.t.Helper()

	_, err := h.r.Reconcile(h.t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "acl-1"}})
	require.NoError(h.t, err)

	return stateOf(h.t, h.client, "acl-1")
}

func (h *harness) node() *corev1.Node {
	h.t.Helper()

	return nodeOf(h.t, h.client, "acl-1")
}

func (h *harness) request() contract.Request {
	h.t.Helper()

	var request contract.Request
	require.NoError(h.t, json.Unmarshal([]byte(h.node().Annotations[Keys.Request]), &request))

	return request
}

func (h *harness) updateNode(change func(*corev1.Node)) {
	h.t.Helper()

	node := h.node()
	change(node)
	require.NoError(h.t, h.client.Update(h.t.Context(), node))
}

// agentAnswers writes the agent's status for its current request under key.
func (h *harness) agentAnswers(key string, code contract.Code) {
	h.t.Helper()

	request := h.request()
	operation := request.Operation

	if key == Keys.CommitStatus {
		operation = contract.OperationCommit
	}

	value, err := json.Marshal(contract.Status{
		SchemaVersion: contract.SchemaVersion, NodeUpdateID: request.NodeUpdateID, OperationID: request.OperationID,
		Operation: operation, Code: code, Message: string(code), StartedUTC: h.clock.Now(), LastUpdatedUTC: h.clock.Now(),
	})
	require.NoError(h.t, err)

	h.updateNode(func(node *corev1.Node) { node.Annotations[key] = string(value) })
}

// agentRestartsInto makes the agent restart the node into version, and report
// its commit with code.
func (h *harness) agentRestartsInto(version string, code contract.Code) {
	h.t.Helper()

	h.agentAnswers(Keys.CommitStatus, code)
	h.updateNode(func(node *corev1.Node) {
		node.Annotations[names.AgentInfoAnnotation] = testNode(h.t, "acl-1", "edge", version).Annotations[names.AgentInfoAnnotation]
	})
}

// setUpdateTimeout changes the Site's Nebraska update timeout.
func (h *harness) setUpdateTimeout(timeout time.Duration) {
	h.t.Helper()

	var site unboundedv1alpha3.Site
	require.NoError(h.t, h.client.Get(h.t.Context(), client.ObjectKey{Name: "edge"}, &site))

	site.Spec.Components.CenterPivot.UpdateTimeout = &metav1.Duration{Duration: timeout}
	require.NoError(h.t, h.client.Update(h.t.Context(), &site))
}

func (h *harness) tainted() bool {
	h.t.Helper()

	return hasTaint(h.node())
}

func (h *harness) wait(d time.Duration) {
	h.clock.SetTime(h.clock.Now().Add(d))
}

// runTo takes the node through a successful update until it reaches phase.
func (h *harness) runTo(phase Phase) State {
	h.t.Helper()

	steps := []struct {
		phase Phase
		agent func()
	}{
		{PhaseStaging, func() {}},
		{PhaseDraining, func() { h.agentAnswers(Keys.Status, contract.CodeSuccess) }},
		{PhaseFinalizing, func() {}},
		{PhaseRestarting, func() { h.agentAnswers(Keys.Status, contract.CodeSuccess) }},
		{PhaseRecovering, func() { h.agentRestartsInto(newer, contract.CodeSuccess) }},
	}

	// Start from the step after the node's phase.
	current := stateOf(h.t, h.client, "acl-1").Phase
	for i, step := range steps {
		if step.phase == current {
			steps = steps[i+1:]

			break
		}
	}

	for _, step := range steps {
		step.agent()

		state := h.reconcile()
		require.Equal(h.t, step.phase, state.Phase, state.Reason)

		if step.phase == phase {
			return state
		}
	}

	h.t.Fatalf("no step reaches %s", phase)

	return State{}
}

func TestAnUpdateFromCheckToIdle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	state := h.runTo(PhaseStaging)
	assert.Equal(t, newer, state.TargetVersion)
	assert.Equal(t, start, state.GrantedAt)
	assert.Equal(t, contract.OperationStage, h.request().Operation)
	assert.Equal(t, newer, h.request().TargetVersion)
	assert.Equal(t, "stable", h.request().Track)
	assert.False(t, h.tainted(), "the node keeps running while it stages")

	require.Len(t, h.nebraska.checks, 1)
	assert.Equal(t, running, h.nebraska.checks[0].Version)
	assert.Equal(t, "ebb9a2bb-9365-d34e-133f-8da0664e7366", h.nebraska.checks[0].MachineID)

	stage := h.request()
	state = h.runTo(PhaseDraining)
	assert.Equal(t, h.drainer.workloads, state.Workloads, "the workloads are recorded before their pods move")
	assert.True(t, h.tainted())

	h.drainer.left = 2
	assert.Equal(t, PhaseDraining, h.reconcile().Phase, "pods left to move")

	h.drainer.left = 0

	h.runTo(PhaseFinalizing)
	assert.Equal(t, contract.OperationFinalize, h.request().Operation)
	assert.Equal(t, stage.NodeUpdateID, h.request().NodeUpdateID, "the finalize continues the staged update")
	assert.True(t, h.tainted())

	h.runTo(PhaseRestarting)

	// Back from the restart, the node waits for its version in agent-info.
	h.agentAnswers(Keys.CommitStatus, contract.CodeSuccess)
	assert.Equal(t, PhaseRestarting, h.reconcile().Phase)

	h.runTo(PhaseRecovering)
	assert.False(t, h.tainted(), "the taint goes before the workloads recover")

	assert.Equal(t, PhaseRecovering, h.reconcile().Phase, "the workloads haven't recovered")

	h.drainer.recovered = true
	state = h.reconcile()
	assert.Equal(t, PhaseRecovering, state.Phase, "the settle time hasn't passed")
	assert.Equal(t, start, state.RecoveredAt)

	h.wait(DefaultSettleTime)

	state = h.reconcile()
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Equal(t, start, state.LastCheck)
	assert.Zero(t, state.Attempts)
}

func TestCheckingGivesTheSlotBack(t *testing.T) {
	t.Parallel()

	t.Run("a node that can't be drained isn't checked", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.drainer.blocked = assert.AnError

		state := h.reconcile()
		assert.Equal(t, PhaseIdle, state.Phase)
		assert.Contains(t, state.Reason, "can't be drained")
		assert.Equal(t, start, state.LastCheck)
		assert.Empty(t, h.nebraska.checks, "no grant was taken")
	})

	t.Run("no update", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.nebraska.result = omaha.Result{Kind: omaha.NoUpdate}

		state := h.reconcile()
		assert.Equal(t, PhaseIdle, state.Phase)
		assert.Equal(t, start, state.LastCheck)
		assert.NotContains(t, h.node().Annotations, Keys.Request)
	})

	t.Run("an update in progress is ended", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.nebraska.result = omaha.Result{Kind: omaha.InProgress}

		assert.Equal(t, PhaseIdle, h.reconcile().Phase)
		assert.Equal(t, contract.OperationStage, h.request().Operation)
		assert.Equal(t, "202606.15.1", h.request().TargetVersion, "a version other than the one the node runs")
	})
}

func TestFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		at        Phase
		fail      func(h *harness)
		want      Phase
		tainted   bool
		attempts  int
		endsGrant bool
	}{
		{
			name: "stage fails",
			at:   PhaseStaging,
			fail: func(h *harness) { h.agentAnswers(Keys.Status, contract.CodeOperationFailed) },
			want: PhaseIdle, attempts: 1,
		},
		{
			name: "already at the target",
			at:   PhaseStaging,
			fail: func(h *harness) { h.agentAnswers(Keys.Status, contract.CodeAlreadyAtTarget) },
			want: PhaseIdle,
		},
		{
			name: "the drain misses its deadline",
			at:   PhaseDraining,
			fail: func(h *harness) { h.drainer.left = 1; h.wait(DefaultDrainDeadline + time.Second) },
			want: PhaseIdle, attempts: 1, endsGrant: true,
		},
		{
			name: "too little of the grant's time is left",
			at:   PhaseDraining,
			fail: func(h *harness) { h.setUpdateTimeout(DefaultRestartDeadline) },
			want: PhaseIdle, endsGrant: true,
		},
		{
			name: "finalize fails",
			at:   PhaseFinalizing,
			fail: func(h *harness) { h.agentAnswers(Keys.Status, contract.CodeOperationFailed) },
			want: PhaseIdle, attempts: 1,
		},
		{
			name: "finalize fails inside the agent",
			at:   PhaseFinalizing,
			fail: func(h *harness) { h.agentAnswers(Keys.Status, contract.CodeAgentInternalError) },
			want: PhaseHeld, tainted: true,
		},
		{
			name: "finalize finds nothing staged",
			at:   PhaseFinalizing,
			fail: func(h *harness) { h.agentAnswers(Keys.Status, contract.CodeNotStaged) },
			want: PhaseStaging,
		},
		{
			name: "the new version doesn't boot",
			at:   PhaseRestarting,
			fail: func(h *harness) { h.agentRestartsInto(running, contract.CodeTargetBootFailed) },
			want: PhaseHeld,
		},
		{
			name: "the node doesn't come back Ready",
			at:   PhaseRestarting,
			fail: func(h *harness) {
				h.agentRestartsInto(newer, contract.CodeSuccess)
				node := h.node()
				node.Status.Conditions[0].Status = corev1.ConditionFalse
				require.NoError(h.t, h.client.Status().Update(h.t.Context(), node))
				h.wait(DefaultRestartDeadline + time.Second)
			},
			want: PhaseHeld,
		},
		{
			name: "the workloads don't recover",
			at:   PhaseRecovering,
			fail: func(h *harness) { h.wait(DefaultRecoveryDeadline + time.Second) },
			want: PhaseHeld,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			before := h.runTo(tt.at)
			request := h.request()

			tt.fail(h)

			state := h.reconcile()
			assert.Equal(t, tt.want, state.Phase, state.Reason)
			assert.Equal(t, tt.tainted, h.tainted(), "tainted")
			assert.Equal(t, tt.attempts, state.Attempts, "attempts")

			if tt.endsGrant {
				ending := h.request()
				assert.Equal(t, contract.OperationStage, ending.Operation, "a new stage request ends the grant")
				assert.NotEqual(t, request.NodeUpdateID, ending.NodeUpdateID)
				assert.Equal(t, before.TargetVersion, ending.TargetVersion)
			}
		})
	}
}

func TestThreeFailedAttemptsHoldTheNode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		h.runTo(PhaseStaging)
		h.agentAnswers(Keys.Status, contract.CodeOperationFailed)

		state := h.reconcile()
		assert.Equal(t, attempt, state.Attempts)

		if attempt < maxAttempts {
			require.Equal(t, PhaseIdle, state.Phase)

			// The scheduler gives the node a slot again.
			state.Phase = PhaseChecking
			require.NoError(t, writeState(t.Context(), h.client, machineOf(t, h.client, "acl-1"), state))
		} else {
			assert.Equal(t, PhaseHeld, state.Phase)
			assert.Contains(t, state.Reason, "3 attempts at "+newer)
		}
	}
}

func TestAnUnansweredRequestIsWrittenAgainThenHeld(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.runTo(PhaseStaging)

	for writes := 2; writes <= maxAttempts; writes++ {
		h.wait(DefaultStatusDeadline)

		state := h.reconcile()
		require.Equal(t, PhaseStaging, state.Phase)
		assert.Equal(t, writes, state.Writes)
	}

	h.wait(DefaultStatusDeadline)
	assert.Equal(t, PhaseHeld, h.reconcile().Phase)
}

func TestClearingAHoldRemovesTheTaint(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.runTo(PhaseFinalizing)
	h.agentAnswers(Keys.Status, contract.CodeAgentInternalError)
	require.Equal(t, PhaseHeld, h.reconcile().Phase)
	require.True(t, h.tainted())

	machine := machineOf(t, h.client, "acl-1")
	delete(machine.Annotations, names.StateAnnotation)
	require.NoError(t, h.client.Update(t.Context(), machine))

	assert.Equal(t, PhaseIdle, h.reconcile().Phase)
	assert.False(t, h.tainted())
}

func TestANodeNeverGivenASlotKeepsNoState(t *testing.T) {
	t.Parallel()

	c := testClient(t, testSite("edge", nil), testNode(t, "acl-1", "edge", running), testMachine(t, "acl-1", nil))
	r := &Reconciler{Client: c, Reader: c, Nebraska: &fakeNebraska{}, Drainer: &fakeDrainer{}, Clock: clocktesting.NewFakePassiveClock(start)}

	_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "acl-1"}})
	require.NoError(t, err)
	assert.NotContains(t, machineOf(t, c, "acl-1").Annotations, names.StateAnnotation)
}

func TestTheNodesAppLabelChoosesItsApp(t *testing.T) {
	t.Parallel()

	site := testSite("edge", nil)
	site.Spec.Components.CenterPivot.Apps = append(site.Spec.Components.CenterPivot.Apps,
		unboundedv1alpha3.CenterPivotApp{Name: "acl-nvidia", AppID: nvidiaApp, DefaultTrack: "gpu-stable"})

	gpu := testNode(t, "gpu-1", "edge", running)
	gpu.Labels[names.AppLabel] = "acl-nvidia"

	c := testClient(t, site,
		gpu, testMachine(t, "gpu-1", &State{Phase: PhaseChecking}),
		testNode(t, "unlabeled", "edge", running), testMachine(t, "unlabeled", &State{Phase: PhaseChecking}))
	nebraska := &fakeNebraska{result: omaha.Result{Kind: omaha.Granted, Version: newer}}
	r := &Reconciler{Client: c, Reader: c, Nebraska: nebraska, Drainer: &fakeDrainer{}, Clock: clocktesting.NewFakePassiveClock(start)}

	for _, name := range []string{"gpu-1", "unlabeled"} {
		_, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		require.NoError(t, err)
	}

	require.Len(t, nebraska.checks, 1, "only the labeled node is checked")
	assert.Equal(t, nvidiaApp, nebraska.checks[0].AppID)
	assert.Equal(t, "gpu-stable", nebraska.checks[0].Track, "the app's default track")

	var request contract.Request
	require.NoError(t, json.Unmarshal([]byte(nodeOf(t, c, "gpu-1").Annotations[Keys.Request]), &request))
	assert.Equal(t, nvidiaApp, request.AppID)

	state := stateOf(t, c, "unlabeled")
	assert.Equal(t, PhaseIdle, state.Phase)
	assert.Contains(t, state.Reason, "the Site has 2 apps")
}

func nodeOf(t *testing.T, c client.Client, name string) *corev1.Node {
	t.Helper()

	var node corev1.Node
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: name}, &node))

	return &node
}
