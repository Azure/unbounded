// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogFlags(t *testing.T) {
	flags := newCommand().Flags()

	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "json", flags.Lookup("log-format").DefValue)
}
