// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"
)

func TestCompareResults(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.90, MaximumLatencyRatio: 1.0}
	baseline := phaseResult{
		RunID:  "run-1",
		Origin: originMetrics{EstimatedBytes: 300, ACR: acrPullMetrics{Successful: 100}},
		Job:    jobObservation{PodStartLatency: latencySummary{P50Seconds: 60, P95Seconds: 90}},
	}
	gantry := phaseResult{
		RunID:  "run-1",
		Origin: originMetrics{EstimatedBytes: 15, ACR: acrPullMetrics{Successful: 20}},
		Gantry: gantryMetrics{PeerFetchHits: 50},
		Job:    jobObservation{PodStartLatency: latencySummary{P50Seconds: 10, P95Seconds: 20}},
	}

	comparison := compareResults(config, baseline, gantry)
	if !comparison.Passed {
		t.Fatalf("comparison failed: %+v", comparison.Checks)
	}

	if comparison.OriginByteReduction != 0.95 || comparison.OriginRequestReduction != 0.8 {
		t.Fatalf("unexpected reductions: %+v", comparison)
	}
}

func TestCompareResultsFailsWithoutPeerActivity(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.50, MaximumLatencyRatio: 2.0}
	baseline := phaseResult{RunID: "run-1", Origin: originMetrics{EstimatedBytes: 100}, Job: jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}}}
	gantry := phaseResult{RunID: "run-1", Origin: originMetrics{EstimatedBytes: 10}, Job: jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}}}

	if comparison := compareResults(config, baseline, gantry); comparison.Passed {
		t.Fatal("comparison passed without Gantry peer activity")
	}
}

func TestCompareResultsTreatsLatencyAsInformational(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.50, MaximumLatencyRatio: 1.0}
	baseline := phaseResult{RunID: "run-1", Origin: originMetrics{EstimatedBytes: 100}, Job: jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}}}
	gantry := phaseResult{
		RunID:  "run-1",
		Origin: originMetrics{EstimatedBytes: 10},
		Gantry: gantryMetrics{PeerFetchHits: 1},
		Job:    jobObservation{PodStartLatency: latencySummary{P95Seconds: 20}},
	}

	comparison := compareResults(config, baseline, gantry)
	if !comparison.Passed {
		t.Fatalf("comparison failed on informational latency: %+v", comparison.Checks)
	}

	latency := comparison.Checks["p95_latency_ratio"]
	if latency.Passed || latency.Gating {
		t.Fatalf("latency check = %+v, want failed informational check", latency)
	}
}

func TestSubtractGantryMetricsIncludesOutcomeMaps(t *testing.T) {
	before := gantryMetrics{
		OriginFailures:    map[string]float64{"rate_limited": 2},
		PeerFetchOutcomes: map[string]float64{"hit": 10, "busy": 5},
	}
	after := gantryMetrics{
		OriginFailures:    map[string]float64{"rate_limited": 3},
		PeerFetchOutcomes: map[string]float64{"hit": 15, "busy": 9},
	}

	delta := subtractGantryMetrics(after, before)
	if delta.OriginFailures["rate_limited"] != 1 || delta.PeerFetchOutcomes["hit"] != 5 || delta.PeerFetchOutcomes["busy"] != 4 {
		t.Fatalf("delta = %+v", delta)
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

	if metrics.OriginPulls != 0 || metrics.PeerFetchHits != 0 {
		t.Fatalf("metrics = %+v, want zero values", metrics)
	}
}
