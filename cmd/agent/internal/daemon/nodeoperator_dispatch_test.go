// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// fakeHost is a hostRootOperations over a fakeNodeOperator that records the
// lifecycle migrator the dispatcher hands the host root move.
type fakeHost struct {
	*fakeNodeOperator
	lifecycle lifecycleMigrator
}

func (h *fakeHost) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine, lifecycle lifecycleMigrator) (bool, error) {
	h.lifecycle = lifecycle

	return h.fakeNodeOperator.ReconcileHostRoot(ctx, log, active)
}

const testMode isolation.Mode = "test-mode"

func dispatchFixture() (*dispatchOperator, *fakeHost, *fakeNodeOperator, *fakeNodeOperator) {
	host := &fakeHost{fakeNodeOperator: &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Mode: isolation.ModeNSpawn}}}
	nspawnOp := &fakeNodeOperator{}
	otherOp := &fakeNodeOperator{}

	return newDispatchOperator(host, ModeOperators{
		isolation.ModeNSpawn: nspawnOp,
		testMode:             otherOp,
	}), host, nspawnOp, otherOp
}

func TestDispatchOperator_RoutesNodeOperationsByMode(t *testing.T) {
	d, host, nspawnOp, otherOp := dispatchFixture()
	log := discardLogger()

	require.NoError(t, d.RestartNode(t.Context(), log, &ActiveMachine{Name: "kube1", Mode: testMode}))
	assert.True(t, otherOp.restartCalled)
	assert.False(t, nspawnOp.restartCalled)

	require.NoError(t, d.RestartNode(t.Context(), log, &ActiveMachine{Name: "kube1", Mode: isolation.ModeNSpawn}))
	assert.True(t, nspawnOp.restartCalled)

	require.NoError(t, d.EnsureLifecycleMigration(t.Context(), log, &ActiveMachine{Mode: testMode}))
	assert.Equal(t, 1, otherOp.lifecycleCalls)
	assert.Zero(t, nspawnOp.lifecycleCalls)

	require.NoError(t, d.RepaveNode(t.Context(), log, &ActiveMachine{Name: "kube2", Mode: testMode}, nil))
	assert.Equal(t, "kube2", otherOp.repaveActive.Name)
	assert.Nil(t, nspawnOp.repaveActive)

	assert.False(t, host.restartCalled || host.repaveActive != nil || host.lifecycleCalls > 0,
		"node operations never reach the host operator")
}

// The host root move is host level, but the lifecycle files it rewrites belong
// to the node's mode, so the move is given that mode's operator.
func TestDispatchOperator_HostRootMoveUsesTheNodesModeForLifecycle(t *testing.T) {
	d, host, nspawnOp, otherOp := dispatchFixture()

	_, err := d.ReconcileHostRoot(t.Context(), discardLogger(), &ActiveMachine{Mode: testMode})
	require.NoError(t, err)
	assert.Equal(t, 1, host.hostRootCalls)
	assert.Same(t, otherOp, host.lifecycle)

	_, err = d.ReconcileHostRoot(t.Context(), discardLogger(), &ActiveMachine{})
	require.NoError(t, err)
	assert.Same(t, nspawnOp, host.lifecycle, "the empty mode is nspawn")
}

func TestDispatchOperator_EmptyModeIsNSpawn(t *testing.T) {
	d, _, nspawnOp, otherOp := dispatchFixture()

	require.NoError(t, d.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Name: "kube1"}))
	assert.True(t, nspawnOp.restartCalled)
	assert.False(t, otherOp.restartCalled)
}

func TestDispatchOperator_HostOperationsGoToTheHostOperator(t *testing.T) {
	d, host, nspawnOp, otherOp := dispatchFixture()
	log := discardLogger()

	active, err := d.FindActiveMachine(log)
	require.NoError(t, err)
	assert.Equal(t, "kube1", active.Name)

	require.NoError(t, d.ResetAgentResources(t.Context(), log))
	assert.True(t, host.resetCalled)

	require.NoError(t, d.StopDaemon(t.Context(), log))
	assert.True(t, host.stopCalled)

	require.NoError(t, d.StageAgentUpgrade(t.Context(), log, agentUpgradeRequest{downloadURL: "https://example/agent"}))
	assert.Equal(t, "https://example/agent", host.stageUpgradeURL)

	host.bootID = "boot-1"

	id, err := d.BootID()
	require.NoError(t, err)
	assert.Equal(t, "boot-1", id)

	require.NoError(t, d.RebootHost(t.Context(), log))
	assert.Equal(t, 1, host.rebootCalls)

	for _, op := range []*fakeNodeOperator{nspawnOp, otherOp} {
		assert.False(t, op.resetCalled || op.stopCalled || op.stageUpgradeCalled)
		assert.Zero(t, op.rebootCalls)
	}
}

func TestDispatchOperator_PropagatesErrors(t *testing.T) {
	d, host, nspawnOp, _ := dispatchFixture()
	nspawnOp.restartErr = errors.New("restart failed")
	host.findErr = errors.New("no applied config")
	host.resetErr = errors.New("reset failed")

	require.ErrorContains(t, d.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Mode: isolation.ModeNSpawn}), "restart failed")
	require.ErrorContains(t, d.ResetAgentResources(t.Context(), discardLogger()), "reset failed")

	_, err := d.FindActiveMachine(discardLogger())
	require.ErrorContains(t, err, "no applied config")
}

// A mode with no operator is an error for every node operation, never a
// fallback to the nspawn operator.
func TestDispatchOperator_AModeWithNoOperatorNeverFallsBack(t *testing.T) {
	d, host, nspawnOp, _ := dispatchFixture()
	log := discardLogger()
	unknown := &ActiveMachine{Name: "kube1", Mode: "unknown"}

	require.ErrorContains(t, d.RestartNode(t.Context(), log, unknown), `cannot operate isolation mode "unknown"`)
	require.Error(t, d.EnsureLifecycleMigration(t.Context(), log, unknown))
	require.Error(t, d.RepaveNode(t.Context(), log, unknown, nil))

	_, err := d.ReconcileHostRoot(t.Context(), log, unknown)
	require.Error(t, err)
	assert.Zero(t, host.hostRootCalls, "the host root is not moved for a node this agent cannot operate")

	assert.False(t, nspawnOp.restartCalled)
	assert.Zero(t, nspawnOp.lifecycleCalls)
	assert.Nil(t, nspawnOp.repaveActive)

	require.ErrorContains(t, d.RestartNode(t.Context(), log, nil), "no active machine")

	// A nil operator in the table is the same as no entry.
	withNil := newDispatchOperator(host, ModeOperators{isolation.ModeNSpawn: nil})
	require.Error(t, withNil.RestartNode(t.Context(), log, &ActiveMachine{}))
}

// The daemon stops before it operates a node applied in a mode it has no
// operator for.
func TestDispatchOperator_FindActiveMachineRefusesAModeWithNoOperator(t *testing.T) {
	d, host, _, _ := dispatchFixture()
	host.active = &ActiveMachine{Name: "kube2", Mode: "unknown"}

	_, err := d.FindActiveMachine(discardLogger())
	require.ErrorContains(t, err, "active machine kube2")
	require.ErrorContains(t, err, `cannot operate isolation mode "unknown"`)

	empty := newDispatchOperator(host, nil)
	host.active = &ActiveMachine{Name: "kube1"}

	_, err = empty.FindActiveMachine(discardLogger())
	require.ErrorContains(t, err, `cannot operate isolation mode "nspawn"`)
}

func TestRun_RequiresANodeOperator(t *testing.T) {
	require.ErrorContains(t, run(t.Context(), discardLogger(), runOptions{}), "node operator")
}
