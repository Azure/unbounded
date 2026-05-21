// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"sort"
	"time"
)

// percentile returns the v-th percentile of samples where v is in
// (0, 100]. Empty samples returns 0. The input is sorted in-place;
// callers must pass a slice they own or copy first.
//
// Uses the nearest-rank method with ceiling: index = ceil(N*v/100)-1
// clamped to [0, N-1]. This matches Prometheus / Grafana convention
// for percentile reporting on small-to-medium sample sizes and
// avoids interpolation surprises on log-distributed latency data.
func percentile(samples []time.Duration, v float64) time.Duration {
	n := len(samples)
	if n == 0 {
		return 0
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	if v <= 0 {
		return samples[0]
	}

	if v >= 100 {
		return samples[n-1]
	}

	// nearest-rank, ceiling
	idx := int((v/100.0)*float64(n) + 0.999999)
	if idx < 1 {
		idx = 1
	}

	if idx > n {
		idx = n
	}

	return samples[idx-1]
}

// latencyStats is the canonical set of percentile + min/max numbers
// the bench / scenario subcommands emit in both human and JSON form.
type latencyStats struct {
	Min, P50, P90, P99, Max time.Duration
}

// computeLatencyStats sorts samples (mutating the input) and
// returns canonical min/p50/p90/p99/max. Empty input returns a
// zero-valued latencyStats so JSON output is still well-formed.
func computeLatencyStats(samples []time.Duration) latencyStats {
	if len(samples) == 0 {
		return latencyStats{}
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	return latencyStats{
		Min: samples[0],
		P50: percentileSorted(samples, 50),
		P90: percentileSorted(samples, 90),
		P99: percentileSorted(samples, 99),
		Max: samples[len(samples)-1],
	}
}

// percentileSorted is the same as percentile but skips the sort. Used
// internally by computeLatencyStats so we sort once and read out
// multiple percentiles.
func percentileSorted(samples []time.Duration, v float64) time.Duration {
	n := len(samples)
	if n == 0 {
		return 0
	}

	if v <= 0 {
		return samples[0]
	}

	if v >= 100 {
		return samples[n-1]
	}

	idx := int((v/100.0)*float64(n) + 0.999999)
	if idx < 1 {
		idx = 1
	}

	if idx > n {
		idx = n
	}

	return samples[idx-1]
}
