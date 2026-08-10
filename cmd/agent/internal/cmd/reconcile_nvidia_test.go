// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type lifecycleTestTask struct{}

func (lifecycleTestTask) Name() string             { return "test-nvidia" }
func (lifecycleTestTask) Do(context.Context) error { return nil }

func TestNSpawnLifecycleCommandHasExplicitPhases(t *testing.T) {
	cmd := newCmdNSpawnLifecycle(&CommandContext{LogFormat: "text"})
	require.True(t, cmd.Hidden)
	require.Len(t, cmd.Commands(), 2)
	require.Equal(t, "post-start", cmd.Commands()[0].Name())
	require.Equal(t, "pre-start", cmd.Commands()[1].Name())

	cmd.SetArgs([]string{"pre-start", "other"})
	require.ErrorContains(t, cmd.ExecuteContext(context.Background()), "unknown nspawn machine")
}

func TestNSpawnLifecyclePostStartUsesExactPreStartState(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "state.json")
	want := completeNVIDIA("resolved-pre-start")
	writeLifecycleState(t, statePath, true, want)

	var (
		waited   bool
		executed bool
		got      *goalstates.NodeStart
	)

	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1", statePath,
		func(context.Context, *slog.Logger, string) error { waited = true; return nil },
		func(_ *slog.Logger, state *goalstates.NodeStart) phases.Task { got = state; return lifecycleTestTask{} },
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

	statePath := filepath.Join(t.TempDir(), "state.json")
	writeLifecycleState(t, statePath, false, goalstates.NvidiaHost{})

	called := false

	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1", statePath,
		func(context.Context, *slog.Logger, string) error { called = true; return nil },
		func(*slog.Logger, *goalstates.NodeStart) phases.Task { called = true; return lifecycleTestTask{} },
		func(context.Context, *slog.Logger, phases.Task) error { called = true; return nil },
	)
	require.NoError(t, err)
	require.False(t, called)
}

func TestNSpawnLifecyclePostStartFailurePropagatesForNSpawnRestart(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "state.json")
	writeLifecycleState(t, statePath, true, completeNVIDIA("direct-restart"))

	err := nspawnLifecyclePostStart(
		context.Background(), testLogger(), "kube1", statePath,
		func(context.Context, *slog.Logger, string) error { return nil },
		func(*slog.Logger, *goalstates.NodeStart) phases.Task { return lifecycleTestTask{} },
		func(context.Context, *slog.Logger, phases.Task) error { return errors.New("setup failed") },
	)
	require.ErrorContains(t, err, "setup failed")
}

func TestNSpawnLifecyclePostStartRejectsMissingAndCorruptState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "missing.json")
	_, err := goalstates.LoadNSpawnLifecycleState(statePath, "kube1")
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, os.WriteFile(statePath, []byte("{"), 0o600))
	_, err = goalstates.LoadNSpawnLifecycleState(statePath, "kube1")
	require.ErrorContains(t, err, "decode nspawn lifecycle state")
}
