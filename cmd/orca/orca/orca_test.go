// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orca

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/logger"
)

// TestResolveLogLevel_PrecedenceAndDefault covers the resolution
// order documented on resolveLogLevel: ORCA_LOG_LEVEL wins when
// set and non-empty (after trim), otherwise the YAML-configured
// value is used, otherwise the empty string defaults through
// config.ParseLogLevel to info.
func TestResolveLogLevel_PrecedenceAndDefault(t *testing.T) {
	tests := []struct {
		name      string
		yamlLevel string
		envLevel  string // "" -> simulate unset via Setenv with ""
		want      slog.Level
		wantErr   bool
	}{
		{"empty yaml, no env -> info", "", "", slog.LevelInfo, false},
		{"yaml info, no env", "info", "", slog.LevelInfo, false},
		{"yaml debug, no env", "debug", "", slog.LevelDebug, false},
		{"yaml info overridden by env debug", "info", "debug", slog.LevelDebug, false},
		{"yaml debug overridden by env warn", "debug", "warn", slog.LevelWarn, false},
		{"whitespace env falls back to yaml", "warn", "   ", slog.LevelWarn, false},
		{"invalid yaml fails", "trace", "", 0, true},
		{"invalid env fails even when yaml valid", "info", "trace", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORCA_LOG_LEVEL", tt.envLevel)

			got, err := resolveLogLevel(tt.yamlLevel)
			if tt.wantErr {
				if err == nil {
					t.Errorf("resolveLogLevel(%q) = %v, want error", tt.yamlLevel, got)
				}

				return
			}

			if err != nil {
				t.Errorf("resolveLogLevel(%q) unexpected err: %v", tt.yamlLevel, err)
				return
			}

			if got != tt.want {
				t.Errorf("resolveLogLevel(yaml=%q, env=%q) = %v, want %v",
					tt.yamlLevel, tt.envLevel, got, tt.want)
			}
		})
	}
}

// TestResolveLogOptions_FlagWins covers the --log-level flag, which takes
// precedence over ORCA_LOG_LEVEL and the YAML level.
func TestResolveLogOptions_FlagWins(t *testing.T) {
	t.Setenv("ORCA_LOG_LEVEL", "debug")

	flags := logger.Options{Level: slog.LevelError, Format: logger.FormatJSON, AddSource: true}

	got, err := resolveLogOptions(flags, true, "warn")
	require.NoError(t, err)
	require.Equal(t, flags, got)

	got, err = resolveLogOptions(flags, false, "warn")
	require.NoError(t, err)
	require.Equal(t, slog.LevelDebug, got.Level, "without the flag, ORCA_LOG_LEVEL wins")

	t.Setenv("ORCA_LOG_LEVEL", "")

	got, err = resolveLogOptions(flags, false, "warn")
	require.NoError(t, err)
	require.Equal(t, slog.LevelWarn, got.Level, "then the YAML level")

	_, err = resolveLogOptions(flags, false, "trace")
	require.Error(t, err)
}

func TestServeLogFlagDefaults(t *testing.T) {
	flags := newServeCmd().Flags()
	require.Equal(t, "info", flags.Lookup("log-level").DefValue)
	require.Equal(t, "json", flags.Lookup("log-format").DefValue)
}
