// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogFlags(t *testing.T) {
	flags := NewCommand().Flags()

	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "text", flags.Lookup("log-format").DefValue)
}
