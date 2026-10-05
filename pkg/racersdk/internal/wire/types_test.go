// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestETag(t *testing.T) {
	for _, s := range []string{`""`, `"a,b"`, `"a\b"`, `"!#~"`, `"` + strings.Repeat("a", 8190) + `"`} {
		if err := ValidateETag(s); err != nil {
			t.Fatalf("valid tag: %v", err)
		}
	}

	for _, s := range []string{"", `*`, `W/"v"`, `"a", "b"`, `"a"b"`, `"a b"`, "\"\x80\"", "\"\t\"", `"` + strings.Repeat("a", 8191) + `"`} {
		assertKind(t, ValidateETag(s), ErrorInvalidArgument)
	}
}

func TestOpaque(t *testing.T) {
	for _, s := range []string{"", " leading", "trailing ", "a\tb", "a\rb", "a\nb", "a\x00b", "a\x7fb"} {
		assertKind(t, ValidateOpaque(s), ErrorInvalidArgument)
	}

	for _, s := range []string{strings.Repeat("x", 8192), "opaque, credential\xff"} {
		if err := ValidateOpaque(s); err != nil {
			t.Fatal(err)
		}
	}

	assertKind(t, ValidateOpaque(strings.Repeat("x", 8193)), ErrorHeaderLimit)
}

func TestMetadata(t *testing.T) {
	tag := `""`
	for _, expiry := range []time.Time{time.UnixMilli(0), time.UnixMilli(math.MaxInt64), time.UnixMilli(123).In(time.FixedZone("offset", 3600))} {
		m := Metadata{Size: math.MaxInt64, ETag: tag, ExpiresAt: expiry}
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}

		h, err := MetadataHeaders(m)
		if err != nil {
			t.Fatal(err)
		}

		parsed, err := Decimal(h.Get("Racer-Expires-At"))
		if err != nil || int64(parsed) != expiry.UnixMilli() {
			t.Fatal("expiry changed")
		}
	}

	for _, m := range []Metadata{{}, {ETag: tag}, {ETag: tag, ExpiresAt: time.UnixMilli(-1)}, {ETag: tag, ExpiresAt: time.Unix(0, 1)}, {ETag: tag, ExpiresAt: time.UnixMilli(math.MaxInt64).Add(time.Millisecond)}, {Size: math.MaxInt64 + 1, ETag: tag, ExpiresAt: time.UnixMilli(0)}} {
		assertKind(t, m.Validate(), ErrorInvalidArgument)
	}
}
