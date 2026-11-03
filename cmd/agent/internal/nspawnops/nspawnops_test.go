// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawnops

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestNewReturnsWiredNSpawnBackend(t *testing.T) {
	b, err := New(Params{Log: quiet()})
	require.NoError(t, err)
	require.Equal(t, isolation.ModeNSpawn, b.Mode())

	nb, ok := b.(*nspawn.Backend)
	require.True(t, ok)
	require.NotNil(t, nb.Ops.Prepare)
	require.NotNil(t, nb.Ops.Start)
	require.NotNil(t, nb.Ops.Stop)
	require.NotNil(t, nb.Ops.Restart)
	require.NotNil(t, nb.Ops.Destroy)
}

func TestOperationsReportResolveFailure(t *testing.T) {
	boom := errors.New("no gpu readiness")
	b := Backend(Params{Log: quiet(), Resolve: func(string) (*goalstates.MachineGoalState, error) { return nil, boom }})
	root := nspawn.Root("kube1")

	_, err := b.Prepare(t.Context(), isolation.NodeSpec{Slot: "kube1"})
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "resolve machine goal state")
	require.ErrorIs(t, b.Start(t.Context(), root), boom)
	require.ErrorIs(t, b.Restart(t.Context(), root), boom)
}

func TestMissingConfigIsAnError(t *testing.T) {
	b := Backend(Params{Log: quiet()})

	err := b.Start(t.Context(), nspawn.Root("kube1"))
	require.ErrorContains(t, err, "no agent configuration")
}

func TestGoalStateResolvedOncePerSlot(t *testing.T) {
	calls := map[string]int{}
	o := &ops{p: Params{Log: quiet(), Resolve: func(slot string) (*goalstates.MachineGoalState, error) {
		calls[slot]++
		return &goalstates.MachineGoalState{}, nil
	}}, goalStates: map[string]*goalstates.MachineGoalState{}}

	for _, slot := range []string{"kube1", "kube1", "kube2"} {
		_, err := o.goalState(slot)
		require.NoError(t, err)
	}

	require.Equal(t, map[string]int{"kube1": 1, "kube2": 1}, calls)
}

func TestFailedResolveIsNotCached(t *testing.T) {
	n := 0
	o := &ops{p: Params{Log: quiet(), Resolve: func(string) (*goalstates.MachineGoalState, error) {
		n++
		if n == 1 {
			return nil, errors.New("transient")
		}

		return &goalstates.MachineGoalState{}, nil
	}}, goalStates: map[string]*goalstates.MachineGoalState{}}

	_, err := o.goalState("kube1")
	require.Error(t, err)

	_, err = o.goalState("kube1")
	require.NoError(t, err)
}
