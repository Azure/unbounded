// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogFlags(t *testing.T) {
	t.Parallel()

	flags := newCommand().Flags()

	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "text", flags.Lookup("log-format").DefValue)
}
