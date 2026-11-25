// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"strings"
	"testing"
)

// TestParseSize covers every accepted suffix and the error paths.
// Lifted from orcaseed; orcadev consumes the same size grammar.
func TestParseSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1024", 1024, false},
		{"0", 0, false},
		{"1B", 1, false},
		{"1KB", 1000, false},
		{"1KiB", 1024, false},
		{"10MB", 10_000_000, false},
		{"10MiB", 10 * 1024 * 1024, false},
		{"1GB", 1_000_000_000, false},
		{"1GiB", 1024 * 1024 * 1024, false},
		{"1.5GB", 1_500_000_000, false},
		{"  10MiB  ", 10 * 1024 * 1024, false},
		{"10mib", 10 * 1024 * 1024, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1XB", 0, true},
		{"-5MB", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseSize(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseSize(%q) = %d, want error", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Errorf("parseSize(%q) unexpected error %v", tt.in, err)
				return
			}

			if got != tt.want {
				t.Errorf("parseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestFormatSize covers each unit boundary plus zero.
func TestFormatSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
		{1024 * 1024 * 1024 * 1024, "1.00 TiB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatSize(tt.in)
			if got != tt.want {
				t.Errorf("formatSize(%d) = %q want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFirstDiffOffset covers prefix, mid, end, equal, and length
// mismatch cases.
func TestFirstDiffOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b []byte
		want int
	}{
		{"equal", []byte("hello"), []byte("hello"), -1},
		{"differ at 0", []byte("a"), []byte("b"), 0},
		{"differ at 2", []byte("abcd"), []byte("abxd"), 2},
		{"a shorter", []byte("abc"), []byte("abcd"), 3},
		{"b shorter", []byte("abcd"), []byte("abc"), 3},
		{"both empty", []byte{}, []byte{}, -1},
		{"a empty", []byte{}, []byte("a"), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstDiffOffset(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("firstDiffOffset(%q, %q) = %d want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// TestHexDiffDump verifies the dump renders both side-by-side and
// flags the offset on the first line. Snapshot-style assertion on
// substrings only; the exact alignment is not load-bearing.
func TestHexDiffDump(t *testing.T) {
	t.Parallel()

	a := []byte("hello world!!!")
	b := []byte("hello WORLD!!!")
	got := hexDiffDump(a, b, 0, 16)

	for _, want := range []string{
		"offset 0x0 (0)",
		"hello world",
		"hello WORLD",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hex dump missing %q\n%s", want, got)
		}
	}
}
