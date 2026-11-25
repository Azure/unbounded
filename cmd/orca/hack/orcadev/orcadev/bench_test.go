// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	mathrand "math/rand"
	"sync/atomic"
	"testing"
	"time"
)

func TestBenchResolveStopCondition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		opts         benchOpts
		wantDuration time.Duration
		wantRequests int
		wantErr      bool
	}{
		{
			name:         "default duration",
			opts:         benchOpts{durationStr: "30s"},
			wantDuration: 30 * time.Second,
		},
		{
			name:         "requests ignores default duration",
			opts:         benchOpts{durationStr: "30s", requests: 100},
			wantDuration: 0,
			wantRequests: 100,
		},
		{
			name:    "requests rejects explicit duration",
			opts:    benchOpts{durationStr: "10s", durationSet: true, requests: 100},
			wantErr: true,
		},
		{
			name:         "requests allows explicit empty duration",
			opts:         benchOpts{durationStr: "", durationSet: true, requests: 100},
			wantDuration: 0,
			wantRequests: 100,
		},
		{
			name:    "explicit zero duration rejected",
			opts:    benchOpts{durationStr: "0s", durationSet: true},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotDuration, gotRequests, err := tt.opts.resolveStopCondition()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveStopCondition() = nil error, want error")
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveStopCondition() unexpected error: %v", err)
			}

			if gotDuration != tt.wantDuration || gotRequests != tt.wantRequests {
				t.Fatalf("resolveStopCondition() = (%s, %d), want (%s, %d)",
					gotDuration, gotRequests, tt.wantDuration, tt.wantRequests)
			}
		})
	}
}

// TestPickBenchRange exercises each placement branch:
//
//   - --full forces [0, size-1] regardless of rangeSize.
//   - rangeSize >= size short-circuits to the full path even when
//     --full is not set.
//   - random returns a valid in-bounds range whose width equals
//     rangeSize; we check 256 draws against a small object so we
//     stress the boundary.
//   - sequential advances by rangeSize per call and wraps cleanly
//     at the object boundary.
func TestPickBenchRange(t *testing.T) {
	t.Parallel()

	const (
		objSize   = int64(10_000)
		rangeSize = int64(1024)
	)

	rng := mathrand.New(mathrand.NewSource(1)) //nolint:gosec // deterministic test RNG

	t.Run("full", func(t *testing.T) {
		var off atomic.Int64

		s, e := pickBenchRange(rng, "sequential", true, objSize, rangeSize, &off)
		if s != 0 || e != objSize-1 {
			t.Fatalf("full = (%d, %d), want (0, %d)", s, e, objSize-1)
		}
	})

	t.Run("range>=size auto-full", func(t *testing.T) {
		var off atomic.Int64

		s, e := pickBenchRange(rng, "random", false, objSize, objSize+1, &off)
		if s != 0 || e != objSize-1 {
			t.Fatalf("auto-full = (%d, %d), want (0, %d)", s, e, objSize-1)
		}
	})

	t.Run("random stays in bounds", func(t *testing.T) {
		var off atomic.Int64

		for i := 0; i < 256; i++ {
			s, e := pickBenchRange(rng, "random", false, objSize, rangeSize, &off)
			if s < 0 || e < s || e >= objSize {
				t.Fatalf("random draw %d: (%d, %d) out of bounds for size %d", i, s, e, objSize)
			}

			if e-s+1 != rangeSize {
				t.Fatalf("random draw %d: width %d, want %d", i, e-s+1, rangeSize)
			}
		}
	})

	t.Run("sequential advances and wraps", func(t *testing.T) {
		var off atomic.Int64

		// Object is 10000 bytes, range is 1024. After 9 strides
		// we've consumed 9216 bytes; the 10th stride would start at
		// 9216 and end at 10239 - past the boundary, so we expect
		// clamping. The 11th stride wraps via the modulus.
		for i := 0; i < 9; i++ {
			s, e := pickBenchRange(rng, "sequential", false, objSize, rangeSize, &off)
			wantStart := int64(i) * rangeSize

			if s != wantStart {
				t.Fatalf("stride %d: start %d, want %d", i, s, wantStart)
			}

			if e-s+1 != rangeSize {
				t.Fatalf("stride %d: width %d, want %d", i, e-s+1, rangeSize)
			}
		}

		s, e := pickBenchRange(rng, "sequential", false, objSize, rangeSize, &off)
		if s != 9*rangeSize || e != objSize-1 {
			t.Fatalf("boundary stride: (%d, %d), want (%d, %d)", s, e, 9*rangeSize, objSize-1)
		}

		// Next stride wraps. nextOffset is now 10*rangeSize == 10240,
		// modulo 10000 yields 240.
		s, _ = pickBenchRange(rng, "sequential", false, objSize, rangeSize, &off)
		if s != 240 {
			t.Fatalf("wrapped stride start = %d, want 240", s)
		}
	})
}
