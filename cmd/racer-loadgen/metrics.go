// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type loadMetrics struct {
	pulls              *prometheus.CounterVec
	pullFailures       *prometheus.CounterVec
	pullDuration       *prometheus.HistogramVec
	inFlight           prometheus.Gauge
	appliedConcurrency prometheus.Gauge
	receivedBytes      prometheus.Counter
	verifiedBytes      prometheus.Counter
	requests           *prometheus.CounterVec
	requestDuration    *prometheus.HistogramVec
	originRequests     *prometheus.CounterVec
	originBytes        prometheus.Counter
	originDuration     prometheus.Histogram
}

func newMetrics(reg *prometheus.Registry) *loadMetrics {
	const ns = "racer_loadgen"

	buckets := []float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
	m := &loadMetrics{
		pulls:              prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "pulls_total", Help: "Completed blob-batch attempts: one OCI image or one generic blob per operation."}, []string{"result"}),
		pullFailures:       prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "pull_failures_total", Help: "Failed blob-batch operations by bounded reason, including cancellation; one reason per operation."}, []string{"reason"}),
		pullDuration:       prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "pull_duration_seconds", Help: "Complete blob-batch latency including failures.", Buckets: buckets}, []string{"result"}),
		inFlight:           prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "in_flight", Help: "Blob-batch operations currently in progress."}),
		appliedConcurrency: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "applied_concurrency", Help: "Applied blob-batch worker limit; in-flight operations may exceed it while draining after a decrease."}),
		receivedBytes:      prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "received_bytes_total", Help: "Body bytes delivered to the loadgen consumer, including failed operations; not wire traffic. SDK reads may discard incomplete failed pages before delivery."}),
		verifiedBytes:      prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "verified_bytes_total", Help: "Bytes in successful fully SHA-256-verified blob batches, including manifest and config for OCI workloads."}),
		requests:           prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "requests_total", Help: "Completed blob acquisitions via HTTP or SDK Get (not SDK page requests)."}, []string{"kind", "result"}),
		requestDuration:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "request_duration_seconds", Help: "Blob acquisition latency through body consumption.", Buckets: buckets}, []string{"kind", "result"}),
		originRequests:     prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "origin_requests_total", Help: "Synthetic HTTP requests or SDK origin callbacks; SDK callback failures use code=error."}, []string{"method", "code"}),
		originBytes:        prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "origin_bytes_total", Help: "Bytes written to HTTP or read by SDK from the synthetic origin; not cache or network delivery accounting."}),
		originDuration:     prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "origin_request_duration_seconds", Help: "Synthetic HTTP request or SDK callback/body lifetime.", Buckets: buckets}),
	}
	reg.MustRegister(m.pulls, m.pullFailures, m.pullDuration, m.inFlight, m.appliedConcurrency, m.receivedBytes, m.verifiedBytes, m.requests, m.requestDuration,
		m.originRequests, m.originBytes, m.originDuration, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return m
}

func (m *loadMetrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		response := &originResponse{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(response, r)

		method := r.Method
		if method != http.MethodGet && method != http.MethodHead {
			method = "other"
		}

		m.originRequests.WithLabelValues(method, strconv.Itoa(response.code)).Inc()
		m.originBytes.Add(float64(response.bytes))
		m.originDuration.Observe(time.Since(start).Seconds())
	})
}

type originResponse struct {
	http.ResponseWriter
	code        int
	bytes       int64
	wroteHeader bool
}

func (w *originResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *originResponse) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}

	w.code, w.wroteHeader = code, true
	w.ResponseWriter.WriteHeader(code)
}

func (w *originResponse) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)

	return n, err
}
