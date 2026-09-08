// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package digest

import "testing"

func TestParse_Valid(t *testing.T) {
	in := "sha256:" + repeat("a", 64)

	d, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if d.Algorithm() != SHA256 {
		t.Errorf("algo = %q, want sha256", d.Algorithm())
	}

	if d.String() != in {
		t.Errorf("String() = %q, want %q", d.String(), in)
	}

	if d.IsZero() {
		t.Error("IsZero() = true for valid digest")
	}
}

func TestParse_Invalid(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"no colon":     "sha256" + repeat("a", 64),
		"empty algo":   ":" + repeat("a", 64),
		"empty hex":    "sha256:",
		"unknown algo": "md5:" + repeat("a", 32),
		"short hex":    "sha256:" + repeat("a", 63),
		"long hex":     "sha256:" + repeat("a", 65),
		"non-hex":      "sha256:" + repeat("z", 64),
		"upper hex":    "sha256:" + repeat("A", 64),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Errorf("Parse(%q) succeeded, want error", in)
			}
		})
	}
}

func TestZeroValue(t *testing.T) {
	var d Digest
	if !d.IsZero() {
		t.Error("zero Digest.IsZero() = false")
	}

	if d.String() != "" {
		t.Errorf("zero Digest.String() = %q, want \"\"", d.String())
	}
}

func repeat(s string, n int) string {
	out := make([]byte, n*len(s))
	for i := 0; i < n; i++ {
		copy(out[i*len(s):], s)
	}

	return string(out)
}
