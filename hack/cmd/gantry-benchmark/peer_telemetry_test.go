// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type diagnosticTimestampRunner struct {
	queryPath string
}

func (r *diagnosticTimestampRunner) Run(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
	r.queryPath = args[2]

	return []byte(fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"__name__":"gantry_mirror_response_completed_timestamp_seconds","pod":"gantry-a","kind":"layer","source":"peer"},"values":[[0,"%d"],[0,"%d"]]},
		{"metric":{"__name__":"gantry_containerd_commit_observed_timestamp_seconds","pod":"gantry-a"},"values":[[0,"%d"]]},
		{"metric":{"__name__":"gantry_containerd_commit_observed_timestamp_seconds","pod":"gantry-b"},"values":[[0,"%d"]]}
	]}}`,
		time.Date(2026, time.August, 4, 1, 2, 30, 0, time.UTC).Unix(),
		time.Date(2026, time.August, 4, 1, 3, 0, 0, time.UTC).Unix(),
		time.Date(2026, time.August, 4, 1, 4, 0, 0, time.UTC).Unix(),
		time.Date(2026, time.August, 4, 1, 5, 1, 0, time.UTC).Unix(),
	)), nil
}

func TestFetchGantryDiagnosticTimestampsUsesExactJobWindow(t *testing.T) {
	runner := &diagnosticTimestampRunner{}
	benchmark := &benchmark{
		config: benchmarkConfig{
			GantryNamespace:     "gantry-system",
			MonitoringNamespace: "monitoring",
			PrometheusService:   "prometheus",
			NodeCount:           2,
		},
		commands: runner,
	}
	window := telemetryWindow{
		StartedAt:  time.Date(2026, time.August, 4, 1, 2, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, time.August, 4, 1, 5, 0, 0, time.UTC),
	}

	timestamps, err := benchmark.fetchGantryDiagnosticTimestamps(context.Background(), "revision-a", window)
	if err != nil {
		t.Fatalf("fetchGantryDiagnosticTimestamps: %v", err)
	}
	if len(timestamps["gantry-a"]) != 2 || timestamps["gantry-b"] != nil {
		t.Fatalf("timestamps = %v, want only two in-window gantry-a values", timestamps)
	}
	if timestamps["gantry-a"]["gantry_mirror_response_completed_timestamp_seconds{kind=layer,source=peer}"] !=
		float64(time.Date(2026, time.August, 4, 1, 3, 0, 0, time.UTC).Unix()) {
		t.Fatalf("timestamps = %v, want latest in-window layer completion", timestamps)
	}
	if !strings.Contains(runner.queryPath, `kind%3D%22layer%22`) {
		t.Fatalf("query path %q does not restrict completion timestamps to layers", runner.queryPath)
	}
}

func TestRequireFinalLayerResponseTimestamps(t *testing.T) {
	timestamps := map[string]map[string]float64{
		"gantry-a": {
			"gantry_mirror_response_completed_timestamp_seconds{kind=layer,source=peer}": 1234,
		},
	}
	podNodes := map[string]string{"gantry-a": "node-a", "gantry-b": "node-b"}

	err := requireFinalLayerResponseTimestamps(timestamps, podNodes)
	if err == nil || !strings.Contains(err.Error(), "gantry-b") {
		t.Fatalf("error = %v, want missing gantry-b completion", err)
	}

	timestamps["gantry-b"] = map[string]float64{
		"gantry_mirror_response_completed_timestamp_seconds{kind=layer,source=origin}": 1235,
	}
	if err := requireFinalLayerResponseTimestamps(timestamps, podNodes); err != nil {
		t.Fatalf("requireFinalLayerResponseTimestamps: %v", err)
	}
}

func TestSubtractPeerByteSnapshots(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a", "gantry-b": "node-b"},
		Counters: map[string]map[string]uint64{
			"gantry-a": {"manifest": 1, "config": 2, "layer": 3},
			"gantry-b": {"manifest": 4, "config": 5, "layer": 6},
		},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a", "gantry-b": "node-b"},
		Counters: map[string]map[string]uint64{
			"gantry-a": {"manifest": 11, "config": 22, "layer": 33},
			"gantry-b": {"manifest": 44, "config": 55, "layer": 66},
		},
	}

	measurement, err := subtractPeerByteSnapshots(before, after)
	if err != nil {
		t.Fatalf("subtractPeerByteSnapshots: %v", err)
	}

	if !measurement.Complete || measurement.Total != 210 || len(measurement.Pods) != 2 {
		t.Fatalf("measurement = %+v, want two pods and 210 bytes", measurement)
	}

	if measurement.Pods[0].PodName != "gantry-a" || measurement.Pods[0].Total != 60 {
		t.Fatalf("first pod = %+v, want gantry-a with 60 bytes", measurement.Pods[0])
	}
}

func TestSubtractPeerByteSnapshotsRejectsRestart(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-old": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-old": {"manifest": 0, "config": 0, "layer": 10}},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-new": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-new": {"manifest": 0, "config": 0, "layer": 0}},
	}

	if _, err := subtractPeerByteSnapshots(before, after); err == nil {
		t.Fatal("expected a pod restart to invalidate the peer-byte delta")
	}
}

func TestSubtractPeerByteSnapshotsRejectsCounterReset(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"manifest": 0, "config": 0, "layer": 10}},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"manifest": 0, "config": 0, "layer": 9}},
	}

	if _, err := subtractPeerByteSnapshots(before, after); err == nil {
		t.Fatal("expected a counter reset to invalidate the peer-byte delta")
	}
}

func TestSubtractPeerByteSnapshotsDefaultsMissingKindsToZero(t *testing.T) {
	before := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"layer": 10}},
	}
	after := peerByteSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]uint64{"gantry-a": {"layer": 20}},
	}

	measurement, err := subtractPeerByteSnapshots(before, after)
	if err != nil {
		t.Fatalf("subtractPeerByteSnapshots: %v", err)
	}

	if measurement.Total != 10 || measurement.Pods[0].ByKind["manifest"] != 0 || measurement.Pods[0].ByKind["config"] != 0 {
		t.Fatalf("measurement = %+v, want missing kinds treated as zero", measurement)
	}
}

func TestDiagnosticMetricKey(t *testing.T) {
	key, err := diagnosticMetricKey(map[string]string{
		"__name__": "p2p_peer_fetch_total",
		"outcome":  "busy",
	})
	if err != nil {
		t.Fatalf("diagnosticMetricKey: %v", err)
	}
	if key != "p2p_peer_fetch_total{outcome=busy}" {
		t.Fatalf("key = %q, want p2p_peer_fetch_total{outcome=busy}", key)
	}
}

func TestSubtractGantryDiagnosticSnapshots(t *testing.T) {
	before := gantryDiagnosticSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]float64{
			"gantry-a": {"p2p_peer_fetch_total{outcome=busy}": 10},
		},
	}
	after := gantryDiagnosticSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]float64{
			"gantry-a": {
				"p2p_peer_fetch_total{outcome=busy}":               13,
				"p2p_dht_lookup_duration_seconds_sum{outcome=hit}": 1.25,
			},
		},
	}
	timestamps := map[string]map[string]float64{
		"gantry-a": {"gantry_mirror_response_completed_timestamp_seconds{kind=layer,source=peer}": 1234},
	}

	measurement, err := subtractGantryDiagnosticSnapshots(before, after, timestamps)
	if err != nil {
		t.Fatalf("subtractGantryDiagnosticSnapshots: %v", err)
	}
	if !measurement.Complete || len(measurement.Pods) != 1 {
		t.Fatalf("measurement = %+v, want one complete pod", measurement)
	}
	pod := measurement.Pods[0]
	if pod.NodeName != "node-a" || pod.CounterDeltas["p2p_peer_fetch_total{outcome=busy}"] != 3 ||
		pod.TimestampSeconds["gantry_mirror_response_completed_timestamp_seconds{kind=layer,source=peer}"] != 1234 ||
		pod.FinalLayerResponseCompletedTimestampSeconds != 1234 {
		t.Fatalf("pod = %+v, want correlated deltas and timestamp", pod)
	}
}

func TestSubtractGantryDiagnosticSnapshotsRejectsReset(t *testing.T) {
	before := gantryDiagnosticSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]float64{"gantry-a": {"counter": 2}},
	}
	after := gantryDiagnosticSnapshot{
		PodNodes: map[string]string{"gantry-a": "node-a"},
		Counters: map[string]map[string]float64{"gantry-a": {"counter": 1}},
	}

	if _, err := subtractGantryDiagnosticSnapshots(before, after, nil); err == nil {
		t.Fatal("expected a counter reset to invalidate diagnostic deltas")
	}
}
