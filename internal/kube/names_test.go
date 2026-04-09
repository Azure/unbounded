// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeK8sName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "dots in IP preserved",
			input:    "20.48.100.5-50001",
			expected: "20.48.100.5-50001",
		},
		{
			name:     "colons replaced with dashes",
			input:    "10.0.0.1:22",
			expected: "10.0.0.1-22",
		},
		{
			name:     "underscores replaced",
			input:    "vmss_worker_0",
			expected: "vmss-worker-0",
		},
		{
			name:     "uppercase lowered",
			input:    "MyMachine-01",
			expected: "mymachine-01",
		},
		{
			name:     "leading non-alnum stripped",
			input:    "---abc",
			expected: "abc",
		},
		{
			name:     "trailing non-alnum stripped",
			input:    "abc---",
			expected: "abc",
		},
		{
			name:     "consecutive dashes collapsed",
			input:    "a::b",
			expected: "a-b",
		},
		{
			name:     "already valid name unchanged",
			input:    "machine-1",
			expected: "machine-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, SanitizeK8sName(tt.input))
		})
	}
}
