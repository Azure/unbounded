// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/Azure/unbounded/internal/gantry/metrics"
)

func TestByteMetricFamiliesMaterializeBoundedLabelsAtStartup(t *testing.T) {
	reg := metrics.New()
	_ = newPhase1Metrics(reg)
	_ = newPhase2Metrics(reg)
	_ = newPhase9Metrics(reg)

	families, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	seriesByName := make(map[string]int, len(families))
	for _, family := range families {
		seriesByName[family.GetName()] = len(family.Metric)
	}

	want := map[string]int{
		"gantry_origin_bytes_total":                                    3,
		"gantry_peer_fetch_bytes_total":                                3,
		"gantry_peer_serve_bytes_total":                                3,
		"gantry_mirror_bytes_served_total":                             9,
		"gantry_mirror_response_completed_timestamp_seconds":           9,
		"p2p_peer_fetch_total":                                         10,
		"gantry_peer_fetch_last_timestamp_seconds":                     2,
		"p2p_peer_fetch_duration_seconds":                              10,
		"p2p_dht_lookup_total":                                         4,
		"p2p_dht_lookup_duration_seconds":                              4,
		"gantry_containerd_commit_observed_timestamp_seconds":          1,
		"gantry_containerd_commit_observation_duration_seconds":        1,
		"gantry_containerd_commit_latest_observation_duration_seconds": 1,
	}

	for name, wantSeries := range want {
		if got := seriesByName[name]; got != wantSeries {
			t.Errorf("%s series = %d, want %d", name, got, wantSeries)
		}
	}
}
