// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
)

func TestSupportsIsolation(t *testing.T) {
	t.Parallel()

	// container is operable by this agent; whether a host may run it is the
	// experimental opt-in in its configuration, not a property of the binary.
	for _, mode := range []string{"nspawn", "none", "container"} {
		require.NoError(t, supportsIsolation(mode), mode)
	}

	for _, mode := range []string{"", "microvm", "None"} {
		require.Error(t, supportsIsolation(mode), mode)
	}
}

func TestSupportsIsolationCommand(t *testing.T) {
	t.Parallel()

	run := func(args ...string) error {
		cmd := newCmdSupportsIsolation()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)

		return cmd.Execute()
	}

	require.NoError(t, run("none"))
	require.NoError(t, run("container"))
	require.Error(t, run("microvm"))
	require.Error(t, run(), "a mode is required")
	require.Error(t, run("none", "nspawn"))

	// The daemon and the recovery script call it by this name.
	require.Equal(t, "supports-isolation", SupportsIsolationCommand)
	require.Equal(t, daemon.SupportsIsolationCommand, newCmdSupportsIsolation().Name())
}
