// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogFlags(t *testing.T) {
	root := newCommand()

	require.Equal(t, "info", root.PersistentFlags().Lookup("log-level").DefValue)
	require.Equal(t, "text", root.PersistentFlags().Lookup("log-format").DefValue)
}

// TestLoggingSetUpForSubcommands: the root's PersistentPreRun sets up logging,
// and cobra skips it for a subcommand that has a hook of its own.
func TestLoggingSetUpForSubcommands(t *testing.T) {
	root := newCommand()
	require.NotNil(t, root.PersistentPreRun)

	controlPlane, _, err := root.Find([]string{"control-plane"})
	require.NoError(t, err)
	require.Nil(t, controlPlane.PersistentPreRun)
	require.Nil(t, controlPlane.PersistentPreRunE)
}
