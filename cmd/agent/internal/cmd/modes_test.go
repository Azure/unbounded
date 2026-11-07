// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
)

func TestEveryModeInTheTableIsFullyWired(t *testing.T) {
	t.Parallel()

	table := agentModes()
	require.Contains(t, table, isolation.DefaultMode, "the default mode must always be operable")

	for mode := range table {
		m, err := table.lookup(mode)
		require.NoError(t, err, mode)

		backend := m.newBackend(slog.New(slog.DiscardHandler), &provision.AgentConfig{})
		require.NotNil(t, backend, mode)
		require.Equal(t, mode, backend.Mode(), "a mode's backend is that mode's")
		require.NotNil(t, m.newOperator(m.newBackend), mode)
		require.NotNil(t, m.newPlan(), mode)
	}
}

func TestNSpawnEntryWiresTheNSpawnComponents(t *testing.T) {
	t.Parallel()

	m, err := agentModes().lookup(isolation.ModeNSpawn)
	require.NoError(t, err)

	require.IsType(t, &nspawn.Backend{}, m.newBackend(slog.New(slog.DiscardHandler), &provision.AgentConfig{}))
	require.IsType(t, nspawnPlan{}, m.newPlan())

	op, ok := m.newOperator(m.newBackend).(daemon.NSpawnOperator)
	require.True(t, ok)
	require.NotNil(t, op.NewBackend, "the operator gets the table's backend")
}

func TestLookupEmptyModeIsTheDefault(t *testing.T) {
	t.Parallel()

	m, err := agentModes().lookup("")
	require.NoError(t, err)
	require.IsType(t, nspawnPlan{}, m.newPlan())
}

func TestLookupRefusesAModeWithNoEntry(t *testing.T) {
	t.Parallel()

	_, err := agentModes().lookup("none")
	require.ErrorContains(t, err, `cannot operate isolation mode "none"`)
}

func TestLookupRefusesAPartlyWiredMode(t *testing.T) {
	t.Parallel()

	full, err := agentModes().lookup(isolation.ModeNSpawn)
	require.NoError(t, err)

	for name, m := range map[string]agentMode{
		"no backend":  {newOperator: full.newOperator, newPlan: full.newPlan},
		"no operator": {newBackend: full.newBackend, newPlan: full.newPlan},
		"no plan":     {newBackend: full.newBackend, newOperator: full.newOperator},
	} {
		_, err := agentModeTable{"partial": m}.lookup("partial")
		require.ErrorContains(t, err, "not fully wired", name)

		_, err = agentModeTable{"partial": m}.operators()
		require.Error(t, err, name)
	}
}

func TestOperatorsBuildsOneOperatorPerMode(t *testing.T) {
	t.Parallel()

	ops, err := agentModes().operators()
	require.NoError(t, err)
	require.Len(t, ops, len(agentModes()))
	require.IsType(t, daemon.NSpawnOperator{}, ops[isolation.ModeNSpawn])
}
