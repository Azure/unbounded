// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"github.com/Azure/unbounded/internal/gantry/metrics"
)

func TestByteMetricFamiliesMaterializeBoundedLabelsAtStartup(t *testing.T) {
	reg := metrics.New()
	_ = newPhase1Metrics(reg)
	_ = newPhase2Metrics(reg)

	families, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	seriesByName := make(map[string]int, len(families))
	for _, family := range families {
		seriesByName[family.GetName()] = len(family.Metric)
	}

	want := map[string]int{
		"gantry_origin_bytes_total":        3,
		"gantry_peer_fetch_bytes_total":    3,
		"gantry_peer_serve_bytes_total":    3,
		"gantry_mirror_bytes_served_total": 9,
	}

	for name, wantSeries := range want {
		if got := seriesByName[name]; got != wantSeries {
			t.Errorf("%s series = %d, want %d", name, got, wantSeries)
		}
	}
}
