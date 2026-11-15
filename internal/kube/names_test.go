// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
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
		{
			name:     "empty label removed",
			input:    "a..b",
			expected: "a.b",
		},
		{
			name:     "label with trailing dash trimmed",
			input:    "a-.b",
			expected: "a.b",
		},
		{
			name:     "label with leading dash trimmed",
			input:    "a.-b",
			expected: "a.b",
		},
		{
			name:     "valid dotted name unchanged",
			input:    "a.b.c",
			expected: "a.b.c",
		},
		{
			name:     "label longer than 63 bytes truncated",
			input:    strings.Repeat("a", 100),
			expected: strings.Repeat("a", 63),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := SanitizeK8sName(tt.input)
			require.Equal(t, tt.expected, got)
			require.Empty(t, validation.IsDNS1123Subdomain(got), "result must be a valid DNS1123 subdomain")
			require.LessOrEqual(t, len(got), 253)
		})
	}
}

func TestSanitizeK8sNameLengthLimits(t *testing.T) {
	t.Parallel()

	longLabel := strings.Repeat("a", 63)
	input := strings.Join([]string{longLabel, longLabel, longLabel, longLabel, longLabel}, ".")

	got := SanitizeK8sName(input)

	require.LessOrEqual(t, len(got), 253)
	require.Empty(t, validation.IsDNS1123Subdomain(got))
	for _, label := range strings.Split(got, ".") {
		require.LessOrEqual(t, len(label), 63)
	}
}

func TestSanitizeK8sNameFallback(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"!!!", "...", "-.-", ""} {
		first := SanitizeK8sName(input)
		second := SanitizeK8sName(input)

		require.NotEmpty(t, first)
		require.Equal(t, first, second, "fallback must be stable for %q", input)
		require.Empty(t, validation.IsDNS1123Subdomain(first), "fallback must be a valid DNS1123 subdomain")
		require.LessOrEqual(t, len(first), 253)
	}
}
