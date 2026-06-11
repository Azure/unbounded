// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"testing"
	"time"
)

func TestBucketIndexRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 5, 63, 64, 65, 100, 1000, 1 << 20, 1234567} {
		idx := bucketIndex(v)
		lo := bucketValue(idx)

		// The representative value is the lower bound of the bucket, so it
		// must never exceed v, and the next bucket must exceed v.
		if lo > v {
			t.Errorf("v=%d: bucketValue(%d)=%d exceeds v", v, idx, lo)
		}

		if idx+1 < sketchBucketCount {
			if next := bucketValue(idx + 1); next <= v && next != 0 {
				t.Errorf("v=%d: next bucket value %d does not exceed v", v, next)
			}
		}
	}
}

func TestLatencySketchQuantiles(t *testing.T) {
	var s latencySketch

	if got := s.quantile(0.5); got != 0 {
		t.Fatalf("empty sketch quantile = %v, want 0", got)
	}

	// Record 1000 samples at 10ms.
	for i := 0; i < 1000; i++ {
		s.record(10 * time.Millisecond)
	}

	for _, q := range []float64{0.5, 0.95, 0.99} {
		got := s.quantile(q)
		// Allow generous bounds for log-linear bucket error.
		if got < 9*time.Millisecond || got > 11*time.Millisecond {
			t.Errorf("quantile(%g) = %v, want ~10ms", q, got)
		}
	}
}

func TestLatencySketchSpread(t *testing.T) {
	var s latencySketch

	for i := 0; i < 970; i++ {
		s.record(time.Millisecond)
	}

	for i := 0; i < 30; i++ {
		s.record(time.Second)
	}

	p50 := s.quantile(0.5)
	p99 := s.quantile(0.99)

	if p50 > 5*time.Millisecond {
		t.Errorf("p50 = %v, want ~1ms", p50)
	}

	if p99 < 500*time.Millisecond {
		t.Errorf("p99 = %v, want close to 1s", p99)
	}
}

func TestMetricsObserve(t *testing.T) {
	m := newMetrics()

	m.observe("GET", "200", 5*time.Millisecond, 1024, false)
	m.observe("GET", "404", time.Millisecond, 0, true)

	if got := m.reqTotal.Load(); got != 2 {
		t.Errorf("reqTotal = %d, want 2", got)
	}

	if got := m.errTotal.Load(); got != 1 {
		t.Errorf("errTotal = %d, want 1", got)
	}

	if got := m.byteTotal.Load(); got != 1024 {
		t.Errorf("byteTotal = %d, want 1024", got)
	}
}
