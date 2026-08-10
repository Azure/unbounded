// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type lifecycleTestTask struct{ name string }

func (t lifecycleTestTask) Name() string           { return t.name }
func (lifecycleTestTask) Do(context.Context) error { return nil }

func TestNSpawnLifecycleCommandHasExplicitOperations(t *testing.T) {
	cmd := newCmdNSpawnLifecycle(&CommandContext{LogFormat: "text"})
	require.True(t, cmd.Hidden)
	require.Len(t, cmd.Commands(), 3)
	require.Equal(t, "post-start", cmd.Commands()[0].Name())
	require.Equal(t, "pre-start", cmd.Commands()[1].Name())
	require.Equal(t, "reconcile", cmd.Commands()[2].Name())

	cmd.SetArgs([]string{"pre-start", "other"})
	require.ErrorContains(t, cmd.ExecuteContext(context.Background()), "unknown nspawn machine")
}

func TestNSpawnLifecyclePostStartRewiresFreshNVIDIAState(t *testing.T) {
	t.Parallel()

	want := completeNVIDIA("post-start")

	var (
		waited   bool
		executed bool
		got      *goalstates.NodeStart
	)

	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1",
		func(string) (goalstates.NvidiaHost, error) { return want, nil },
		func(context.Context, *slog.Logger, string) error { waited = true; return nil },
		func(_ *slog.Logger, state *goalstates.NodeStart) phases.Task {
			got = state
			return lifecycleTestTask{name: "test-nvidia"}
		},
		func(_ context.Context, _ *slog.Logger, task phases.Task) error {
			executed = true

			require.Equal(t, "test-nvidia", task.Name())

			return nil
		},
	)
	require.NoError(t, err)
	require.True(t, waited)
	require.True(t, executed)
	require.True(t, got.Nvidia.Required)
	require.True(t, got.Containerd.NvidiaRuntime.Enabled)
	require.Equal(t, want, got.Nvidia)
}

func TestNSpawnLifecyclePostStartCPUNodeDoesNotEnableNVIDIA(t *testing.T) {
	t.Parallel()

	called := false
	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1",
		func(string) (goalstates.NvidiaHost, error) { return goalstates.NvidiaHost{}, nil },
		func(context.Context, *slog.Logger, string) error { called = true; return nil },
		func(*slog.Logger, *goalstates.NodeStart) phases.Task {
			called = true

			return lifecycleTestTask{name: "unexpected-reconcile"}
		},
		func(context.Context, *slog.Logger, phases.Task) error { called = true; return nil },
	)
	require.NoError(t, err)
	require.False(t, called)
}

func TestNSpawnLifecyclePostStartFailurePropagatesForNSpawnRestart(t *testing.T) {
	t.Parallel()

	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1",
		func(string) (goalstates.NvidiaHost, error) { return completeNVIDIA("direct-restart"), nil },
		func(context.Context, *slog.Logger, string) error { return nil },
		func(*slog.Logger, *goalstates.NodeStart) phases.Task { return lifecycleTestTask{name: "test-nvidia"} },
		func(context.Context, *slog.Logger, phases.Task) error { return errors.New("setup failed") },
	)
	require.ErrorContains(t, err, "setup failed")
}

func TestNSpawnLifecyclePostStartRejectsIncompleteNVIDIAState(t *testing.T) {
	t.Parallel()

	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1",
		func(string) (goalstates.NvidiaHost, error) {
			return goalstates.NvidiaHost{GPUDevicePaths: []string{"/dev/nvidia0"}}, nil
		},
		func(context.Context, *slog.Logger, string) error { return nil },
		func(*slog.Logger, *goalstates.NodeStart) phases.Task { return lifecycleTestTask{name: "test-nvidia"} },
		func(context.Context, *slog.Logger, phases.Task) error { return nil },
	)
	require.ErrorIs(t, err, goalstates.ErrNVIDIAStateUnavailable)
}
