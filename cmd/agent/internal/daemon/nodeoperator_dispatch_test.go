// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func dispatchFixture() (*dispatchOperator, *fakeNodeOperator, *fakeNodeOperator) {
	nspawnOp := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Mode: isolation.ModeNSpawn}}
	noneOp := &fakeNodeOperator{}

	return newDispatchOperator(map[isolation.Mode]nodeOperator{
		isolation.ModeNSpawn: nspawnOp,
		isolation.ModeNone:   noneOp,
	}), nspawnOp, noneOp
}

func TestDispatchOperator_RoutesActiveMachineOperationsByMode(t *testing.T) {
	d, nspawnOp, noneOp := dispatchFixture()
	log := discardLogger()

	require.NoError(t, d.RestartNode(t.Context(), log, &ActiveMachine{Name: "kube1", Mode: isolation.ModeNone}))
	assert.True(t, noneOp.restartCalled)
	assert.False(t, nspawnOp.restartCalled)

	require.NoError(t, d.RestartNode(t.Context(), log, &ActiveMachine{Name: "kube1", Mode: isolation.ModeNSpawn}))
	assert.True(t, nspawnOp.restartCalled)

	require.NoError(t, d.EnsureLifecycleMigration(t.Context(), log, &ActiveMachine{Mode: isolation.ModeNone}))
	assert.Equal(t, 1, noneOp.lifecycleCalls)
	assert.Zero(t, nspawnOp.lifecycleCalls)

	_, err := d.ReconcileHostRoot(t.Context(), log, &ActiveMachine{Mode: isolation.ModeNone})
	require.NoError(t, err)
	assert.Equal(t, 1, noneOp.hostRootCalls)
	assert.Zero(t, nspawnOp.hostRootCalls)
}

func TestDispatchOperator_EmptyModeIsNSpawn(t *testing.T) {
	d, nspawnOp, noneOp := dispatchFixture()

	require.NoError(t, d.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Name: "kube1"}))
	assert.True(t, nspawnOp.restartCalled)
	assert.False(t, noneOp.restartCalled)
}

func TestDispatchOperator_HostGlobalOperationsUseDefaultMode(t *testing.T) {
	d, nspawnOp, noneOp := dispatchFixture()
	log := discardLogger()

	active, err := d.FindActiveMachine(log)
	require.NoError(t, err)
	assert.Equal(t, "kube1", active.Name)

	require.NoError(t, d.ResetAgentResources(t.Context(), log))
	assert.True(t, nspawnOp.resetCalled)

	require.NoError(t, d.StopDaemon(t.Context(), log))
	assert.True(t, nspawnOp.stopCalled)

	require.NoError(t, d.StageAgentUpgrade(t.Context(), log, agentUpgradeRequest{}))
	assert.True(t, nspawnOp.stageUpgradeCalled)

	// The boot ID and the host reboot belong to the host, not to one mode.
	nspawnOp.bootID = "boot-1"

	id, err := d.BootID()
	require.NoError(t, err)
	assert.Equal(t, "boot-1", id)

	require.NoError(t, d.RebootHost(t.Context(), log))
	assert.Equal(t, 1, nspawnOp.rebootCalls)

	assert.False(t, noneOp.resetCalled || noneOp.stopCalled || noneOp.stageUpgradeCalled)
	assert.Zero(t, noneOp.rebootCalls)
}

func TestDispatchOperator_PropagatesOperatorErrors(t *testing.T) {
	d, nspawnOp, _ := dispatchFixture()
	nspawnOp.restartErr = errors.New("restart failed")
	nspawnOp.findErr = errors.New("no applied config")

	require.ErrorContains(t, d.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Mode: isolation.ModeNSpawn}), "restart failed")

	_, err := d.FindActiveMachine(discardLogger())
	require.ErrorContains(t, err, "no applied config")
}

func TestDispatchOperator_UnregisteredModeAndNilActive(t *testing.T) {
	d, nspawnOp, _ := dispatchFixture()
	log := discardLogger()

	err := d.RestartNode(t.Context(), log, &ActiveMachine{Mode: isolation.ModeContainer})
	require.ErrorContains(t, err, `no node operator for isolation mode "container"`)
	assert.False(t, nspawnOp.restartCalled, "an unknown mode must not fall back to nspawn")

	require.ErrorContains(t, d.RestartNode(t.Context(), log, nil), "no active machine")

	_, err = d.ReconcileHostRoot(t.Context(), log, &ActiveMachine{Mode: isolation.ModeContainer})
	require.Error(t, err)

	empty := newDispatchOperator(nil)
	_, err = empty.FindActiveMachine(log)
	require.ErrorContains(t, err, `no node operator for isolation mode "nspawn"`)
	require.Error(t, empty.ResetAgentResources(t.Context(), log))
	require.Error(t, empty.StopDaemon(t.Context(), log))
	require.Error(t, empty.StageAgentUpgrade(t.Context(), log, agentUpgradeRequest{}))
	require.Error(t, empty.RebootHost(t.Context(), log))

	_, err = empty.BootID()
	require.Error(t, err)
}

func TestNewNodeOperator_DefaultsToNSpawn(t *testing.T) {
	op, ok := newNodeOperator().(*dispatchOperator)
	require.True(t, ok)

	nspawnOp, err := op.forMode(isolation.ModeNSpawn)
	require.NoError(t, err)
	assert.IsType(t, nspawnNodeOperator{}, nspawnOp)
}

func TestRegisterNodeOperator_AddsModeWithoutTouchingDispatcher(t *testing.T) {
	fake := &fakeNodeOperator{}

	registerNodeOperator("test-mode", func() nodeOperator { return fake })
	t.Cleanup(func() {
		nodeOperatorMu.Lock()
		delete(nodeOperatorFactories, "test-mode")
		nodeOperatorMu.Unlock()
	})

	require.NoError(t, newNodeOperator().RestartNode(t.Context(), discardLogger(), &ActiveMachine{Mode: "test-mode"}))
	assert.True(t, fake.restartCalled)

	assert.Panics(t, func() { registerNodeOperator(isolation.ModeNSpawn, func() nodeOperator { return fake }) })
}
