// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRootLogFlags(t *testing.T) {
	flags := newRootCommand().PersistentFlags()

	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "prettytext", flags.Lookup("log-format").DefValue)
}

// TestLoggingSetUpBeforeEverySubcommand: the net command has a PersistentPreRun
// of its own, which must not stop the root's logging setup from running. Run
// sets cobra.EnableTraverseRunHooks for that; the test sets it the same way.
// The root hook is replaced so the test does not install the process-wide
// logger.
func TestLoggingSetUpBeforeEverySubcommand(t *testing.T) {
	previous := cobra.EnableTraverseRunHooks

	t.Cleanup(func() { cobra.EnableTraverseRunHooks = previous })

	cobra.EnableTraverseRunHooks = true

	for _, args := range [][]string{{"version"}, {"net", "options"}, {"net", "version"}} {
		root := newRootCommand()

		setUp := false
		root.PersistentPreRun = func(*cobra.Command, []string) { setUp = true }

		root.SetArgs(args)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)

		require.NoError(t, root.Execute(), "args %v", args)
		require.True(t, setUp, "logging not set up for %v", args)
	}
}
