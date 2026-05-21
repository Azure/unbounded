// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"math"
	"time"
)

// histogramBucket is one bucket in a fixed log-spaced histogram.
type histogramBucket struct {
	GeNs  int64 `json:"ge_ns"`
	LtNs  int64 `json:"lt_ns"`
	Count int64 `json:"count"`
}

// histogram is a fixed-width log-spaced histogram of latency samples
// suitable for emission in JSON. Bucket boundaries are derived from
// (LowerNs, UpperNs, BucketCount) so a downstream consumer can
// regenerate them without parsing per-bucket fields.
//
// Out-of-range samples are counted in UnderflowCount / OverflowCount
// so totals always reconcile with the source sample count.
type histogram struct {
	Scale          string            `json:"scale"`
	LowerNs        int64             `json:"lower_ns"`
	UpperNs        int64             `json:"upper_ns"`
	BucketCount    int               `json:"bucket_count"`
	UnderflowCount int64             `json:"underflow_count"`
	OverflowCount  int64             `json:"overflow_count"`
	Buckets        []histogramBucket `json:"buckets"`
}

// buildHistogram returns a log-spaced histogram of samples bounded
// by [lower, upper] with bucketCount buckets. Bucket boundaries are
// distributed evenly in log space: bucket i spans
//
//	lower * (upper/lower)^(i/N), lower * (upper/lower)^((i+1)/N)
//
// Samples < lower fall into UnderflowCount; samples >= upper fall
// into OverflowCount. The boundaries are clamped to int64 nanos and
// every Count is int64 to allow long benchmark runs.
func buildHistogram(samples []time.Duration, lower, upper time.Duration, bucketCount int) histogram {
	if bucketCount < 1 {
		bucketCount = 1
	}

	if lower <= 0 {
		lower = 1
	}

	if upper <= lower {
		upper = lower * 2
	}

	logRatio := math.Log(float64(upper) / float64(lower))

	// Pre-compute the bucket boundaries so the per-sample loop only
	// does an O(log N) lookup. boundaries[i] is the LOWER bound of
	// bucket i; boundaries[bucketCount] is upper.
	boundaries := make([]int64, bucketCount+1)
	for i := 0; i <= bucketCount; i++ {
		// frac in [0, 1]; pow in [1, upper/lower].
		frac := float64(i) / float64(bucketCount)
		pow := math.Exp(frac * logRatio)
		boundaries[i] = int64(float64(lower) * pow)
	}
	// Force exact endpoints to avoid float rounding leaving a sample
	// in underflow or overflow due to a sub-ns off-by-one.
	boundaries[0] = int64(lower)
	boundaries[bucketCount] = int64(upper)

	h := histogram{
		Scale:       "log",
		LowerNs:     int64(lower),
		UpperNs:     int64(upper),
		BucketCount: bucketCount,
		Buckets:     make([]histogramBucket, bucketCount),
	}
	for i := 0; i < bucketCount; i++ {
		h.Buckets[i] = histogramBucket{
			GeNs: boundaries[i],
			LtNs: boundaries[i+1],
		}
	}

	for _, s := range samples {
		ns := int64(s)
		switch {
		case ns < boundaries[0]:
			h.UnderflowCount++
		case ns >= boundaries[bucketCount]:
			h.OverflowCount++
		default:
			// Binary search for the bucket whose [Ge, Lt) range
			// contains ns. lower is inclusive, upper exclusive.
			lo, hi := 0, bucketCount
			for lo < hi {
				mid := (lo + hi) / 2
				if ns < boundaries[mid] {
					hi = mid
				} else if ns >= boundaries[mid+1] {
					lo = mid + 1
				} else {
					lo = mid
					break
				}
			}

			h.Buckets[lo].Count++
		}
	}

	return h
}
