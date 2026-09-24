// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestAcceptCurrentDirectConfig(t *testing.T) {
	state := benchmarkState{
		Mode:                    benchmarkModeDirect,
		Status:                  "restore-failed",
		OriginalGantryConfig:    "chair_seed_count: 50\n",
		OriginalGantryConfigSHA: gantryConfigSHA("chair_seed_count: 50\n"),
		GantryRestored:          true,
	}
	current := "chair_holder_count: 64\nchair_seed_count: 8\n"

	updated, err := acceptCurrentDirectConfig(state, current, gantryConfigSHA(current))
	if err != nil {
		t.Fatalf("acceptCurrentDirectConfig: %v", err)
	}

	if updated.OriginalGantryConfig != current ||
		updated.OriginalGantryConfigSHA != gantryConfigSHA(current) ||
		updated.GantryRestored {
		t.Fatalf("updated state = %+v", updated)
	}
}

func TestAcceptCurrentDirectConfigRejectsUnsafeRecovery(t *testing.T) {
	current := "chair_holder_count: 64\nchair_seed_count: 8\n"
	original := "chair_seed_count: 50\n"
	tests := []struct {
		name     string
		state    benchmarkState
		expected string
		want     string
	}{
		{
			name: "proxy mode",
			state: benchmarkState{
				Mode:                    benchmarkModeProxy,
				Status:                  "restore-failed",
				OriginalGantryConfigSHA: gantryConfigSHA(original),
			},
			expected: gantryConfigSHA(current),
			want:     "requires a direct-mode benchmark",
		},
		{
			name: "wrong state",
			state: benchmarkState{
				Mode:                    benchmarkModeDirect,
				Status:                  "completed",
				OriginalGantryConfigSHA: gantryConfigSHA(original),
			},
			expected: gantryConfigSHA(current),
			want:     `state is "completed"`,
		},
		{
			name: "sha mismatch",
			state: benchmarkState{
				Mode:                    benchmarkModeDirect,
				Status:                  "restore-failed",
				OriginalGantryConfigSHA: gantryConfigSHA(original),
			},
			expected: gantryConfigSHA("different"),
			want:     "current Gantry config sha256 is",
		},
		{
			name: "no drift",
			state: benchmarkState{
				Mode:                    benchmarkModeDirect,
				Status:                  "restore-failed",
				OriginalGantryConfigSHA: gantryConfigSHA(current),
			},
			expected: gantryConfigSHA(current),
			want:     "still matches the recorded original",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := acceptCurrentDirectConfig(test.state, current, test.expected)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}
