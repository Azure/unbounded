// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package digest defines the canonical OCI digest type used across Gantry.
//
// All Gantry-internal APIs that identify cached or in-flight content use a
// Digest, not a raw string. This package centralizes parsing and validation
// so the format is enforced exactly once.
package digest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Algorithm identifies a digest hash algorithm.
//
// v1 supports SHA-256 only — that is what the OCI Distribution API uses for
// `blobs/sha256:<hex>` and `manifests/sha256:<hex>` (the only digest-keyed
// endpoints in the agent's API surface, per architecture.md §API).
type Algorithm string

const (
	// SHA256 is the only digest algorithm Gantry accepts (per OCI spec
	// alignment in §4 of detailed-design.md).
	SHA256 Algorithm = "sha256"
)

// Digest is a parsed OCI content digest, e.g. "sha256:abc...".
//
// The zero value is invalid; construct via Parse.
type Digest struct {
	algo Algorithm
	hex  string // lower-case hex; length depends on algo (64 for sha256)
}

// Parse validates s as an OCI digest in the form "<algo>:<hex>" and returns
// the parsed Digest. Unknown algorithms are rejected.
func Parse(s string) (Digest, error) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return Digest{}, fmt.Errorf("digest: malformed %q (expected algo:hex)", s)
	}
	algo := Algorithm(s[:colon])
	hexPart := s[colon+1:]

	switch algo {
	case SHA256:
		if len(hexPart) != 64 {
			return Digest{}, fmt.Errorf("digest: sha256 hex must be 64 chars, got %d", len(hexPart))
		}
	default:
		return Digest{}, fmt.Errorf("digest: unsupported algorithm %q", algo)
	}

	if _, err := hex.DecodeString(hexPart); err != nil {
		return Digest{}, fmt.Errorf("digest: invalid hex: %w", err)
	}
	// Reject uppercase hex — OCI canonicalizes on lower-case.
	if strings.ToLower(hexPart) != hexPart {
		return Digest{}, errors.New("digest: hex must be lower-case")
	}

	return Digest{algo: algo, hex: hexPart}, nil
}

// MustParse is Parse but panics on error. Intended for test fixtures and
// compile-time constants only.
func MustParse(s string) Digest {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

// Algorithm returns the digest's hash algorithm.
func (d Digest) Algorithm() Algorithm { return d.algo }

// Hex returns the lower-case hex-encoded digest body.
func (d Digest) Hex() string { return d.hex }

// String returns the canonical "<algo>:<hex>" form.
func (d Digest) String() string {
	if d.algo == "" {
		return ""
	}
	return string(d.algo) + ":" + d.hex
}

// IsZero reports whether d is the zero Digest (invalid).
func (d Digest) IsZero() bool { return d.algo == "" }
