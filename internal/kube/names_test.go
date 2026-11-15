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
			name:     "label longer than 63 bytes preserved",
			input:    strings.Repeat("a", 70),
			expected: strings.Repeat("a", 70),
		},
		{
			name:     "long label among short labels preserved",
			input:    "a." + strings.Repeat("b", 70) + ".c",
			expected: "a." + strings.Repeat("b", 70) + ".c",
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

	// Kubernetes caps the total name at 253 bytes but does not cap individual
	// labels, so a single label longer than 63 bytes must survive intact only
	// when the whole name fits.
	seventy := strings.Repeat("a", 70)
	require.Equal(t, seventy, SanitizeK8sName(seventy))

	longLabel := strings.Repeat("a", 200)
	input := strings.Join([]string{longLabel, longLabel, longLabel}, ".")

	got := SanitizeK8sName(input)

	// The total cap truncates mid-label: 200 a's, a dot, then 52 more.
	require.Equal(t, longLabel+"."+strings.Repeat("a", 52), got)
	require.Len(t, got, 253)
	require.Empty(t, validation.IsDNS1123Subdomain(got))
}

func TestSanitizeK8sNameIdempotent(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"",
		"!!!",
		"...",
		"a..b",
		"a-.b",
		"a.-b",
		"---abc---",
		"MyMachine-01",
		"10.0.0.1:22",
		strings.Repeat("a", 70),
		strings.Repeat("a", 200) + "." + strings.Repeat("b", 200),
		strings.Join([]string{strings.Repeat("x-", 100), strings.Repeat("y.", 100)}, "."),
	} {
		first := SanitizeK8sName(input)
		second := SanitizeK8sName(first)

		require.Equal(t, first, second, "sanitization must be idempotent for %q", input)
		require.Empty(t, validation.IsDNS1123Subdomain(first), "result must be a valid DNS1123 subdomain")
		require.LessOrEqual(t, len(first), 253)
	}
}

func TestSanitizeK8sNamePropertyTable(t *testing.T) {
	t.Parallel()

	// Fuzz-style table: every result must be valid, non-empty, and unchanged on
	// a second pass.
	inputs := []string{
		"a", "A", "0", "-", ".", "..", ".-", "-.", "a.b", "a..b", "a-.b", "a.-b",
		"!!!", "a b", "a--b", "a_-_b", "a...b", ".a.", "-a-", "a..", "..a",
		"Machine_Name.With:Weird@Chars", "20.48.100.5-50001", "10.0.0.1:22",
		strings.Repeat("a", 63), strings.Repeat("a", 64), strings.Repeat("a", 70),
		strings.Repeat("a", 253), strings.Repeat("a", 254), strings.Repeat("a", 300),
		strings.Repeat("a.", 200) + "z", strings.Repeat("a-", 200) + "z",
	}

	for _, input := range inputs {
		got := SanitizeK8sName(input)

		require.NotEmpty(t, got, "sanitized name must not be empty for %q", input)
		require.Empty(t, validation.IsDNS1123Subdomain(got), "invalid result for %q: %q", input, got)
		require.LessOrEqual(t, len(got), 253, "result too long for %q", input)
		require.Equal(t, got, SanitizeK8sName(got), "not idempotent for %q", input)
	}

	// Canonical names that already pass IsDNS1123Subdomain must be returned
	// unchanged, regardless of label length.
	for _, input := range []string{
		"a",
		"a.b",
		"20.48.100.5-50001",
		strings.Repeat("a", 63),
		strings.Repeat("a", 64),
		strings.Repeat("a", 70),
		strings.Repeat("a", 253),
		"a." + strings.Repeat("b", 70) + ".c",
	} {
		require.Empty(t, validation.IsDNS1123Subdomain(input), "fixture must be a valid name")
		require.Equal(t, input, SanitizeK8sName(input), "valid input must be preserved: %q", input)
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
