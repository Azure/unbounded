// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"math"
	"strings"

	"github.com/dustin/go-humanize"
	"gopkg.in/yaml.v3"
)

// ByteSize is an int64 byte count with a YAML unmarshal hook that
// accepts either a numeric scalar (legacy form: `size: 8388608`) or
// a human-readable string scalar (`size: 8 MiB`, `size: 1.5 GiB`,
// `size: 128MiB`, `size: 1 GB`).
//
// SI suffixes (KB, MB, GB, TB, PB) are decimal multipliers (powers
// of ten); IEC suffixes (KiB, MiB, GiB, TiB, PiB) are binary
// multipliers (powers of two). This matches the convention used by
// Kubernetes resource quantities, most container tooling, and the
// IEC standard. Operators who mean exactly 1 048 576 bytes should
// write "1 MiB"; "1 MB" is 1 000 000.
//
// Fractional values are allowed and truncated by the underlying
// parser ("1.5 GiB" -> 1 610 612 736). Negative values, NaN,
// overflow above int64 max, and empty / whitespace-only scalars are
// rejected at unmarshal time with a message tagged with the YAML
// line number for ease of locating the offending entry.
//
// The zero value is 0, which applyDefaults treats as "field
// omitted" for fields that have a default fallback (e.g.
// Chunking.Size).
type ByteSize int64

// UnmarshalYAML implements yaml.Unmarshaler. The accepted forms are
// described on ByteSize. The function trims surrounding whitespace
// and rejects negatives up front so the operator sees a
// bytesize-flavored error rather than humanize.ParseBytes's
// less-specific "unhandled size name" surface.
func (b *ByteSize) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf(
			"line %d: bytesize must be a scalar (integer bytes or human-readable string like \"8 MiB\"); got node kind %d",
			value.Line, value.Kind,
		)
	}

	raw := strings.TrimSpace(value.Value)
	if raw == "" {
		return fmt.Errorf("line %d: bytesize is empty", value.Line)
	}
	// Reject negatives explicitly. humanize.ParseBytes is built on
	// uint64 and would reject "-1 MiB" with a generic message; the
	// explicit check produces a clearer error.
	if strings.HasPrefix(raw, "-") {
		return fmt.Errorf("line %d: bytesize %q invalid; must be >= 0", value.Line, raw)
	}

	u, err := humanize.ParseBytes(raw)
	if err != nil {
		return fmt.Errorf("line %d: parse bytesize %q: %w", value.Line, raw, err)
	}

	if u > math.MaxInt64 {
		return fmt.Errorf("line %d: bytesize %q overflows int64", value.Line, raw)
	}

	*b = ByteSize(u)

	return nil
}

// String renders the byte count using IEC units (e.g. "8.0 MiB",
// "1.5 GiB"). Used in validation error messages so operators see
// the offending value in human-friendly units regardless of how it
// was written in YAML.
func (b ByteSize) String() string {
	if b < 0 {
		return fmt.Sprintf("%d B", int64(b))
	}

	return humanize.IBytes(uint64(b))
}

// Int64 returns the raw byte count as an int64. Provided as an
// explicit accessor so callsites that hand the value to int64-typed
// APIs (chunk.SizeFor, chunk.Tier.ChunkSize) read naturally without
// scattered int64(...) casts.
func (b ByteSize) Int64() int64 { return int64(b) }
