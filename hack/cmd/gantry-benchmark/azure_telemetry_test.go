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

type telemetryCommandRunner struct {
	logAnalytics []byte
	metrics      []byte
	commands     []string
}

func (r *telemetryCommandRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	command := name + " " + strings.Join(args, " ")
	r.commands = append(r.commands, command)

	if strings.Contains(command, "monitor metrics list") {
		return r.metrics, nil
	}

	return r.logAnalytics, nil
}

func logAnalyticsFixture(columns []string, rows [][]any) []byte {
	typedColumns := make([]map[string]string, 0, len(columns))
	for _, name := range columns {
		typedColumns = append(typedColumns, map[string]string{"name": name, "type": "string"})
	}

	encoded, err := json.Marshal(map[string]any{
		"tables": []any{map[string]any{
			"name":    "PrimaryResult",
			"columns": typedColumns,
			"rows":    rows,
		}},
	})
	if err != nil {
		panic(err)
	}

	return encoded
}

func TestCollectACRPulls(t *testing.T) {
	runner := &telemetryCommandRunner{logAnalytics: logAnalyticsFixture(
		[]string{"pull_count", "successful_pull_count", "other_repository_event_count", "first_event_at", "last_event_at"},
		[][]any{{float64(5), float64(5), float64(0), "2026-07-30T10:00:01Z", "2026-07-30T10:00:05Z"}},
	)}
	benchmark := &benchmark{
		config: benchmarkConfig{
			LogAnalyticsWorkspaceID: "workspace-id",
			ACRResourceID:           "/subscriptions/s/resourceGroups/r/providers/Microsoft.ContainerRegistry/registries/acr",
			WorkloadRepository:      "gantry-benchmark-pull",
		},
		commands: runner,
	}

	measurement, err := benchmark.collectACRPulls(
		context.Background(),
		"acr.example/gantry-benchmark-pull@sha256:abc",
		telemetryWindow{StartedAt: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("collectACRPulls: %v", err)
	}

	if measurement.PullCount != 5 || measurement.SuccessfulPullCount != 5 || !measurement.Complete {
		t.Fatalf("measurement = %+v, want five complete pulls", measurement)
	}

	command := runner.commands[0]
	for _, want := range []string{"/query?timespan=", "ContainerRegistryRepositoryEvents", "sha256:abc"} {
		if !strings.Contains(command, want) {
			t.Fatalf("command %q is missing %q", command, want)
		}
	}
}

func TestCollectACRPullsRejectsConcurrentRepositoryTraffic(t *testing.T) {
	runner := &telemetryCommandRunner{logAnalytics: logAnalyticsFixture(
		[]string{"pull_count", "successful_pull_count", "other_repository_event_count", "first_event_at", "last_event_at"},
		[][]any{{float64(5), float64(5), float64(1), "2026-07-30T10:00:01Z", "2026-07-30T10:00:05Z"}},
	)}
	benchmark := &benchmark{
		config: benchmarkConfig{
			LogAnalyticsWorkspaceID: "workspace-id",
			ACRResourceID:           "/subscriptions/s/resourceGroups/r/providers/Microsoft.ContainerRegistry/registries/acr",
			WorkloadRepository:      "gantry-benchmark-pull",
		},
		commands: runner,
	}

	measurement, err := benchmark.collectACRPulls(
		context.Background(),
		"acr.example/gantry-benchmark-pull@sha256:abc",
		telemetryWindow{StartedAt: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("collectACRPulls: %v", err)
	}

	if measurement.Complete || measurement.OtherRepositoryEventCount != 1 {
		t.Fatalf("measurement = %+v, want incomplete concurrent traffic", measurement)
	}
}

func TestCollectPrivateEndpointBytes(t *testing.T) {
	totalA := 100.0
	totalB := 23.0

	metrics, err := json.Marshal(azureMetricsResponse{Value: []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
		Timeseries []struct {
			Data []struct {
				Total *float64 `json:"total"`
			} `json:"data"`
		} `json:"timeseries"`
	}{
		{
			Name: struct {
				Value string `json:"value"`
			}{Value: "PEBytesIn"},
			Timeseries: []struct {
				Data []struct {
					Total *float64 `json:"total"`
				} `json:"data"`
			}{{Data: []struct {
				Total *float64 `json:"total"`
			}{{Total: &totalA}, {Total: &totalB}}}},
		},
	}})
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}

	runner := &telemetryCommandRunner{metrics: metrics}
	benchmark := &benchmark{
		config:   benchmarkConfig{ACRPrivateEndpointResourceID: "/subscriptions/s/privateEndpoints/acr"},
		commands: runner,
	}

	measurement, err := benchmark.collectPrivateEndpointBytes(
		context.Background(),
		telemetryWindow{StartedAt: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("collectPrivateEndpointBytes: %v", err)
	}

	if measurement.BytesFromACR != 123 || !measurement.Complete {
		t.Fatalf("measurement = %+v, want 123 complete bytes", measurement)
	}
}

func TestCollectPrivateEndpointBytesAcceptsQueryableZero(t *testing.T) {
	zero := 0.0

	metrics, err := json.Marshal(azureMetricsResponse{Value: []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
		Timeseries []struct {
			Data []struct {
				Total *float64 `json:"total"`
			} `json:"data"`
		} `json:"timeseries"`
	}{
		{
			Name: struct {
				Value string `json:"value"`
			}{Value: "PEBytesIn"},
			Timeseries: []struct {
				Data []struct {
					Total *float64 `json:"total"`
				} `json:"data"`
			}{{Data: []struct {
				Total *float64 `json:"total"`
			}{{Total: &zero}}}},
		},
	}})
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}

	benchmark := &benchmark{
		config:   benchmarkConfig{ACRPrivateEndpointResourceID: "/subscriptions/s/privateEndpoints/acr"},
		commands: &telemetryCommandRunner{metrics: metrics},
	}

	measurement, err := benchmark.collectPrivateEndpointBytes(
		context.Background(),
		telemetryWindow{StartedAt: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("collectPrivateEndpointBytes: %v", err)
	}

	if measurement.BytesFromACR != 0 || !measurement.Complete {
		t.Fatalf("measurement = %+v, want complete zero-byte window", measurement)
	}
}

func TestCollectAuditLatency(t *testing.T) {
	columns := []string{"RequestReceivedTime", "Verb", "Subresource", "Name", "RequestObject"}
	started := `{"status":{"containerStatuses":[{"state":{"running":{"startedAt":"2026-07-30T10:00:03Z"}}}]}}`
	rows := [][]any{
		{"2026-07-30T10:00:01Z", "create", "", "job-a-pod", nil},
		{"2026-07-30T10:00:02Z", "create", "binding", "job-a-pod", nil},
		{"2026-07-30T10:00:04Z", "patch", "status", "job-a-pod", started},
	}

	runner := &telemetryCommandRunner{logAnalytics: logAnalyticsFixture(columns, rows)}
	benchmark := &benchmark{
		config: benchmarkConfig{
			Namespace:               "gantry-benchmark",
			LogAnalyticsWorkspaceID: "workspace-id",
			AKSResourceID:           "/subscriptions/s/managedClusters/aks",
		},
		commands: runner,
	}
	job := jobObservation{
		JobName:  "job-a",
		Pods:     []string{"job-a-pod"},
		PodNodes: map[string]string{"job-a-pod": "node-a"},
	}

	measurement, err := benchmark.collectAuditLatency(
		context.Background(),
		job,
		telemetryWindow{StartedAt: time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 7, 30, 10, 1, 0, 0, time.UTC)},
	)
	if err != nil {
		t.Fatalf("collectAuditLatency: %v", err)
	}

	if !measurement.Complete || len(measurement.Pods) != 1 {
		t.Fatalf("measurement = %+v, want one complete pod", measurement)
	}

	pod := measurement.Pods[0]
	if pod.NodeName != "node-a" || pod.StartupSeconds != 3 || pod.SchedulingSeconds != 1 || pod.PostBindStartupSeconds != 2 {
		t.Fatalf("pod latency = %+v", pod)
	}

	command := runner.commands[0]
	for _, want := range []string{"/search?timespan=", "parse_json(ObjectRef)", "ParsedResponseObject.metadata.name", "GenerateName startswith"} {
		if !strings.Contains(command, want) {
			t.Fatalf("audit command %q is missing %q", command, want)
		}
	}
}

func TestTelemetryConfigRequiresAllResourceIDs(t *testing.T) {
	config, err := loadBenchmarkConfig(envFromMap(map[string]string{
		"BENCHMARK_MODE":            "direct",
		"BENCHMARK_AZURE_TELEMETRY": "true",
		"ACR_LOGIN_SERVER":          "acr.example",
	}))
	if err != nil {
		t.Fatalf("loadBenchmarkConfig: %v", err)
	}

	if err := config.validateEnable(); err == nil {
		t.Fatal("validateEnable passed without Azure telemetry resource IDs")
	}
}

func TestNextMetricBoundary(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 30, 123, time.FixedZone("offset", -4*60*60))
	want := time.Date(2026, 7, 30, 14, 1, 0, 0, time.UTC)

	if got := nextMetricBoundary(now); !got.Equal(want) {
		t.Fatalf("nextMetricBoundary(%s) = %s, want %s", now, got, want)
	}
}

func TestMetricWindowCloseBoundaryIncludesTrailingGuard(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 30, 0, time.UTC)
	want := time.Date(2026, 7, 30, 10, 4, 0, 0, time.UTC)

	if got := metricWindowCloseBoundary(now); !got.Equal(want) {
		t.Fatalf("metricWindowCloseBoundary(%s) = %s, want %s", now, got, want)
	}
}

func TestAzureTelemetrySettlementRequiresCompleteStableWindow(t *testing.T) {
	start := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	tracker := azureTelemetrySettlement{window: 30 * time.Second}
	measurement := azurePhaseMeasurement{
		ACR:             acrPhaseMeasurement{PullCount: 5, SuccessfulPullCount: 5, Complete: true},
		PrivateEndpoint: privateEndpointPhaseMeasurement{BytesFromACR: 100, Complete: true},
		Audit:           auditPhaseMeasurement{Complete: true},
		Complete:        true,
	}

	if tracker.Observe(start, measurement) {
		t.Fatal("settled on first complete observation")
	}

	if tracker.Observe(start.Add(29*time.Second), measurement) {
		t.Fatal("settled before stability window")
	}

	if !tracker.Observe(start.Add(30*time.Second), measurement) {
		t.Fatal("did not settle after stability window")
	}

	measurement.PrivateEndpoint.BytesFromACR++
	if tracker.Observe(start.Add(31*time.Second), measurement) {
		t.Fatal("settled when delayed metric changed")
	}

	measurement.Complete = false
	if tracker.Observe(start.Add(time.Minute), measurement) {
		t.Fatal("settled with incomplete telemetry")
	}
}
