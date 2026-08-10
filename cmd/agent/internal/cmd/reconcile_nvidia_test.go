// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestReconcileNVIDIACommand(t *testing.T) {
	original := reconcileNVIDIAOnMachineStart

	t.Cleanup(func() {
		reconcileNVIDIAOnMachineStart = original
	})

	var gotMachine string

	reconcileNVIDIAOnMachineStart = func(_ context.Context, _ *slog.Logger, machine string) error {
		gotMachine = machine

		return nil
	}

	commandContext := &CommandContext{LogFormat: "text"}
	command := newCmdReconcileNVIDIA(commandContext)
	command.SetArgs([]string{"kube1"})

	require.NoError(t, command.ExecuteContext(context.Background()))
	require.Equal(t, "kube1", gotMachine)
}

func TestReconcileNVIDIACommandRejectsUnknownMachine(t *testing.T) {
	command := newCmdReconcileNVIDIA(&CommandContext{LogFormat: "text"})
	command.SetArgs([]string{"other"})

	require.ErrorContains(t, command.ExecuteContext(context.Background()), "unknown nspawn machine")
}

func TestReconcileNVIDIA(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "applied.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var waited, executed bool

	err := reconcileNVIDIA(
		context.Background(), log, "kube1", configPath, configPath+".sha256",
		func(_ *provision.AgentConfig, machine string) (*goalstates.NodeStart, error) {
			require.Equal(t, "kube1", machine)

			state := &goalstates.NodeStart{
				MachineName: machine,
				Nvidia: goalstates.NvidiaHost{
					LibMappings: []goalstates.NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
				},
			}
			state.Containerd.NvidiaRuntime.Enabled = true

			return state, nil
		},
		func(_ context.Context, _ *slog.Logger, machine string) error {
			waited = true

			require.Equal(t, "kube1", machine)

			return nil
		},
		func(_ context.Context, _ *slog.Logger, task phases.Task) error {
			executed = true

			require.Equal(t, "setup-nvidia", task.Name())

			return nil
		},
	)

	require.NoError(t, err)
	require.True(t, waited)
	require.True(t, executed)
}

func TestReconcileNVIDIAMissingAppliedConfigDefersToManagedStart(t *testing.T) {
	t.Parallel()

	called := false
	err := reconcileNVIDIA(
		context.Background(), slog.Default(), "kube1",
		filepath.Join(t.TempDir(), "missing.json"), filepath.Join(t.TempDir(), "missing.sha256"),
		func(*provision.AgentConfig, string) (*goalstates.NodeStart, error) {
			called = true

			return nil, errors.New("unexpected resolve")
		},
		func(context.Context, *slog.Logger, string) error { return errors.New("unexpected wait") },
		func(context.Context, *slog.Logger, phases.Task) error { return errors.New("unexpected execute") },
	)

	require.NoError(t, err)
	require.False(t, called)
}

func TestReconcileNVIDIARejectsInvalidChecksum(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "applied.json")
	checksumPath := configPath + ".sha256"
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(checksumPath, []byte("invalid\n"), 0o600))

	err := reconcileNVIDIA(
		context.Background(), slog.Default(), "kube1", configPath, checksumPath,
		func(*provision.AgentConfig, string) (*goalstates.NodeStart, error) { return nil, nil },
		func(context.Context, *slog.Logger, string) error { return nil },
		func(context.Context, *slog.Logger, phases.Task) error { return nil },
	)

	require.ErrorIs(t, err, goalstates.ErrChecksumMismatch)
}

func TestReconcileNVIDIARejectsUnavailableSetupState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "applied.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	err := reconcileNVIDIA(
		context.Background(), slog.Default(), "kube1", configPath, configPath+".sha256",
		func(*provision.AgentConfig, string) (*goalstates.NodeStart, error) {
			return &goalstates.NodeStart{}, nil
		},
		func(context.Context, *slog.Logger, string) error { return nil },
		func(context.Context, *slog.Logger, phases.Task) error { return nil },
	)

	require.ErrorContains(t, err, "NVIDIA setup state is unavailable")
}
