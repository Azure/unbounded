// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		received int
		want     int
	}{
		{name: "no replies received", received: 0, want: 1},
		{name: "one reply received", received: 1, want: 0},
		{name: "some replies received", received: 10, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.received); got != tt.want {
				t.Errorf("exitCode(%d) = %d, want %d", tt.received, got, tt.want)
			}
		})
	}
}
