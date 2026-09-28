// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type racerMetrics struct {
	requests        *prometheus.CounterVec
	bytes           *prometheus.CounterVec
	durations       *prometheus.HistogramVec
	originRequests  *prometheus.CounterVec
	originBodyBytes *prometheus.CounterVec
}

func newRacerMetrics(reg *metrics.Registry, client racerAgentClient) *racerMetrics {
	m := &racerMetrics{
		requests:        reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_mirror_requests_total", Help: "Racer mirror responses by method, HTTP status, and completion."}, []string{"method", "status", "outcome"}),
		bytes:           reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_mirror_bytes_total", Help: "Body bytes written downstream, including partial responses."}, []string{"method"}),
		durations:       reg.NewHistogramVec("racer", prometheus.HistogramOpts{Name: "gantry_racer_mirror_duration_seconds", Help: "Racer mirror handler duration including downstream writes.", Buckets: prometheus.ExponentialBuckets(0.001, 4, 11)}, []string{"method"}),
		originRequests:  reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_origin_requests_total", Help: "Upstream HTTP round trips, including HEAD, GET, and authentication; status zero denotes transport failure."}, []string{"method", "status"}),
		originBodyBytes: reg.NewCounterVec("racer", prometheus.CounterOpts{Name: "gantry_racer_origin_bytes_total", Help: "Upstream GET body bytes read, including partial transfers."}, []string{"kind"}),
	}
	if source, ok := client.(interface{ Stats() racersdk.Stats }); ok {
		// Adapt the SDK's cumulative snapshot directly, preserving counter types
		// and sampling once per scrape rather than running a telemetry goroutine.
		reg.PrometheusRegistry().MustRegister(newRacerSDKCollector(source.Stats))
	}

	return m
}

func racerMetricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return method
	default:
		return "other"
	}
}

func (m *racerMetrics) mirrorResponse(o mirror.RacerHTTPObservation) {
	method := racerMetricMethod(o.Method)

	outcome := "complete"
	if o.Aborted {
		outcome = "aborted"
	}

	m.requests.WithLabelValues(method, strconv.Itoa(o.Status), outcome).Inc()
	m.bytes.WithLabelValues(method).Add(float64(o.Bytes))
	m.durations.WithLabelValues(method).Observe(o.Duration.Seconds())
}

func (m *racerMetrics) originRequest(method string, status int) {
	m.originRequests.WithLabelValues(racerMetricMethod(method), strconv.Itoa(status)).Inc()
}

func (m *racerMetrics) originBytes(kind string, bytes int64) {
	m.originBodyBytes.WithLabelValues(kind).Add(float64(bytes))
}

type racerSDKMetric struct {
	desc  *prometheus.Desc
	kind  prometheus.ValueType
	value func(racersdk.Stats) float64
}

type racerSDKCollector struct {
	stats   func() racersdk.Stats
	metrics []racerSDKMetric
}

func newRacerSDKCollector(stats func() racersdk.Stats) *racerSDKCollector {
	c := &racerSDKCollector{stats: stats}
	add := func(name, help string, kind prometheus.ValueType, value func(racersdk.Stats) float64) {
		c.metrics = append(c.metrics, racerSDKMetric{prometheus.NewDesc("gantry_racer_sdk_"+name, help, nil, nil), kind, value})
	}
	add("queue_depth", "Calls waiting for SDK admission.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.QueueDepth) })
	add("bulk_queue_depth", "Calls waiting for SDK bulk admission.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.BulkQueueDepth) })
	add("metadata_queue_depth", "Calls waiting for SDK metadata admission.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.MetadataQueueDepth) })
	add("small_object_queue_depth", "Calls waiting for SDK small-object admission.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.SmallObjectQueueDepth) })
	add("active_bulk", "Occupied bulk admission slots.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.ActiveBulk) })
	add("active_metadata", "Occupied metadata admission slots.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.ActiveMetadata) })
	add("active_small_objects", "Occupied small-object admission slots.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.ActiveSmallObjects) })
	add("connections", "Open SDK connections across all three pools.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.Connections) })
	add("idle_connections", "Reusable SDK connections across all three pools.", prometheus.GaugeValue, func(s racersdk.Stats) float64 { return float64(s.IdleConnections) })
	add("queue_waits_total", "Calls admitted to the SDK queue.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.QueueWaits) })
	add("queue_wait_seconds_total", "Completed SDK admission wait time, including failures.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.QueueWaitNanoseconds) / 1e9 })
	add("queue_rejections_total", "Calls rejected by a full SDK queue.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.QueueRejections) })
	add("queue_timeouts_total", "SDK queue admission timeouts.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.QueueTimeouts) })
	add("dials_total", "SDK dial attempts, including failures.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.Dials) })
	add("connection_reuses_total", "SDK idle connection leases.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.ConnectionReuses) })
	add("connection_rotations_total", "SDK reusable connections retired at their jittered maximum age across all three pools, excluding failures, aborts, and stale retries.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.ConnectionRotations) })
	add("retries_total", "SDK stale pooled connection retries.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.Retries) })
	add("bytes_read_total", "SDK body bytes consumed, including partial transfers.", prometheus.CounterValue, func(s racersdk.Stats) float64 { return float64(s.BytesRead) })

	return c
}

func (c *racerSDKCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, metric := range c.metrics {
		ch <- metric.desc
	}
}

func (c *racerSDKCollector) Collect(ch chan<- prometheus.Metric) {
	snapshot := c.stats()
	for _, metric := range c.metrics {
		ch <- prometheus.MustNewConstMetric(metric.desc, metric.kind, metric.value(snapshot))
	}
}
