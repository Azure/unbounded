// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orca

import (
	"log/slog"
	"testing"
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
