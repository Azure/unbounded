// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServePXELogFlags(t *testing.T) {
	flags := ServePXECmd().Flags()

	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "text", flags.Lookup("log-format").DefValue)
}
