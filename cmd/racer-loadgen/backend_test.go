// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"hash/fnv"
	"os"
	"testing"
)

func TestRangeRotationZeroAndSingleRange(t *testing.T) {
	rotating := &puller{opts: pullOptions{RangeBytes: 10, RangeRotate: true}}
	cohorts := &puller{opts: pullOptions{RangeBytes: 10, RangeCohorts: 4, NodeName: "w0006"}}

	for _, p := range []*puller{rotating, cohorts} {
		// A zero-size object has no ranges and must not divide by zero.
		if got := p.rangeRotation(0); got != 0 {
			t.Fatalf("size 0 rotation = %d, want 0", got)
		}

		// A size smaller than one range and exactly one range both have a
		// single possible start.
		for _, size := range []int64{5, 10} {
			if got := p.rangeRotation(size); got != 0 {
				t.Fatalf("size %d rotation = %d, want 0", size, got)
			}
		}
	}
}

func TestRangeRotationMultipleRanges(t *testing.T) {
	name, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) //nolint:errcheck // hash.Hash never fails.

	sum := h.Sum64()

	p := &puller{opts: pullOptions{RangeBytes: 10, RangeRotate: true}}

	seen := map[int64]bool{}

	for _, ranges := range []int64{2, 3, 5, 7, 11} {
		got := p.rangeRotation(ranges * 10)
		if want := int64(sum % uint64(ranges)); got != want {
			t.Fatalf("ranges %d rotation = %d, want %d", ranges, got, want)
		}

		if got < 0 || got >= ranges {
			t.Fatalf("ranges %d rotation %d out of bounds", ranges, got)
		}

		seen[got] = true
	}

	// A constant result across these range counts means the hash is 0 or 1
	// modulo 2310, their least common multiple.
	if sum%2310 > 1 && len(seen) < 2 {
		t.Fatalf("rotation did not vary across range counts: %v", seen)
	}
}
