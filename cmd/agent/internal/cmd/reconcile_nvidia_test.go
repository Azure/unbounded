// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
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
