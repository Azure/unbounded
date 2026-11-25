// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"testing"
)

func TestParseByteRange(t *testing.T) {
	t.Parallel()

	const size = 1024

	tests := []struct {
		name      string
		spec      string
		wantStart int64
		wantEnd   int64
		wantErr   bool
	}{
		{"full closed", "bytes=0-99", 0, 99, false},
		{"open end", "bytes=100-", 100, 1023, false},
		{"suffix", "bytes=-50", 974, 1023, false},
		{"clamp end", "bytes=0-99999", 0, 1023, false},
		{"missing prefix", "0-99", 0, 0, true},
		{"missing dash", "bytes=99", 0, 0, true},
		{"end before start", "bytes=99-10", 0, 0, true},
		{"non-numeric", "bytes=a-b", 0, 0, true},
		{"suffix too large", "bytes=-9999", 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, e, err := parseByteRange(tt.spec, size)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseByteRange(%q) = (%d, %d, nil) want error", tt.spec, s, e)
				}

				return
			}

			if err != nil {
				t.Errorf("parseByteRange(%q) unexpected error: %v", tt.spec, err)
				return
			}

			if s != tt.wantStart || e != tt.wantEnd {
				t.Errorf("parseByteRange(%q) = (%d, %d) want (%d, %d)",
					tt.spec, s, e, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestOriginHashPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"inttest-origin/abc123/0", "inttest-origin/abc123"},
		{"inttest-origin/abc123/47", "inttest-origin/abc123"},
		{"only-one-segment", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := originHashPrefix(tt.in); got != tt.want {
				t.Errorf("originHashPrefix(%q) = %q want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitBucketKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in string
		wb string
		wk string
	}{
		{"bucket/key", "bucket", "key"},
		{"bucket/nested/key", "bucket", "nested/key"},
		{"justbucket", "", ""},
		{"/key", "", ""},
		{"bucket/", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			b, k := splitBucketKey(tt.in)
			if b != tt.wb || k != tt.wk {
				t.Errorf("splitBucketKey(%q) = (%q, %q) want (%q, %q)",
					tt.in, b, k, tt.wb, tt.wk)
			}
		})
	}
}

func TestDefaultGlobalFlags(t *testing.T) {
	t.Parallel()

	g := defaultGlobalFlags()
	if g.orcaURL == "" {
		t.Error("orcaURL should have a default")
	}

	if g.originDriver != "azureblob" {
		t.Errorf("originDriver default = %q want azureblob", g.originDriver)
	}

	if g.cachestoreEndpoint == "" {
		t.Error("cachestoreEndpoint should have a default")
	}

	if g.originAccountKey == "" {
		t.Error("originAccountKey should default to the Azurite well-known key")
	}
}
