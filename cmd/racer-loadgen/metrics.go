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
	pulls           *prometheus.CounterVec
	pullDuration    *prometheus.HistogramVec
	inFlight        prometheus.Gauge
	receivedBytes   prometheus.Counter
	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	originRequests  *prometheus.CounterVec
	originBytes     prometheus.Counter
	originDuration  prometheus.Histogram
}

func newMetrics(reg *prometheus.Registry) *loadMetrics {
	const ns = "racer_loadgen"

	buckets := []float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
	m := &loadMetrics{
		pulls:           prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "pulls_total", Help: "Completed full image pull attempts."}, []string{"result"}),
		pullDuration:    prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "pull_duration_seconds", Help: "Full image pull latency including failures.", Buckets: buckets}, []string{"result"}),
		inFlight:        prometheus.NewGauge(prometheus.GaugeOpts{Namespace: ns, Name: "in_flight", Help: "Image pulls currently in progress."}),
		receivedBytes:   prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "received_bytes_total", Help: "Response body bytes received, including failed pulls."}),
		requests:        prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "requests_total", Help: "Completed pull HTTP requests."}, []string{"kind", "result"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: ns, Name: "request_duration_seconds", Help: "Pull HTTP request latency through body consumption.", Buckets: buckets}, []string{"kind", "result"}),
		originRequests:  prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: ns, Name: "origin_requests_total", Help: "Synthetic origin HTTP requests."}, []string{"method", "code"}),
		originBytes:     prometheus.NewCounter(prometheus.CounterOpts{Namespace: ns, Name: "origin_bytes_total", Help: "Response body bytes written by the synthetic origin."}),
		originDuration:  prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: ns, Name: "origin_request_duration_seconds", Help: "Synthetic origin HTTP request latency.", Buckets: buckets}),
	}
	reg.MustRegister(m.pulls, m.pullDuration, m.inFlight, m.receivedBytes, m.requests, m.requestDuration,
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
