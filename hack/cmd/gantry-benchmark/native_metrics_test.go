// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"
	"time"
)

func TestParseACRPullMetrics(t *testing.T) {
	start := time.Date(2026, 7, 21, 1, 22, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	raw := []byte(`{
		"value": [
			{"name":{"value":"TotalPullCount"},"timeseries":[{"data":[{"total":1000},{"total":5}]}]},
			{"name":{"value":"SuccessfulPullCount"},"timeseries":[{"data":[{"total":995},{"total":3}]}]}
		]
	}`)

	metrics, err := parseACRPullMetrics(raw, start, end)
	if err != nil {
		t.Fatalf("parseACRPullMetrics: %v", err)
	}

	if metrics.Total != 1005 || metrics.Successful != 998 || metrics.Failed != 7 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

func TestSubtractKubeletPullMetrics(t *testing.T) {
	before := kubeletPullMetrics{Operations: 100, Errors: 10, DurationSamples: 90, DurationSeconds: 450}
	after := kubeletPullMetrics{Operations: 140, Errors: 12, DurationSamples: 130, DurationSeconds: 690}

	delta := subtractKubeletPullMetrics(after, before)
	if delta.Operations != 40 || delta.Errors != 2 || delta.DurationSamples != 40 || delta.AverageDurationSeconds != 6 {
		t.Fatalf("delta = %+v", delta)
	}
}

func TestEstimatedOriginBytes(t *testing.T) {
	if got := estimatedBaselineBytes(1000, 1024); got != 1000*1024*mebibyte {
		t.Fatalf("baseline bytes = %d", got)
	}

	if got := estimatedGantryOriginBytes(3, 1024, 1); got != 3*1024*mebibyte {
		t.Fatalf("Gantry bytes = %d", got)
	}
}

func TestACRRegistryName(t *testing.T) {
	for _, input := range []string{"bench.azurecr.io", "https://bench.azurecr.io"} {
		name, err := acrRegistryName(input)
		if err != nil {
			t.Fatalf("acrRegistryName(%q): %v", input, err)
		}

		if name != "bench" {
			t.Fatalf("acrRegistryName(%q) = %q", input, name)
		}
	}
}
