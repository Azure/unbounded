// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type performanceTelemetryRunner struct {
	commands [][]string
}

func (r *performanceTelemetryRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	command := append([]string{name}, args...)
	r.commands = append(r.commands, command)

	return []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil
}

func TestQueryPrometheusRange(t *testing.T) {
	runner := &performanceTelemetryRunner{}
	benchmark := &benchmark{
		config: benchmarkConfig{
			MonitoringNamespace: "monitoring",
			PrometheusService:   "prometheus",
		},
		commands: runner,
	}
	window := telemetryWindow{
		StartedAt:  time.Date(2026, time.August, 4, 1, 2, 3, 0, time.UTC),
		FinishedAt: time.Date(2026, time.August, 4, 1, 12, 3, 0, time.UTC),
	}

	response, err := benchmark.queryPrometheusRange(
		context.Background(),
		`rate(node_disk_written_bytes_total[30s])`,
		window,
		10*time.Second,
	)
	if err != nil {
		t.Fatalf("queryPrometheusRange: %v", err)
	}

	if !strings.Contains(string(response), `"status":"success"`) {
		t.Fatalf("response = %s, want successful raw envelope", response)
	}

	if len(runner.commands) != 1 {
		t.Fatalf("commands = %v, want one command", runner.commands)
	}

	path := runner.commands[0][3]
	for _, want := range []string{"/query_range?", "step=10", "node_disk_written_bytes_total", "2026-08-04T01%3A02%3A03Z"} {
		if !strings.Contains(path, want) {
			t.Fatalf("query path %q is missing %q", path, want)
		}
	}
}

func TestPerformanceTelemetryQueriesBoundContainerdCardinality(t *testing.T) {
	queries := performanceTelemetryQueries()

	byName := make(map[string]performanceTelemetryQuery, len(queries))
	for _, query := range queries {
		byName[query.name] = query
		if strings.Contains(query.query, `containerd_.*|grpc_server_.*`) {
			t.Fatalf("query %q uses unbounded containerd and gRPC selector", query.name)
		}
	}

	for _, name := range []string{"containerd_image_pulls", "containerd_grpc_started", "containerd_grpc_handled"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("missing bounded telemetry query %q", name)
		}
	}

	if byName["containerd_image_pulls"].step != 0 || byName["containerd_grpc_started"].step != 0 {
		t.Fatal("containerd pull and gRPC started queries must use the default 10-second step")
	}

	if byName["containerd_grpc_handled"].step != 5*time.Minute {
		t.Fatalf("gRPC handled step = %s, want 5m", byName["containerd_grpc_handled"].step)
	}
}

func TestValidatePrometheusRangeResponseSize(t *testing.T) {
	if err := validatePrometheusRangeResponseSize([]byte("1234"), 4); err != nil {
		t.Fatalf("response at limit: %v", err)
	}

	if err := validatePrometheusRangeResponseSize([]byte("12345"), 4); err == nil || !strings.Contains(err.Error(), "5 bytes") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestParseContainerdJournal(t *testing.T) {
	window := telemetryWindow{
		StartedAt:  time.Date(2026, time.August, 4, 1, 2, 3, 0, time.UTC),
		FinishedAt: time.Date(2026, time.August, 4, 1, 12, 3, 0, time.UTC),
	}
	raw := strings.Join([]string{
		`[pod/observer-a/containerd-journal] 2026-08-04T01:02:02Z level=debug msg="layer unpacked" duration=1s layer=sha256:before`,
		`[pod/observer-a/containerd-journal] 2026-08-04T01:03:04.123456789Z level=debug msg="layer unpacked" duration=2.5s layer=sha256:abc`,
		`[pod/observer-b/containerd-journal] 2026-08-04T01:04:05Z level=info msg="Pulled image" image="example/image@sha256:def"`,
		`[pod/observer-a/containerd-journal] 2026-08-04T01:12:04Z level=debug msg="image unpacked" duration=3s`,
	}, "\n")

	events, err := parseContainerdJournal(raw, map[string]string{
		"observer-a": "node-a",
		"observer-b": "node-b",
	}, window)
	if err != nil {
		t.Fatalf("parseContainerdJournal: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("events = %v, want two phase-bounded events", events)
	}

	if events[0].NodeName != "node-a" || events[0].Type != "layer_unpacked" ||
		events[0].LayerDigest != "sha256:abc" || events[0].DurationSeconds != 2.5 {
		t.Fatalf("first event = %+v, want parsed layer event", events[0])
	}

	if events[1].NodeName != "node-b" || events[1].Type != "pull_completed" {
		t.Fatalf("second event = %+v, want correlated pull completion", events[1])
	}
}

func TestParseContainerdJournalRequiresEveryObserver(t *testing.T) {
	window := telemetryWindow{
		StartedAt:  time.Date(2026, time.August, 4, 1, 2, 3, 0, time.UTC),
		FinishedAt: time.Date(2026, time.August, 4, 1, 12, 3, 0, time.UTC),
	}
	raw := `[pod/observer-a/containerd-journal] 2026-08-04T01:03:04Z level=debug msg="image unpacked" duration=2s`

	_, err := parseContainerdJournal(raw, map[string]string{
		"observer-a": "node-a",
		"observer-b": "node-b",
	}, window)
	if err == nil || !strings.Contains(err.Error(), "1/2 observer pods") {
		t.Fatalf("error = %v, want incomplete observer coverage", err)
	}
}

func TestValidatePrometheusRangePodCoverage(t *testing.T) {
	raw := json.RawMessage(`{"data":{"result":[
		{"metric":{"pod":"observer-a"},"values":[[1,"2"]]},
		{"metric":{"pod":"observer-b"},"values":[[1,"3"]]}
	]}}`)

	if err := validatePrometheusRangePodCoverage("disk", raw, 2); err != nil {
		t.Fatalf("validatePrometheusRangePodCoverage: %v", err)
	}

	if err := validatePrometheusRangePodCoverage("disk", raw, 3); err == nil || !strings.Contains(err.Error(), "2/3 pods") {
		t.Fatalf("error = %v, want partial pod coverage", err)
	}
}
