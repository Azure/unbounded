// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"
)

func TestCompareResults(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.90, MaximumLatencyRatio: 1.0}
	baseline := phaseResult{
		RunID:             "run-1",
		Proxy:             proxyPhaseTotals{BytesUpstream: 300, RequestsCompleted: 100},
		OriginBytes:       300,
		OriginBytesSource: originBytesProxy,
		GantryPeer:        gantryPeerPhaseMeasurement{Complete: true},
		Job:               jobObservation{PodStartLatency: latencySummary{P50Seconds: 60, P95Seconds: 90}},
	}
	gantry := phaseResult{
		RunID:             "run-1",
		Proxy:             proxyPhaseTotals{BytesUpstream: 15, RequestsCompleted: 20},
		OriginBytes:       15,
		OriginBytesSource: originBytesProxy,
		Gantry:            gantryMetrics{PeerFetchHits: 50},
		GantryPeer:        gantryPeerPhaseMeasurement{Complete: true, Total: 285},
		Job:               jobObservation{PodStartLatency: latencySummary{P50Seconds: 10, P95Seconds: 20}},
	}

	comparison := compareResults(config, baseline, gantry)
	if !comparison.Passed {
		t.Fatalf("comparison failed: %+v", comparison.Checks)
	}

	if comparison.OriginByteReduction != 0.95 || comparison.OriginRequestReduction != 0.8 {
		t.Fatalf("unexpected reductions: %+v", comparison)
	}

	if comparison.OriginRequestSource != "acr-origin-proxy" {
		t.Fatalf("request source = %q, want proxy fallback", comparison.OriginRequestSource)
	}
}

// An unrecorded origin-byte source must fail rather than read as a 0%
// reduction against an unset OriginBytes.
func TestCompareResultsFailsWithoutRecordedByteSource(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.90, MaximumLatencyRatio: 1.0}
	baseline := phaseResult{RunID: "run-1", OriginBytes: 300, Job: jobObservation{PodStartLatency: latencySummary{P95Seconds: 90}}}
	gantry := phaseResult{
		RunID:       "run-1",
		OriginBytes: 15,
		Gantry:      gantryMetrics{PeerFetchHits: 50},
		Job:         jobObservation{PodStartLatency: latencySummary{P95Seconds: 20}},
	}

	comparison := compareResults(config, baseline, gantry)
	if comparison.Checks["origin_bytes_recorded"].Passed {
		t.Fatal("expected the origin byte source check to fail")
	}

	if comparison.Passed {
		t.Fatal("comparison passed without a recorded origin byte source")
	}
}

func TestCompareResultsFailsWithoutPeerActivity(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.50, MaximumLatencyRatio: 2.0}
	baseline := phaseResult{
		RunID:             "run-1",
		Proxy:             proxyPhaseTotals{BytesUpstream: 100},
		OriginBytes:       100,
		OriginBytesSource: originBytesProxy,
		GantryPeer:        gantryPeerPhaseMeasurement{Complete: true},
		Job:               jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}},
	}
	gantry := phaseResult{
		RunID:             "run-1",
		Proxy:             proxyPhaseTotals{BytesUpstream: 10},
		OriginBytes:       10,
		OriginBytesSource: originBytesProxy,
		GantryPeer:        gantryPeerPhaseMeasurement{Complete: true},
		Job:               jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}},
	}

	comparison := compareResults(config, baseline, gantry)
	if comparison.Passed {
		t.Fatal("comparison passed without Gantry peer activity")
	}

	// Guard against passing for the wrong reason: only peer activity should be
	// failing here.
	if !comparison.Checks["origin_byte_reduction"].Passed {
		t.Fatalf("byte reduction should still pass: %+v", comparison.Checks)
	}
}

type staticPrometheusRunner struct {
	output []byte
}

func (r staticPrometheusRunner) Run(context.Context, []byte, string, ...string) ([]byte, error) {
	return r.output, nil
}

func TestFetchGantryRevisionMetricsTreatsMissingCounterSeriesAsZero(t *testing.T) {
	benchmark := &benchmark{
		config: benchmarkConfig{
			GantryNamespace:     "gantry-system",
			MonitoringNamespace: "monitoring",
			PrometheusService:   "prometheus",
		},
		commands: staticPrometheusRunner{output: []byte(`{"status":"success","data":{"result":[]}}`)},
	}

	metrics, err := benchmark.fetchGantryRevisionMetrics(context.Background(), "revision-1")
	if err != nil {
		t.Fatalf("fetchGantryRevisionMetrics: %v", err)
	}

	if metrics.OriginPulls != 0 || metrics.OriginBytes != 0 || metrics.PeerFetchHits != 0 {
		t.Fatalf("metrics = %+v, want zero values", metrics)
	}
}

func TestGantryMetricSettlementRequiresStableIdleWindow(t *testing.T) {
	start := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	tracker := gantryMetricSettlement{window: 20 * time.Second}
	metrics := gantryMetrics{OriginPulls: 2, OriginBytes: 64, PeerFetchHits: 4}

	if tracker.Observe(start, metrics, 1) {
		t.Fatal("settled while a pull was in flight")
	}

	if tracker.Observe(start.Add(5*time.Second), metrics, 0) {
		t.Fatal("settled on the first idle observation")
	}

	if tracker.Observe(start.Add(24*time.Second), metrics, 0) {
		t.Fatal("settled before the full stable window elapsed")
	}

	if !tracker.Observe(start.Add(25*time.Second), metrics, 0) {
		t.Fatal("did not settle after the full stable idle window")
	}
}

func TestGantryMetricSettlementResetsWhenCountersMove(t *testing.T) {
	start := time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)
	tracker := gantryMetricSettlement{window: 20 * time.Second}
	first := gantryMetrics{OriginPulls: 2, OriginBytes: 64, PeerFetchHits: 4}
	second := gantryMetrics{OriginPulls: 3, OriginBytes: 96, PeerFetchHits: 8}

	if tracker.Observe(start, first, 0) {
		t.Fatal("settled on the first observation")
	}

	if tracker.Observe(start.Add(20*time.Second), second, 0) {
		t.Fatal("settled when counters moved at the edge of the window")
	}

	if tracker.Observe(start.Add(39*time.Second), second, 0) {
		t.Fatal("settled before the reset window elapsed")
	}

	if !tracker.Observe(start.Add(40*time.Second), second, 0) {
		t.Fatal("did not settle after counters stayed stable for the reset window")
	}
}

func TestGantryMetricSettlementAllowsStableBaselineWithoutPeers(t *testing.T) {
	start := time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC)
	tracker := gantryMetricSettlement{window: 20 * time.Second, requirePeerActivity: false}
	metrics := gantryMetrics{}

	if tracker.Observe(start, metrics, 0) {
		t.Fatal("settled on first baseline observation")
	}

	if !tracker.Observe(start.Add(20*time.Second), metrics, 0) {
		t.Fatal("baseline did not settle with zero peer activity")
	}
}
