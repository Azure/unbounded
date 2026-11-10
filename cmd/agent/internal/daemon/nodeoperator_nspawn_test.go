// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNSpawnOperator_RestartNodeRestartsTheActiveSlotThroughItsBackend(t *testing.T) {
	fake := &isolation.FakeBackend{}
	cfg := baseConfig()

	var gotCfg *provision.AgentConfig

	op := NewNSpawnOperator(func(_ *slog.Logger, c *provision.AgentConfig) isolation.Backend {
		gotCfg = c

		return fake
	})

	require.NoError(t, op.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Name: "kube2", Config: cfg}))
	require.Equal(t, []string{"restart kube2"}, fake.Calls())
	require.Same(t, cfg, gotCfg, "the backend is bound to the active machine's applied config")
}

func TestNSpawnOperator_RestartNodeReturnsTheBackendError(t *testing.T) {
	boom := errors.New("machine did not come back")
	op := NewNSpawnOperator(func(*slog.Logger, *provision.AgentConfig) isolation.Backend {
		return &isolation.FakeBackend{RestartErr: boom}
	})

	require.ErrorIs(t, op.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Name: "kube1", Config: baseConfig()}), boom)
}

func TestNSpawnOperator_RestartNodeWithoutABackendFails(t *testing.T) {
	err := NSpawnOperator{}.RestartNode(t.Context(), discardLogger(), &ActiveMachine{Name: "kube1"})
	require.ErrorContains(t, err, "no backend")
}
