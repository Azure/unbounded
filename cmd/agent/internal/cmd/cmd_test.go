// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"io"
	"log/slog"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/logger"
)

// parseLogFlags runs a root command carrying the agent's logging flags and
// returns the options a subcommand would see.
func parseLogFlags(t *testing.T, args ...string) logger.Options {
	t.Helper()

	cmdCtx := newCommandContext()
	root := &cobra.Command{Use: "agent", SilenceErrors: true, SilenceUsage: true}
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	bindLogFlags(root, cmdCtx)
	root.AddCommand(&cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }})
	root.SetArgs(append([]string{"start"}, args...))
	require.NoError(t, root.Execute())

	return cmdCtx.LogOpts
}

func TestAgentLogFlags(t *testing.T) {
	task := []string{"task"}

	require.Equal(t, logger.Options{Level: logger.DefaultLevel, Format: logger.FormatPrettyText, AttrOrder: task}, parseLogFlags(t))
	require.Equal(t, logger.Options{Level: slog.LevelDebug, Format: logger.FormatPrettyText, AttrOrder: task}, parseLogFlags(t, "--debug"))
	require.Equal(t, logger.Options{Level: logger.DefaultLevel, Format: logger.FormatPrettyText, AttrOrder: task, NoColor: true}, parseLogFlags(t, "--no-color"))
}

// TestAgentDebugFlagIsHidden: --debug keeps working for the install script, but
// help shows --log-level.
func TestAgentDebugFlagIsHidden(t *testing.T) {
	root := &cobra.Command{Use: "agent"}
	bindLogFlags(root, newCommandContext())

	debug := root.PersistentFlags().Lookup("debug")
	require.NotNil(t, debug)
	require.True(t, debug.Hidden)
	require.Empty(t, debug.Deprecated, "using --debug must not print a deprecation warning")
	require.NotNil(t, root.PersistentFlags().Lookup("log-level"))
}
