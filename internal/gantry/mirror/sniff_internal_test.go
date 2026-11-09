// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/manifest"
)

// TestSniffWriterBoundsWhitespacePrefix checks that a body of only
// whitespace is released once it reaches the detector's limit instead of
// being buffered without bound.
func TestSniffWriterBoundsWhitespacePrefix(t *testing.T) {
	var out bytes.Buffer

	var prefixLen int

	s := &sniffWriter{w: &out}
	s.deferHeaders(func(prefix []byte) { prefixLen = len(prefix) })

	chunk := bytes.Repeat([]byte(" "), 64<<10)
	limit := int(manifest.MaxContentTypeDetectionBytes)

	for written := 0; written < 2*limit; written += len(chunk) {
		if _, err := s.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}

		if len(s.buf) > limit {
			t.Fatalf("buffered %d bytes, limit %d", len(s.buf), limit)
		}
	}

	if s.headers != nil {
		t.Fatal("headers still deferred after the detector limit")
	}

	if prefixLen != limit {
		t.Fatalf("sniffed prefix %d bytes, want %d", prefixLen, limit)
	}

	if out.Len() != 2*limit {
		t.Fatalf("wrote %d bytes, want %d", out.Len(), 2*limit)
	}
}

// TestSniffWriterBuffersWhitespaceThenJSON checks that leading whitespace
// split across writes still defers headers for a JSON body and releases a
// non-JSON body at once.
func TestSniffWriterBuffersWhitespaceThenJSON(t *testing.T) {
	for _, tc := range []struct {
		name     string
		second   string
		deferred bool
	}{
		{"json", `{"schemaVersion":2`, true},
		{"layer", "LLLL", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			s := &sniffWriter{w: &out}
			s.deferHeaders(func([]byte) {})

			for _, p := range []string{" \n", "\t ", tc.second} {
				if _, err := s.Write([]byte(p)); err != nil {
					t.Fatalf("Write: %v", err)
				}
			}

			if got := s.headers != nil; got != tc.deferred {
				t.Fatalf("headers deferred = %v, want %v", got, tc.deferred)
			}

			if s.lead != 4 && tc.deferred {
				t.Fatalf("lead %d, want 4", s.lead)
			}
		})
	}
}
