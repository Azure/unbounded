// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestNodeLifecycleCommand(t *testing.T) {
	cmd := newCmdNodeLifecycle(&CommandContext{LogFormat: "text"})
	require.True(t, cmd.Hidden)
	require.Len(t, cmd.Commands(), 3)

	names := map[string]bool{}

	for _, c := range cmd.Commands() {
		require.True(t, c.Hidden)

		names[c.Name()] = true
	}

	require.Equal(t, map[string]bool{"pre-start": true, "post-start": true, "reconcile": true}, names)

	cmd.SetArgs([]string{"pre-start", "other"})
	require.ErrorContains(t, cmd.ExecuteContext(context.Background()), "invalid container slot")
}

func TestNodeLifecycleRegistered(t *testing.T) {
	l := newNodeLifecycle(testLogger())
	require.NotNil(t, l)

	// Pre-start checks the other slot before it starts one.
	require.NotNil(t, l.Peer)
	require.Equal(t, "kube2", l.Peer("kube1"))
	require.Equal(t, "kube1", l.Peer("kube2"))
}

func TestNodeLifecycleWiresTheGPUHooks(t *testing.T) {
	l := newNodeLifecycleWith(testLogger(), containerops.NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) { return goalstates.NvidiaHost{}, nil },
	})

	c, ok := l.Controller.(containerController)
	require.True(t, ok)
	require.NotNil(t, c.b.RefreshGrants, "pre-start follows the host's GPUs")
	require.NotNil(t, l.AfterStart, "post-start wires GPUs in the container")

	// A host with no NVIDIA device: the hook succeeds without touching the
	// container runtime (which does not exist in this test).
	require.NoError(t, l.AfterStart(context.Background(), "kube1"))

	// The pre-start refresh leaves a node recorded without NVIDIA grants alone.
	got, err := c.b.RefreshGrants(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestNodeLifecycleGPUDiscoveryFailureFailsPostStart(t *testing.T) {
	l := newNodeLifecycleWith(testLogger(), containerops.NVIDIAHooks{
		Resolve: func(string) (goalstates.NvidiaHost, error) {
			return goalstates.NvidiaHost{}, errors.New("nvidia-smi hung")
		},
	})

	require.ErrorContains(t, l.AfterStart(context.Background(), "kube1"), "nvidia-smi hung")
}
