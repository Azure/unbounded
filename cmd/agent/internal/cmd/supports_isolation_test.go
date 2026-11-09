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

	for _, mode := range []string{"nspawn", "none"} {
		require.NoError(t, supportsIsolation(mode), mode)
	}

	for _, mode := range []string{"", "container", "microvm", "None"} {
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
	require.Error(t, run("container"))
	require.Error(t, run(), "a mode is required")
	require.Error(t, run("none", "nspawn"))

	// The daemon and the recovery script call it by this name.
	require.Equal(t, "supports-isolation", SupportsIsolationCommand)
	require.Equal(t, daemon.SupportsIsolationCommand, newCmdSupportsIsolation().Name())
}
