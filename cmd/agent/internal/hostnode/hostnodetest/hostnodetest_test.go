// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnodetest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGoalStateTracksSlotAndLocalDNS(t *testing.T) {
	t.Parallel()

	gs := GoalState("kube2", true)
	assert.Equal(t, "kube2", gs.NodeStart.MachineName)
	assert.True(t, gs.RootFS.LocalDNS.Enabled)
	assert.True(t, gs.NodeStart.LocalDNS.Enabled)
	assert.Equal(t, "tok", gs.NodeStart.Kubelet.BootstrapToken)

	gs = GoalState("kube1", false)
	assert.False(t, gs.RootFS.LocalDNS.Enabled)
	assert.False(t, gs.NodeStart.LocalDNS.Enabled)

	assert.NotSame(t, GoalState("kube1", false), GoalState("kube1", false), "each call returns a goal state the test may change")
}

func TestMinimalGoalState(t *testing.T) {
	t.Parallel()

	gs := MinimalGoalState()
	assert.Equal(t, "kube1", gs.NodeStart.MachineName)
	assert.Equal(t, "tok", gs.NodeStart.Kubelet.BootstrapToken)
	assert.NotSame(t, gs, MinimalGoalState())
}

func TestQuietLoggerDoesNotPanic(t *testing.T) {
	t.Parallel()

	Quiet().Info("nothing is printed")
}
