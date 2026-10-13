// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogFlags(t *testing.T) {
	t.Parallel()

	root := newRootCommand()
	flags := root.PersistentFlags()

	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "prettytext", flags.Lookup("log-format").DefValue)

	for _, name := range []string{"build", "validate-oci"} {
		sub, _, err := root.Find([]string{name})
		require.NoError(t, err)
		require.NotNil(t, sub.InheritedFlags().Lookup("log-level"), "%s inherits --log-level", name)
	}
}
