// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package origin

import "testing"

// TestETagShort covers the truncation contract: ETags 8 characters or
// shorter pass through unchanged; longer ETags are truncated to the
// first 8 characters. The truncation is for log-line readability only;
// callers must not use the short form as a precondition value.
func TestETagShort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"01234567", "01234567"},
		{"012345678", "01234567"},
		{"0x8DDCAFE00000000ABCDEF", "0x8DDCAF"},
	}

	for _, tt := range tests {
		got := ETagShort(tt.in)
		if got != tt.want {
			t.Errorf("ETagShort(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
