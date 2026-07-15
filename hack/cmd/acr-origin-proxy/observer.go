// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const maxByDigestEntries = 1024

type trafficTotals struct {
	Requests      uint64 `json:"requests"`
	BytesUpstream uint64 `json:"bytes_upstream"`
	BytesToClient uint64 `json:"bytes_to_client"`
}

type digestEntry struct {
	Phase         benchmarkPhase                `json:"phase"`
	Digest        string                        `json:"digest"`
	PathClass     pathClass                     `json:"path_class"`
	TrafficTotals trafficTotals                 `json:"totals"`
	ByClientClass map[clientClass]trafficTotals `json:"by_client_class"`
}

type phaseTotals struct {
	RequestsCompleted uint64                        `json:"requests_completed"`
	BytesUpstream     uint64                        `json:"bytes_upstream"`
	BytesToClient     uint64                        `json:"bytes_to_client"`
	ByPathClass       map[pathClass]trafficTotals   `json:"by_path_class"`
	ByClientClass     map[clientClass]trafficTotals `json:"by_client_class"`
}

type totals struct {
	RequestsCompleted uint64                         `json:"requests_completed"`
	BytesUpstream     uint64                         `json:"bytes_upstream"`
	BytesToClient     uint64                         `json:"bytes_to_client"`
	ByPathClass       map[pathClass]trafficTotals    `json:"by_path_class"`
	ByClientClass     map[clientClass]trafficTotals  `json:"by_client_class"`
	ByPhase           map[benchmarkPhase]phaseTotals `json:"by_phase"`
	ByDigest          []digestEntry                  `json:"by_digest"`
}

type summary struct {
	RunID      string         `json:"run_id"`
	Phase      benchmarkPhase `json:"phase"`
	Since      string         `json:"since"`
	UptimeSecs int64          `json:"uptime_seconds"`
	Totals     totals         `json:"totals"`
}

type observer struct {
	started           *prometheus.CounterVec
	completed         *prometheus.CounterVec
	bytesUpstream     *prometheus.CounterVec
	bytesToClient     *prometheus.CounterVec
	latency           *prometheus.HistogramVec
	inflight          *prometheus.GaugeVec
	authRefresh       *prometheus.CounterVec
	syntheticThrottle *prometheus.CounterVec
	controller        *phaseController
	startedAt         time.Time
	mu                sync.Mutex
	totals            totals
	inflightByClass   map[pathClass]int
	byDigest          map[string]*digestEntry
}

func newObserver(registry *prometheus.Registry, now time.Time, controller *phaseController) *observer {
	requestLabels := []string{"method", "path_class", "client_class", "run_id", "phase"}
	responseLabels := []string{"method", "path_class", "client_class", "status", "run_id", "phase"}
	trafficLabels := []string{"path_class", "client_class", "status", "run_id", "phase"}

	result := &observer{
		started: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "origin_requests_started_total",
			Help: "Logical client requests started by the benchmark ACR origin proxy.",
		}, requestLabels),
		completed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "origin_requests_completed_total",
			Help: "Logical client requests completed by the benchmark ACR origin proxy.",
		}, responseLabels),
		bytesUpstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "origin_bytes_upstream_total",
			Help: "Response body bytes read from the upstream registry.",
		}, trafficLabels),
		bytesToClient: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "origin_bytes_to_client_total",
			Help: "Response body bytes written to proxy clients.",
		}, trafficLabels),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "origin_latency_seconds",
			Help:    "Logical request latency through the benchmark ACR origin proxy.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"path_class", "client_class", "status", "run_id", "phase"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "origin_inflight_requests",
			Help: "Logical requests currently in flight through the benchmark ACR origin proxy.",
		}, []string{"path_class", "run_id", "phase"}),
		authRefresh: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "origin_auth_token_refresh_total",
			Help: "Bearer token refresh attempts by result.",
		}, []string{"result", "run_id", "phase"}),
		syntheticThrottle: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "origin_synthetic_throttle_total",
			Help: "Synthetic proxy throttles by reason.",
		}, []string{"reason", "run_id", "phase"}),
		controller:      controller,
		startedAt:       now,
		inflightByClass: make(map[pathClass]int),
		byDigest:        make(map[string]*digestEntry),
		totals: totals{
			ByPathClass:   newPathTotals(),
			ByClientClass: newClientTotals(),
			ByPhase:       make(map[benchmarkPhase]phaseTotals),
		},
	}

	registry.MustRegister(
		result.started,
		result.completed,
		result.bytesUpstream,
		result.bytesToClient,
		result.latency,
		result.inflight,
		result.authRefresh,
		result.syntheticThrottle,
	)

	return result
}

func newPathTotals() map[pathClass]trafficTotals {
	result := make(map[pathClass]trafficTotals, len(allPathClasses))
	for _, class := range allPathClasses {
		result[class] = trafficTotals{}
	}

	return result
}

func newClientTotals() map[clientClass]trafficTotals {
	result := make(map[clientClass]trafficTotals, len(allClientClasses))
	for _, class := range allClientClasses {
		result[class] = trafficTotals{}
	}

	return result
}

func (o *observer) begin(method string, path pathClass, client clientClass) phaseSnapshot {
	attribution := o.controller.begin()
	o.started.WithLabelValues(method, string(path), string(client), attribution.RunID, string(attribution.Phase)).Inc()
	o.inflight.WithLabelValues(string(path), attribution.RunID, string(attribution.Phase)).Inc()

	o.mu.Lock()
	o.inflightByClass[path]++
	o.mu.Unlock()

	return attribution
}

func (o *observer) finish(
	attribution phaseSnapshot,
	method string,
	path pathClass,
	client clientClass,
	digest string,
	status string,
	upstreamBytes int64,
	clientBytes int64,
	elapsed time.Duration,
) {
	defer o.controller.finish(attribution)

	if upstreamBytes < 0 {
		upstreamBytes = 0
	}

	if clientBytes < 0 {
		clientBytes = 0
	}

	o.completed.WithLabelValues(method, string(path), string(client), status, attribution.RunID, string(attribution.Phase)).Inc()
	o.bytesUpstream.WithLabelValues(string(path), string(client), status, attribution.RunID, string(attribution.Phase)).Add(float64(upstreamBytes))
	o.bytesToClient.WithLabelValues(string(path), string(client), status, attribution.RunID, string(attribution.Phase)).Add(float64(clientBytes))
	o.latency.WithLabelValues(string(path), string(client), status, attribution.RunID, string(attribution.Phase)).Observe(elapsed.Seconds())
	o.inflight.WithLabelValues(string(path), attribution.RunID, string(attribution.Phase)).Dec()

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.inflightByClass[path] > 0 {
		o.inflightByClass[path]--
	}

	traffic := trafficTotals{Requests: 1, BytesUpstream: uint64(upstreamBytes), BytesToClient: uint64(clientBytes)}
	o.totals.RequestsCompleted++
	o.totals.BytesUpstream += traffic.BytesUpstream
	o.totals.BytesToClient += traffic.BytesToClient
	addTraffic(o.totals.ByPathClass, path, traffic)
	addTraffic(o.totals.ByClientClass, client, traffic)

	phase := o.totals.ByPhase[attribution.Phase]
	if phase.ByPathClass == nil {
		phase.ByPathClass = newPathTotals()
		phase.ByClientClass = newClientTotals()
	}

	phase.RequestsCompleted++
	phase.BytesUpstream += traffic.BytesUpstream
	phase.BytesToClient += traffic.BytesToClient
	addTraffic(phase.ByPathClass, path, traffic)
	addTraffic(phase.ByClientClass, client, traffic)
	o.totals.ByPhase[attribution.Phase] = phase

	if digest == "" || (path != pathClassBlob && path != pathClassManifestByDigest) {
		return
	}

	key := string(attribution.Phase) + "\x00" + string(path) + "\x00" + digest

	entry, ok := o.byDigest[key]
	if !ok {
		if len(o.byDigest) >= maxByDigestEntries {
			return
		}

		entry = &digestEntry{
			Phase:         attribution.Phase,
			Digest:        digest,
			PathClass:     path,
			ByClientClass: newClientTotals(),
		}
		o.byDigest[key] = entry
	}

	entry.TrafficTotals = sumTraffic(entry.TrafficTotals, traffic)
	addTraffic(entry.ByClientClass, client, traffic)
}

func addTraffic[K comparable](values map[K]trafficTotals, key K, delta trafficTotals) {
	values[key] = sumTraffic(values[key], delta)
}

func sumTraffic(current, delta trafficTotals) trafficTotals {
	return trafficTotals{
		Requests:      current.Requests + delta.Requests,
		BytesUpstream: current.BytesUpstream + delta.BytesUpstream,
		BytesToClient: current.BytesToClient + delta.BytesToClient,
	}
}

func (o *observer) currentInflight(class pathClass) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.inflightByClass[class]
}

func (o *observer) recordAuthRefresh(attribution phaseSnapshot, result string) {
	o.authRefresh.WithLabelValues(result, attribution.RunID, string(attribution.Phase)).Inc()
}

func (o *observer) recordSyntheticThrottle(attribution phaseSnapshot, reason string) {
	o.syntheticThrottle.WithLabelValues(reason, attribution.RunID, string(attribution.Phase)).Inc()
}

func (o *observer) snapshot(now time.Time) summary {
	o.mu.Lock()
	defer o.mu.Unlock()

	result := summary{
		RunID:      o.controller.snapshot().RunID,
		Phase:      o.controller.snapshot().Phase,
		Since:      o.startedAt.UTC().Format(time.RFC3339),
		UptimeSecs: int64(now.Sub(o.startedAt).Seconds()),
		Totals: totals{
			RequestsCompleted: o.totals.RequestsCompleted,
			BytesUpstream:     o.totals.BytesUpstream,
			BytesToClient:     o.totals.BytesToClient,
			ByPathClass:       cloneTotals(o.totals.ByPathClass),
			ByClientClass:     cloneTotals(o.totals.ByClientClass),
			ByPhase:           make(map[benchmarkPhase]phaseTotals, len(o.totals.ByPhase)),
			ByDigest:          make([]digestEntry, 0, len(o.byDigest)),
		},
	}
	for phase, value := range o.totals.ByPhase {
		value.ByPathClass = cloneTotals(value.ByPathClass)
		value.ByClientClass = cloneTotals(value.ByClientClass)
		result.Totals.ByPhase[phase] = value
	}

	for _, entry := range o.byDigest {
		copy := *entry
		copy.ByClientClass = cloneTotals(entry.ByClientClass)
		result.Totals.ByDigest = append(result.Totals.ByDigest, copy)
	}

	sort.Slice(result.Totals.ByDigest, func(i, j int) bool {
		left := result.Totals.ByDigest[i]

		right := result.Totals.ByDigest[j]
		if left.Phase != right.Phase {
			return left.Phase < right.Phase
		}

		if left.TrafficTotals.BytesUpstream != right.TrafficTotals.BytesUpstream {
			return left.TrafficTotals.BytesUpstream > right.TrafficTotals.BytesUpstream
		}

		return left.Digest < right.Digest
	})

	return result
}

func cloneTotals[K comparable](values map[K]trafficTotals) map[K]trafficTotals {
	result := make(map[K]trafficTotals, len(values))
	for key, value := range values {
		result[key] = value
	}

	return result
}

func summaryHandler(observer *observer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(observer.snapshot(time.Now())); err != nil {
			slog.Error("encode proxy summary", "error", err)
		}
	})
}
