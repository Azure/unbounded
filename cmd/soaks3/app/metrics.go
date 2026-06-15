// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics holds the Prometheus collectors plus an in-process latency sketch
// used for the periodic and final stdout summaries.
type metrics struct {
	registry *prometheus.Registry

	requests *prometheus.CounterVec
	bytes    prometheus.Counter
	errors   prometheus.Counter
	latency  *prometheus.HistogramVec
	inflight prometheus.Gauge

	// Aggregate counters for the stdout reporter, kept independently so the
	// summary does not depend on scraping Prometheus.
	reqTotal  atomic.Int64
	errTotal  atomic.Int64
	byteTotal atomic.Int64

	sketch latencySketch
}

// newMetrics constructs the collectors and registers them on a private
// registry.
func newMetrics() *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "soaks3_requests_total",
			Help: "Total S3 requests by operation and HTTP status code.",
		}, []string{"op", "status"}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soaks3_response_bytes_total",
			Help: "Total response body bytes read.",
		}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "soaks3_errors_total",
			Help: "Total request errors (transport failures or unexpected status).",
		}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "soaks3_request_duration_seconds",
			Help:    "Request latency by operation and HTTP status code.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 16),
		}, []string{"op", "status"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "soaks3_inflight_requests",
			Help: "In-flight S3 requests.",
		}),
	}

	m.registry.MustRegister(m.requests, m.bytes, m.errors, m.latency, m.inflight)

	return m
}

// observe records the outcome of a single request.
func (m *metrics) observe(op, status string, d time.Duration, n int64, failed bool) {
	m.requests.WithLabelValues(op, status).Inc()
	m.latency.WithLabelValues(op, status).Observe(d.Seconds())
	m.reqTotal.Add(1)
	m.sketch.record(d)

	if n > 0 {
		m.bytes.Add(float64(n))
		m.byteTotal.Add(n)
	}

	if failed {
		m.errors.Inc()
		m.errTotal.Add(1)
	}
}

// serve starts an HTTP server exposing the Prometheus registry at /metrics on
// addr. It returns the server so the caller can shut it down. A nil server is
// returned when addr is empty.
func (m *metrics) serve(addr string) *http.Server {
	if addr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("soaks3: metrics server error: %v\n", err)
		}
	}()

	return srv
}

// snapshot captures cumulative counters at a point in time, including the
// cumulative latency histogram so callers can derive interval-scoped
// percentiles by differencing two snapshots.
type snapshot struct {
	requests int64
	errors   int64
	bytes    int64
	at       time.Time
	latency  latencyBuckets
}

// snapshot returns the current cumulative totals.
func (m *metrics) snapshot() snapshot {
	s := snapshot{
		requests: m.reqTotal.Load(),
		errors:   m.errTotal.Load(),
		bytes:    m.byteTotal.Load(),
		at:       time.Now(),
	}

	m.sketch.readInto(&s.latency)

	return s
}

// report prints an interval line describing the delta between prev and the
// current totals, and returns the new snapshot.
func (m *metrics) report(prev snapshot) snapshot {
	cur := m.snapshot()

	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return cur
	}

	dReq := cur.requests - prev.requests
	dErr := cur.errors - prev.errors
	dBytes := cur.bytes - prev.bytes

	rps := float64(dReq) / elapsed
	throughput := float64(dBytes) / elapsed

	// Percentiles over just this interval's observations, derived from the
	// difference of the cumulative histograms, so the reported latency
	// tracks recent behavior rather than the whole-run aggregate.
	lat := cur.latency.sub(&prev.latency)

	fmt.Printf("[soaks3] %6.0f req/s  %10s/s  errors=%d  p50=%s p95=%s p99=%s\n",
		rps,
		humanize.IBytes(uint64(throughput)),
		dErr,
		lat.quantile(0.50),
		lat.quantile(0.95),
		lat.quantile(0.99),
	)

	return cur
}

// summary prints the final aggregate summary over the full run duration.
func (m *metrics) summary(start time.Time) {
	cur := m.snapshot()

	elapsed := cur.at.Sub(start).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}

	rps := float64(cur.requests) / elapsed
	throughput := float64(cur.bytes) / elapsed

	errRate := 0.0
	if cur.requests > 0 {
		errRate = float64(cur.errors) / float64(cur.requests) * 100
	}

	fmt.Println("[soaks3] ---- summary ----")
	fmt.Printf("[soaks3] duration:   %s\n", time.Duration(elapsed*float64(time.Second)).Round(time.Millisecond))
	fmt.Printf("[soaks3] requests:   %d (%.0f req/s)\n", cur.requests, rps)
	fmt.Printf("[soaks3] data:       %s (%s/s)\n", humanize.IBytes(uint64(cur.bytes)), humanize.IBytes(uint64(throughput)))
	fmt.Printf("[soaks3] errors:     %d (%.2f%%)\n", cur.errors, errRate)
	fmt.Printf("[soaks3] latency:    p50=%s p95=%s p99=%s\n",
		m.sketch.quantile(0.50), m.sketch.quantile(0.95), m.sketch.quantile(0.99))
}

// runReporter prints an interval summary every interval until ctx is done.
func (m *metrics) runReporter(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prev := m.snapshot()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prev = m.report(prev)
		}
	}
}

// sketchBucketCount bounds the latency sketch. With subBits=6 it covers well
// beyond any plausible request latency.
const (
	subBits           = 6
	subCount          = 1 << subBits
	sketchBucketCount = 4096
)

// latencySketch is a log-linear histogram of latencies in microseconds. It
// yields percentiles with bounded relative error (~1.5%) and bounded memory,
// and is safe for concurrent use.
type latencySketch struct {
	mu      sync.Mutex
	buckets [sketchBucketCount]uint64
	count   uint64
}

// record adds a latency observation.
func (s *latencySketch) record(d time.Duration) {
	us := d.Microseconds()
	if us < 0 {
		us = 0
	}

	idx := bucketIndex(uint64(us))

	s.mu.Lock()
	s.buckets[idx]++
	s.count++
	s.mu.Unlock()
}

// readInto copies the current cumulative histogram into dst under the lock.
func (s *latencySketch) readInto(dst *latencyBuckets) {
	s.mu.Lock()
	dst.buckets = s.buckets
	dst.count = s.count
	s.mu.Unlock()
}

// quantile returns the approximate q-quantile (0..1) of all observations so
// far as a Duration.
func (s *latencySketch) quantile(q float64) time.Duration {
	var b latencyBuckets

	s.readInto(&b)

	return b.quantile(q)
}

// latencyBuckets is a point-in-time copy of a latency histogram. Differencing
// two copies yields the observations recorded in the interval between them,
// which is how the periodic reporter computes interval-scoped percentiles.
type latencyBuckets struct {
	buckets [sketchBucketCount]uint64
	count   uint64
}

// sub returns the histogram of observations recorded between prev and b.
func (b *latencyBuckets) sub(prev *latencyBuckets) latencyBuckets {
	var d latencyBuckets

	for i := range b.buckets {
		d.buckets[i] = b.buckets[i] - prev.buckets[i]
	}

	d.count = b.count - prev.count

	return d
}

// quantile returns the approximate q-quantile (0..1) of the observations in b
// as a Duration.
func (b *latencyBuckets) quantile(q float64) time.Duration {
	if b.count == 0 {
		return 0
	}

	if q < 0 {
		q = 0
	}

	if q > 1 {
		q = 1
	}

	target := uint64(q * float64(b.count))
	if target == 0 {
		target = 1
	}

	var cum uint64

	for idx, c := range b.buckets {
		cum += c
		if cum >= target {
			return time.Duration(bucketValue(idx)) * time.Microsecond
		}
	}

	return time.Duration(bucketValue(len(b.buckets)-1)) * time.Microsecond
}

// bucketIndex maps a microsecond value to its log-linear bucket.
func bucketIndex(v uint64) int {
	if v < subCount {
		return int(v)
	}

	e := bits.Len64(v) - 1
	shift := uint(e - subBits)
	mantissa := (v >> shift) - subCount
	idx := subCount + (e-subBits)*subCount + int(mantissa)

	if idx >= sketchBucketCount {
		return sketchBucketCount - 1
	}

	return idx
}

// bucketValue returns the lower-bound microsecond value represented by a
// bucket index (the inverse of bucketIndex).
func bucketValue(idx int) uint64 {
	if idx < subCount {
		return uint64(idx)
	}

	rel := idx - subCount
	octave := rel / subCount
	mantissa := uint64(rel % subCount)
	e := subBits + octave

	return (subCount + mantissa) << uint(e-subBits)
}
