// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/logger"
)

func TestParseLogFlags(t *testing.T) {
	got, rest, err := parseLogFlags(nil, io.Discard)
	require.NoError(t, err)
	require.Equal(t, logger.Options{Level: logger.DefaultLevel, Format: logger.FormatText}, got)
	require.Empty(t, rest)

	// Flags after the subcommand name belong to the subcommand.
	got, rest, err = parseLogFlags([]string{"--log-level=warn", "get-url", "--url=http://example", "--log-level=error"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, slog.LevelWarn, got.Level)
	require.Equal(t, []string{"get-url", "--url=http://example", "--log-level=error"}, rest)
}

func TestParseLogFlagsUsage(t *testing.T) {
	var out bytes.Buffer

	_, _, err := parseLogFlags([]string{"-h"}, &out)
	require.ErrorIs(t, err, flag.ErrHelp)
	require.Contains(t, out.String(), "probe-health")
	require.Contains(t, out.String(), "(default info)")

	out.Reset()

	_, _, err = parseLogFlags([]string{"--log-level=verbose"}, &out)
	require.ErrorIs(t, err, errFlags, "main exits 2 without logging the error again")
	require.Contains(t, out.String(), "unknown log level")
	require.Contains(t, out.String(), "Subcommands:")
}
