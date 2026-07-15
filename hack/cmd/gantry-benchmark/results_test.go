// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

func TestCompareResults(t *testing.T) {
	config := benchmarkConfig{MinimumByteReduction: 0.90, MaximumLatencyRatio: 1.0}
	baseline := phaseResult{
		RunID: "run-1",
		Proxy: proxyPhaseTotals{BytesUpstream: 300, RequestsCompleted: 100},
		Job:   jobObservation{PodStartLatency: latencySummary{P50Seconds: 60, P95Seconds: 90}},
	}
	gantry := phaseResult{
		RunID:  "run-1",
		Proxy:  proxyPhaseTotals{BytesUpstream: 15, RequestsCompleted: 20},
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
	baseline := phaseResult{RunID: "run-1", Proxy: proxyPhaseTotals{BytesUpstream: 100}, Job: jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}}}
	gantry := phaseResult{RunID: "run-1", Proxy: proxyPhaseTotals{BytesUpstream: 10}, Job: jobObservation{PodStartLatency: latencySummary{P95Seconds: 10}}}

	if comparison := compareResults(config, baseline, gantry); comparison.Passed {
		t.Fatal("comparison passed without Gantry peer activity")
	}
}
