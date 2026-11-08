// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"math/bits"
	"slices"
	"strings"
)

// histSubBits sets the histogram precision: 2^histSubBits linear buckets per
// power of two, so a bucket's width is at most 1/64 (about 1.6%) of its
// lower bound.
const (
	histSubBits = 6
	histSub     = 1 << histSubBits
	histBuckets = (64 - histSubBits + 1) * histSub
)

// Histogram is an integer log-linear histogram of non-negative values, such
// as latencies in nanoseconds. Quantiles are accurate to about 1.6%.
type Histogram struct {
	counts [histBuckets]uint64
	n      uint64
	sum    uint64
	min    uint64
	max    uint64
}

func histIndex(v uint64) int {
	if v < histSub {
		return int(v)
	}

	e := bits.Len64(v) - histSubBits - 1

	return (e+1)*histSub + int((v>>uint(e))&(histSub-1))
}

// histBounds returns the inclusive bounds of bucket i.
func histBounds(i int) (uint64, uint64) {
	if i < histSub {
		return uint64(i), uint64(i)
	}

	e := i/histSub - 1
	lo := (uint64(histSub) | uint64(i%histSub)) << uint(e)

	return lo, lo + (1 << uint(e)) - 1
}

// Record adds a value. Negative values are recorded as 0.
func (h *Histogram) Record(v int64) {
	u := uint64(max(v, 0))
	h.counts[histIndex(u)]++

	if h.n == 0 || u < h.min {
		h.min = u
	}

	h.max = max(h.max, u)
	h.n++
	h.sum += u
}

// Count returns the number of recorded values.
func (h *Histogram) Count() uint64 { return h.n }

// Max returns the largest recorded value.
func (h *Histogram) Max() int64 { return int64(h.max) }

// Min returns the smallest recorded value.
func (h *Histogram) Min() int64 { return int64(h.min) }

// Mean returns the integer mean of the recorded values.
func (h *Histogram) Mean() int64 {
	if h.n == 0 {
		return 0
	}

	return int64(h.sum / h.n)
}

// Quantile returns an estimate of the q-quantile (0 <= q <= 1), the midpoint
// of the bucket holding it, clamped to the observed min and max.
func (h *Histogram) Quantile(q float64) int64 {
	if h.n == 0 {
		return 0
	}

	q = max(0, min(1, q))
	rank := uint64(q * float64(h.n-1))

	var seen uint64

	for i := range h.counts {
		seen += h.counts[i]
		if seen > rank {
			lo, hi := histBounds(i)
			v := lo + (hi-lo)/2

			return int64(max(h.min, min(h.max, v)))
		}
	}

	return int64(h.max)
}

// Merge adds o's values into h.
func (h *Histogram) Merge(o *Histogram) {
	if o.n == 0 {
		return
	}

	for i := range o.counts {
		h.counts[i] += o.counts[i]
	}

	if h.n == 0 || o.min < h.min {
		h.min = o.min
	}

	h.max = max(h.max, o.max)
	h.n += o.n
	h.sum += o.sum
}

// String summarizes the distribution.
func (h *Histogram) String() string {
	return fmt.Sprintf("n=%d p50=%d p90=%d p99=%d p999=%d max=%d",
		h.n, h.Quantile(0.5), h.Quantile(0.9), h.Quantile(0.99), h.Quantile(0.999), h.max)
}

// Counters is a set of named counters.
type Counters struct {
	m map[string]*uint64
}

// Counter returns a pointer to the named counter for cheap repeated
// increments.
func (c *Counters) Counter(name string) *uint64 {
	if c.m == nil {
		c.m = make(map[string]*uint64)
	}

	p, ok := c.m[name]
	if !ok {
		p = new(uint64)
		c.m[name] = p
	}

	return p
}

// Add adds d to the named counter.
func (c *Counters) Add(name string, d uint64) { *c.Counter(name) += d }

// Get returns the named counter's value.
func (c *Counters) Get(name string) uint64 {
	if p, ok := c.m[name]; ok {
		return *p
	}

	return 0
}

// Names returns the counter names, sorted.
func (c *Counters) Names() []string {
	names := make([]string, 0, len(c.m))
	for k := range c.m {
		names = append(names, k)
	}

	slices.Sort(names)

	return names
}

// Merge adds o's counters into c.
func (c *Counters) Merge(o *Counters) {
	for _, k := range o.Names() {
		c.Add(k, *o.m[k])
	}
}

// String lists the counters in name order.
func (c *Counters) String() string {
	var b strings.Builder
	for _, k := range c.Names() {
		fmt.Fprintf(&b, "%s=%d\n", k, *c.m[k])
	}

	return b.String()
}
