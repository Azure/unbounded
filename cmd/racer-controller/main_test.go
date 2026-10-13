// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/logger"
	"github.com/Azure/unbounded/internal/version"
)

func TestRejectInitializeCLI(t *testing.T) {
	for _, args := range [][]string{{"initialize"}, {"other"}, {"initialize", "extra"}, {"--log-level=debug", "initialize"}} {
		// run rejects them before installing a logger.
		require.EqualError(t, run(args, &bytes.Buffer{}), usage, "args %v", args)
	}
}

func TestLogFlags(t *testing.T) {
	got, err := parseFlags(nil)
	require.NoError(t, err)
	require.Equal(t, logger.Options{Level: logger.DefaultLevel, Format: logger.FormatJSON}, got)

	got, err = parseFlags([]string{"--log-level=debug", "--log-format=text"})
	require.NoError(t, err)
	require.Equal(t, logger.Options{Level: slog.LevelDebug, Format: logger.FormatText}, got)

	_, err = parseFlags([]string{"--log-level=verbose"})
	require.ErrorContains(t, err, "log-level")
	require.ErrorContains(t, err, usage, "flag errors include the usage")
}

func TestHelp(t *testing.T) {
	_, err := parseFlags([]string{"--help"})
	require.ErrorIs(t, err, pflag.ErrHelp)
	require.ErrorIs(t, run([]string{"-h"}, &bytes.Buffer{}), pflag.ErrHelp, "help stops before the logger is installed")

	help := helpText()
	require.Contains(t, help, usage)
	require.Contains(t, help, "--log-level")
	require.Contains(t, help, "(default json)")
}

// TestStartupLogsBuildMetadataBeforeConfigurationFailure installs the
// process-wide logger. controller-runtime accepts only the first logger it is
// given and cannot be reset, so this must remain the only test in the package
// that gets run past flag parsing.
func TestStartupLogsBuildMetadataBeforeConfigurationFailure(t *testing.T) {
	previous := slog.Default()

	t.Cleanup(func() { slog.SetDefault(previous) })

	t.Setenv("RACER_CLUSTER_ID", "invalid")

	var output bytes.Buffer

	require.Error(t, run(nil, &output))

	line, _, _ := strings.Cut(output.String(), "\n")

	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &rec), "startup line is JSON: %q", line)
	require.Equal(t, "starting racer-controller", rec["msg"])
	require.Equal(t, version.String(), rec["version"])
	require.Equal(t, "INFO", rec["level"])
}
