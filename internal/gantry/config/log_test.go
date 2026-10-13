// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/logger"
)

func TestLogOptionsDefaults(t *testing.T) {
	defaults := logger.Options{Level: logger.DefaultLevel, Format: logger.FormatJSON}

	got, err := NewDefault().LogOptions()
	require.NoError(t, err)
	require.Equal(t, defaults, got)

	// An empty value means the default, as an unset environment variable does.
	for _, empty := range []string{"", "  "} {
		c := NewDefault()
		c.LogLevel, c.LogFormat = empty, empty

		got, err := c.LogOptions()
		require.NoError(t, err, "%q", empty)
		require.Equal(t, defaults, got, "%q", empty)
	}
}

func TestLogOptionsRejectsInvalidValues(t *testing.T) {
	c := NewDefault()
	c.LogLevel, c.LogFormat = "loud", "yaml"

	_, err := c.LogOptions()
	require.ErrorContains(t, err, "log_level")
	require.ErrorContains(t, err, "log_format", "both errors are reported")

	require.ErrorContains(t, c.Validate(), "log_level", "Validate reports what LogOptions rejects")
}

// TestLogSettingPrecedence: YAML, then GANTRY_LOG_*, then flags.
func TestLogSettingPrecedence(t *testing.T) {
	c := NewDefault()
	require.NoError(t, c.LoadYAML(strings.NewReader("log_level: debug\nlog_format: text\n")))

	got, err := c.LogOptions()
	require.NoError(t, err)
	require.Equal(t, logger.Options{Level: slog.LevelDebug, Format: logger.FormatText}, got)

	env := map[string]string{"GANTRY_LOG_LEVEL": "warn", "GANTRY_LOG_FORMAT": "prettytext"}

	require.NoError(t, c.LoadEnv(func(k string) string { return env[k] }))

	got, err = c.LogOptions()
	require.NoError(t, err)
	require.Equal(t, logger.Options{Level: slog.LevelWarn, Format: logger.FormatPrettyText}, got)

	fs := flag.NewFlagSet("gantry", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.BindFlags(fs)
	require.NoError(t, fs.Parse([]string{"--log-level=error", "--log-format=json"}))

	got, err = c.LogOptions()
	require.NoError(t, err)
	require.Equal(t, logger.Options{Level: slog.LevelError, Format: logger.FormatJSON}, got)
}
