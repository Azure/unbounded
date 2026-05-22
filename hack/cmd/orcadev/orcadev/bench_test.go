// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"testing"
	"time"
)

func TestBenchResolveStopCondition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		opts         benchOpts
		wantDuration time.Duration
		wantRequests int
		wantErr      bool
	}{
		{
			name:         "default duration",
			opts:         benchOpts{durationStr: "30s"},
			wantDuration: 30 * time.Second,
		},
		{
			name:         "requests ignores default duration",
			opts:         benchOpts{durationStr: "30s", requests: 100},
			wantDuration: 0,
			wantRequests: 100,
		},
		{
			name:    "requests rejects explicit duration",
			opts:    benchOpts{durationStr: "10s", durationSet: true, requests: 100},
			wantErr: true,
		},
		{
			name:         "requests allows explicit empty duration",
			opts:         benchOpts{durationStr: "", durationSet: true, requests: 100},
			wantDuration: 0,
			wantRequests: 100,
		},
		{
			name:    "explicit zero duration rejected",
			opts:    benchOpts{durationStr: "0s", durationSet: true},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotDuration, gotRequests, err := tt.opts.resolveStopCondition()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveStopCondition() = nil error, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveStopCondition() unexpected error: %v", err)
			}

			if gotDuration != tt.wantDuration || gotRequests != tt.wantRequests {
				t.Fatalf("resolveStopCondition() = (%s, %d), want (%s, %d)",
					gotDuration, gotRequests, tt.wantDuration, tt.wantRequests)
			}
		})
	}
}
