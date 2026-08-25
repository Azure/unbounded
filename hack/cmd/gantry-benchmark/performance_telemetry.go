// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	performanceTelemetryStep        = 10 * time.Second
	grpcHandledTelemetryStep        = 5 * time.Minute
	maxPrometheusRangeResponseBytes = 256 * 1024 * 1024
)

type prometheusRangeCapture struct {
	Name        string          `json:"name"`
	Query       string          `json:"query"`
	StepSeconds int             `json:"step_seconds"`
	Response    json.RawMessage `json:"response"`
}

type performanceTelemetryQuery struct {
	name  string
	query string
	step  time.Duration
}

type containerdJournalEvent struct {
	ObserverPod     string    `json:"observer_pod"`
	NodeName        string    `json:"node_name"`
	Timestamp       time.Time `json:"timestamp"`
	Type            string    `json:"type"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	LayerDigest     string    `json:"layer_digest,omitempty"`
	Message         string    `json:"message"`
}

type phasePerformanceTelemetry struct {
	Window                  telemetryWindow          `json:"window"`
	ObserverPodNodes        map[string]string        `json:"observer_pod_nodes"`
	Prometheus              []prometheusRangeCapture `json:"prometheus"`
	ContainerdJournal       string                   `json:"containerd_journal"`
	ContainerdJournalEvents []containerdJournalEvent `json:"containerd_journal_events"`
	Complete                bool                     `json:"complete"`
}

var journalFieldPattern = regexp.MustCompile(`(?:^|[[:space:]])([a-zA-Z_]+)="?([^"[:space:]]+)"?`)

func performanceTelemetryQueries() []performanceTelemetryQuery {
	return []performanceTelemetryQuery{
		{name: "node_disk_read_bytes_per_second", query: `rate(node_disk_read_bytes_total{gantry_benchmark="true"}[30s])`},
		{name: "node_disk_written_bytes_per_second", query: `rate(node_disk_written_bytes_total{gantry_benchmark="true"}[30s])`},
		{name: "node_disk_busy_ratio", query: `rate(node_disk_io_time_seconds_total{gantry_benchmark="true"}[30s])`},
		{name: "node_disk_weighted_io_seconds_per_second", query: `rate(node_disk_io_time_weighted_seconds_total{gantry_benchmark="true"}[30s])`},
		{name: "node_network_receive_bytes_per_second", query: `rate(node_network_receive_bytes_total{gantry_benchmark="true",device!="lo"}[30s])`},
		{name: "node_network_transmit_bytes_per_second", query: `rate(node_network_transmit_bytes_total{gantry_benchmark="true",device!="lo"}[30s])`},
		{name: "node_network_receive_utilization_ratio", query: `rate(node_network_receive_bytes_total{gantry_benchmark="true",device!="lo"}[30s]) / node_network_speed_bytes{gantry_benchmark="true",device!="lo"}`},
		{name: "node_network_transmit_utilization_ratio", query: `rate(node_network_transmit_bytes_total{gantry_benchmark="true",device!="lo"}[30s]) / node_network_speed_bytes{gantry_benchmark="true",device!="lo"}`},
		{name: "node_network_receive_drops_per_second", query: `rate(node_network_receive_drop_total{gantry_benchmark="true",device!="lo"}[30s])`},
		{name: "node_network_transmit_drops_per_second", query: `rate(node_network_transmit_drop_total{gantry_benchmark="true",device!="lo"}[30s])`},
		{name: "node_network_receive_errors_per_second", query: `rate(node_network_receive_errs_total{gantry_benchmark="true",device!="lo"}[30s])`},
		{name: "node_network_transmit_errors_per_second", query: `rate(node_network_transmit_errs_total{gantry_benchmark="true",device!="lo"}[30s])`},
		{name: "node_cpu_busy_ratio", query: `1 - avg by(pod) (rate(node_cpu_seconds_total{gantry_benchmark="true",mode="idle"}[30s]))`},
		{name: "node_memory_available_bytes", query: `node_memory_MemAvailable_bytes{gantry_benchmark="true"}`},
		{name: "containerd_process", query: `{__name__=~"process_(cpu_seconds_total|resident_memory_bytes|virtual_memory_bytes)",gantry_benchmark="true",endpoint="ctr-metrics"}`},
		{name: "containerd_image_pulls", query: `{__name__=~"containerd_cri_sandboxed_(image_pulls_total|in_progress_image_pulls_total|image_pulling_throughput_(sum|count))",gantry_benchmark="true"}`},
		{name: "containerd_grpc_started", query: `sum by (pod, grpc_service, grpc_method) (rate(grpc_server_started_total{gantry_benchmark="true"}[30s])) > 0`},
		{name: "containerd_grpc_handled", query: `sum by (pod, grpc_code) (rate(grpc_server_handled_total{gantry_benchmark="true"}[5m])) > 0`, step: grpcHandledTelemetryStep},
		{name: "gantry_peer_outcomes", query: `p2p_peer_fetch_total{gantry_benchmark="true"}`},
		{name: "gantry_peer_busy_stall_timestamps", query: `gantry_peer_fetch_last_timestamp_seconds{outcome=~"busy|stall",gantry_benchmark="true"}`},
		{name: "gantry_peer_duration", query: `{__name__=~"p2p_peer_fetch_duration_seconds_(bucket|sum|count)",outcome=~"busy|stall",gantry_benchmark="true"}`},
		{name: "gantry_dht_outcomes", query: `p2p_dht_lookup_total{gantry_benchmark="true"}`},
		{name: "gantry_dht_duration", query: `{__name__=~"p2p_dht_lookup_duration_seconds_(bucket|sum|count)",gantry_benchmark="true"}`},
		{name: "gantry_mirror_bytes", query: `gantry_mirror_bytes_served_total{gantry_benchmark="true"}`},
		{name: "gantry_response_completed", query: `gantry_mirror_response_completed_timestamp_seconds{kind="layer",gantry_benchmark="true"}`},
		{name: "gantry_commit_observation", query: `{__name__=~"gantry_containerd_commit_(observed_total|observed_timestamp_seconds|observation_duration_seconds_(sum|count)|latest_observation_duration_seconds|missing_after_stream_total)",gantry_benchmark="true"}`},
		// Seeding width: how many HRW pullers each layer actually activates, and
		// how many please_pull requests convert into origin pulls versus dedup.
		{name: "gantry_coord_seeding", query: `{__name__=~"p2p_coord_please_pull_(served|started|declined)_total|p2p_coord_pull_intent_served_total|p2p_prefetch_(batches|digests|groups)_total",gantry_benchmark="true"}`},
		{name: "gantry_prefetch_pullers", query: `{__name__=~"p2p_prefetch_pullers_per_manifest_(bucket|sum|count)",gantry_benchmark="true"}`},
	}
}

func (b *benchmark) capturePhasePerformanceTelemetry(
	ctx context.Context,
	phase proxyPhase,
	job jobObservation,
) (phasePerformanceTelemetry, error) {
	window := telemetryWindow{
		StartedAt:  job.PhaseStartedAt,
		FinishedAt: job.PhaseFinishedAt,
	}

	observerPods, err := b.observerPodNodes(ctx)
	if err != nil {
		return phasePerformanceTelemetry{}, err
	}

	queries := performanceTelemetryQueries()

	captures := make([]prometheusRangeCapture, 0, len(queries))
	for _, item := range queries {
		step := item.step
		if step == 0 {
			step = performanceTelemetryStep
		}

		response, err := b.queryPrometheusRange(ctx, item.query, window, step)
		if err != nil {
			return phasePerformanceTelemetry{}, fmt.Errorf("capture %s: %w", item.name, err)
		}

		if err := validatePrometheusRangePodCoverage(item.name, response, b.config.NodeCount); err != nil {
			return phasePerformanceTelemetry{}, err
		}

		captures = append(captures, prometheusRangeCapture{
			Name:        item.name,
			Query:       item.query,
			StepSeconds: int(step.Seconds()),
			Response:    response,
		})
	}

	journal, err := b.collectContainerdJournal(ctx, window)
	if err != nil {
		return phasePerformanceTelemetry{}, err
	}

	journalEvents, err := parseContainerdJournal(journal, observerPods, window)
	if err != nil {
		return phasePerformanceTelemetry{}, err
	}

	return phasePerformanceTelemetry{
		Window:                  window,
		ObserverPodNodes:        observerPods,
		Prometheus:              captures,
		ContainerdJournal:       journal,
		ContainerdJournalEvents: journalEvents,
		Complete:                true,
	}, nil
}

func validatePrometheusRangePodCoverage(name string, raw json.RawMessage, expectedPods int) error {
	var response struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode %s range response for pod coverage: %w", name, err)
	}

	pods := map[string]struct{}{}

	for _, series := range response.Data.Result {
		pod := series.Metric["pod"]
		if pod == "" {
			return fmt.Errorf("%s range series has no pod label", name)
		}

		if len(series.Values) > 0 {
			pods[pod] = struct{}{}
		}
	}

	if len(pods) != expectedPods {
		return fmt.Errorf("%s range capture has samples from %d/%d pods", name, len(pods), expectedPods)
	}

	return nil
}

func parseContainerdJournal(
	raw string,
	observerPodNodes map[string]string,
	window telemetryWindow,
) ([]containerdJournalEvent, error) {
	events := []containerdJournalEvent{}
	observedPods := map[string]struct{}{}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		prefixEnd := strings.Index(line, "] ")
		if !strings.HasPrefix(line, "[pod/") || prefixEnd < 0 {
			return nil, fmt.Errorf("parse containerd journal prefix: %q", line)
		}

		prefixParts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(line[:prefixEnd], "["), "]"), "/")
		if len(prefixParts) != 3 || prefixParts[0] != "pod" || prefixParts[2] != "containerd-journal" {
			return nil, fmt.Errorf("parse containerd journal source: %q", line[:prefixEnd+1])
		}

		observerPod := prefixParts[1]

		nodeName, ok := observerPodNodes[observerPod]
		if !ok {
			return nil, fmt.Errorf("containerd journal belongs to unexpected observer pod %q", observerPod)
		}

		remainder := line[prefixEnd+2:]

		timestampEnd := strings.IndexByte(remainder, ' ')
		if timestampEnd < 0 {
			return nil, fmt.Errorf("parse containerd journal timestamp: %q", line)
		}

		timestamp, err := time.Parse(time.RFC3339Nano, remainder[:timestampEnd])
		if err != nil {
			return nil, fmt.Errorf("parse containerd journal timestamp %q: %w", remainder[:timestampEnd], err)
		}

		if timestamp.Before(window.StartedAt) || timestamp.After(window.FinishedAt) {
			continue
		}

		message := remainder[timestampEnd+1:]

		eventType := classifyContainerdJournalEvent(message)
		if eventType == "" {
			return nil, fmt.Errorf("classify filtered containerd journal message: %q", message)
		}

		event := containerdJournalEvent{
			ObserverPod: observerPod,
			NodeName:    nodeName,
			Timestamp:   timestamp,
			Type:        eventType,
			Message:     message,
		}
		for _, match := range journalFieldPattern.FindAllStringSubmatch(message, -1) {
			switch match[1] {
			case "duration":
				duration, err := time.ParseDuration(match[2])
				if err != nil {
					return nil, fmt.Errorf("parse containerd journal duration %q: %w", match[2], err)
				}

				event.DurationSeconds = duration.Seconds()
			case "layer":
				event.LayerDigest = match[2]
			}
		}

		events = append(events, event)
		observedPods[observerPod] = struct{}{}
	}

	if len(observedPods) != len(observerPodNodes) {
		return nil, fmt.Errorf(
			"containerd journal has phase events from %d/%d observer pods",
			len(observedPods),
			len(observerPodNodes),
		)
	}

	return events, nil
}

func classifyContainerdJournalEvent(message string) string {
	switch {
	case strings.Contains(message, "layer unpacked"):
		return "layer_unpacked"
	case strings.Contains(message, "image unpacked"):
		return "image_unpacked"
	case strings.Contains(message, "cancel pulling image"):
		return "pull_canceled"
	case strings.Contains(message, "Pulled image"):
		return "pull_completed"
	case strings.Contains(message, "PullImage "):
		return "pull_started"
	default:
		return ""
	}
}

func (b *benchmark) queryPrometheusRange(
	ctx context.Context,
	query string,
	window telemetryWindow,
	step time.Duration,
) (json.RawMessage, error) {
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:9090/proxy/api/v1/query_range?query=%s&start=%s&end=%s&step=%d",
		b.config.MonitoringNamespace,
		b.config.PrometheusService,
		url.QueryEscape(query),
		url.QueryEscape(window.StartedAt.UTC().Format(time.RFC3339Nano)),
		url.QueryEscape(window.FinishedAt.UTC().Format(time.RFC3339Nano)),
		int(step.Seconds()),
	)

	output, err := b.commands.Run(ctx, nil, "kubectl", "get", "--raw", rawPath)
	if err != nil {
		return nil, err
	}

	if err := validatePrometheusRangeResponseSize(output, maxPrometheusRangeResponseBytes); err != nil {
		return nil, err
	}

	var envelope struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		return nil, fmt.Errorf("decode Prometheus range response: %w", err)
	}

	if envelope.Status != "success" {
		return nil, fmt.Errorf("prometheus range query status is %q", envelope.Status)
	}

	return json.RawMessage(output), nil
}

func validatePrometheusRangeResponseSize(output []byte, limit int) error {
	if len(output) > limit {
		return fmt.Errorf(
			"prometheus range response is %d bytes, exceeds %d-byte capture limit",
			len(output),
			limit,
		)
	}

	return nil
}

func (b *benchmark) observerPodNodes(ctx context.Context) (map[string]string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"get", "pods", "-l", "app.kubernetes.io/name=gantry-benchmark-node-observer",
		"-o", "json",
	)
	if err != nil {
		return nil, err
	}

	var pods struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &pods); err != nil {
		return nil, fmt.Errorf("decode observer pods: %w", err)
	}

	result := make(map[string]string, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Metadata.Name == "" || pod.Spec.NodeName == "" {
			return nil, fmt.Errorf("observer pod has empty name or nodeName")
		}

		result[pod.Metadata.Name] = pod.Spec.NodeName
	}

	if len(result) != b.config.NodeCount {
		return nil, fmt.Errorf("observer pod/node map has %d pods, want %d", len(result), b.config.NodeCount)
	}

	return result, nil
}

func (b *benchmark) collectContainerdJournal(ctx context.Context, window telemetryWindow) (string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"logs", "-l", "app.kubernetes.io/name=gantry-benchmark-node-observer",
		"-c", "containerd-journal",
		"--prefix=true",
		"--timestamps=true",
		"--max-log-requests", fmt.Sprintf("%d", b.config.NodeCount),
		"--since-time", window.StartedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return "", fmt.Errorf("collect containerd journal: %w", err)
	}

	return string(output), nil
}

func (b *benchmark) writePerformanceTelemetryArtifact(
	runID string,
	phase proxyPhase,
	measurement phasePerformanceTelemetry,
) error {
	return b.writeJSONArtifact(runID, string(phase)+"-performance.json", measurement)
}
