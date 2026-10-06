// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"testing"
	"time"
)

func TestPercentileSorted(t *testing.T) {
	t.Parallel()

	// percentileSorted requires ascending input; computeLatencyStats is what
	// sorts before calling it.
	samples := func() []time.Duration {
		return []time.Duration{
			1 * time.Millisecond,
			2 * time.Millisecond,
			3 * time.Millisecond,
			5 * time.Millisecond,
			7 * time.Millisecond,
			10 * time.Millisecond,
			20 * time.Millisecond,
			50 * time.Millisecond,
			100 * time.Millisecond,
			200 * time.Millisecond,
		}
	}

	tests := []struct {
		name string
		v    float64
		want time.Duration
	}{
		{"p0 -> min", 0, 1 * time.Millisecond},
		{"p50", 50, 7 * time.Millisecond},
		{"p90", 90, 100 * time.Millisecond},
		{"p99", 99, 200 * time.Millisecond},
		{"p100 -> max", 100, 200 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentileSorted(samples(), tt.v)
			if got != tt.want {
				t.Errorf("percentileSorted(%v) = %v want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestPercentileSorted_Empty(t *testing.T) {
	t.Parallel()

	if got := percentileSorted([]time.Duration{}, 50); got != 0 {
		t.Errorf("percentileSorted on empty samples = %v want 0", got)
	}
}

func TestComputeLatencyStats(t *testing.T) {
	t.Parallel()

	samples := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
		6 * time.Millisecond,
		7 * time.Millisecond,
		8 * time.Millisecond,
		9 * time.Millisecond,
		10 * time.Millisecond,
	}

	got := computeLatencyStats(samples)

	if got.Min != 1*time.Millisecond {
		t.Errorf("Min = %v", got.Min)
	}

	if got.Max != 10*time.Millisecond {
		t.Errorf("Max = %v", got.Max)
	}

	if got.P50 != 5*time.Millisecond {
		t.Errorf("P50 = %v want 5ms", got.P50)
	}

	if got.P90 != 9*time.Millisecond {
		t.Errorf("P90 = %v want 9ms", got.P90)
	}

	if got.P99 != 10*time.Millisecond {
		t.Errorf("P99 = %v want 10ms", got.P99)
	}
}

func TestBuildHistogram_BasicCounts(t *testing.T) {
	t.Parallel()

	samples := []time.Duration{
		50 * time.Microsecond,  // underflow (< 100us)
		200 * time.Microsecond, // first bucket
		400 * time.Microsecond, // first bucket
		1 * time.Millisecond,
		5 * time.Millisecond,
		100 * time.Millisecond,
		20 * time.Second, // overflow (> 10s)
	}

	h := buildHistogram(samples, 100*time.Microsecond, 10*time.Second, 50)

	if h.BucketCount != 50 {
		t.Errorf("BucketCount = %d want 50", h.BucketCount)
	}

	if h.UnderflowCount != 1 {
		t.Errorf("UnderflowCount = %d want 1", h.UnderflowCount)
	}

	if h.OverflowCount != 1 {
		t.Errorf("OverflowCount = %d want 1", h.OverflowCount)
	}

	var sum int64
	for _, b := range h.Buckets {
		sum += b.Count
	}

	const inRange = int64(5)
	if sum != inRange {
		t.Errorf("in-range bucket total = %d want %d", sum, inRange)
	}

	if int64(len(samples)) != sum+h.UnderflowCount+h.OverflowCount {
		t.Errorf("sample count reconciliation failed")
	}
}

func TestBuildHistogram_BoundsAreMonotonic(t *testing.T) {
	t.Parallel()

	h := buildHistogram(nil, 1*time.Microsecond, 10*time.Second, 30)

	prev := int64(-1)
	for _, b := range h.Buckets {
		if b.GeNs <= prev {
			t.Fatalf("bucket lower bounds not monotonic: prev=%d cur=%d", prev, b.GeNs)
		}

		if b.LtNs <= b.GeNs {
			t.Fatalf("bucket upper bound %d <= lower %d", b.LtNs, b.GeNs)
		}

		prev = b.GeNs
	}
}

func TestBuildHistogram_AssignsToCorrectBucket(t *testing.T) {
	t.Parallel()
	// Build a histogram with deterministic bounds and verify each
	// sample lands in a bucket containing it.
	h := buildHistogram(nil, 1*time.Millisecond, 1*time.Second, 6)
	// One sample per bucket midpoint.
	var samples []time.Duration

	for i := 0; i < len(h.Buckets); i++ {
		mid := (h.Buckets[i].GeNs + h.Buckets[i].LtNs) / 2
		samples = append(samples, time.Duration(mid))
	}

	h = buildHistogram(samples, 1*time.Millisecond, 1*time.Second, 6)

	for i, b := range h.Buckets {
		if b.Count != 1 {
			t.Errorf("bucket %d count = %d want 1 (ge=%d lt=%d)", i, b.Count, b.GeNs, b.LtNs)
		}
	}
}
