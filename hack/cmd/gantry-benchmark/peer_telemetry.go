// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type gantryPeerPodMeasurement struct {
	PodName  string            `json:"pod_name"`
	NodeName string            `json:"node_name"`
	ByKind   map[string]uint64 `json:"by_kind"`
	Total    uint64            `json:"total"`
}

type gantryPeerPhaseMeasurement struct {
	Pods     []gantryPeerPodMeasurement `json:"pods"`
	Total    uint64                     `json:"total"`
	Source   string                     `json:"source"`
	Complete bool                       `json:"complete"`
}

type prometheusSample struct {
	Metric map[string]string
	Value  float64
}

type peerByteSnapshot struct {
	PodNodes map[string]string
	Counters map[string]map[string]uint64
}

type gantryPodDiagnosticMeasurement struct {
	PodName                                     string             `json:"pod_name"`
	NodeName                                    string             `json:"node_name"`
	CounterDeltas                               map[string]float64 `json:"counter_deltas"`
	TimestampSeconds                            map[string]float64 `json:"timestamp_seconds"`
	FinalLayerResponseCompletedTimestampSeconds float64            `json:"final_layer_response_completed_timestamp_seconds,omitempty"`
}

type gantryDiagnosticPhaseMeasurement struct {
	Pods     []gantryPodDiagnosticMeasurement `json:"pods"`
	Source   string                           `json:"source"`
	Complete bool                             `json:"complete"`
}

type gantryDiagnosticSnapshot struct {
	PodNodes map[string]string
	Counters map[string]map[string]float64
}

func (b *benchmark) queryPrometheusSamples(ctx context.Context, query string) ([]prometheusSample, error) {
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:9090/proxy/api/v1/query?query=%s",
		b.config.MonitoringNamespace,
		b.config.PrometheusService,
		url.QueryEscape(query),
	)

	output, err := b.commands.Run(ctx, nil, "kubectl", "get", "--raw", rawPath)
	if err != nil {
		return nil, err
	}

	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}

	if response.Status != "success" {
		return nil, fmt.Errorf("prometheus query status is %q", response.Status)
	}

	samples := make([]prometheusSample, 0, len(response.Data.Result))
	for _, result := range response.Data.Result {
		rawValue, ok := result.Value[1].(string)
		if !ok {
			return nil, fmt.Errorf("prometheus sample has non-string value")
		}

		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return nil, fmt.Errorf("parse Prometheus sample %q: %w", rawValue, err)
		}

		samples = append(samples, prometheusSample{Metric: result.Metric, Value: value})
	}

	return samples, nil
}

func (b *benchmark) gantryPodNodes(ctx context.Context, revision string) (map[string]string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.GantryNamespace,
		"get", "pods", "-l", "app.kubernetes.io/name="+b.config.GantryDaemonSet,
		"-o", "json",
	)
	if err != nil {
		return nil, err
	}

	var pods struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &pods); err != nil {
		return nil, fmt.Errorf("decode Gantry pods: %w", err)
	}

	result := make(map[string]string, b.config.NodeCount)

	for _, pod := range pods.Items {
		if pod.Metadata.Labels["controller-revision-hash"] != revision {
			continue
		}

		if pod.Metadata.Name == "" || pod.Spec.NodeName == "" {
			return nil, fmt.Errorf("gantry pod has empty name or nodeName")
		}

		result[pod.Metadata.Name] = pod.Spec.NodeName
	}

	if len(result) != b.config.NodeCount {
		return nil, fmt.Errorf("gantry pod/node map has %d pods for revision %s, want %d", len(result), revision, b.config.NodeCount)
	}

	return result, nil
}

func (b *benchmark) fetchGantryDiagnosticSnapshot(ctx context.Context, revision string) (gantryDiagnosticSnapshot, error) {
	podNodes, err := b.gantryPodNodes(ctx, revision)
	if err != nil {
		return gantryDiagnosticSnapshot{}, err
	}

	query := fmt.Sprintf(
		`{__name__=~"p2p_peer_fetch_total|p2p_peer_fetch_duration_seconds_(sum|count)|p2p_dht_lookup_total|p2p_dht_lookup_duration_seconds_(sum|count)|gantry_peer_fetch_bytes_total|gantry_containerd_commit_observed_total|gantry_containerd_commit_observation_duration_seconds_(sum|count)|gantry_containerd_commit_missing_after_stream_total",namespace=%q,gantry_benchmark="true",controller_revision_hash=%q}`,
		b.config.GantryNamespace,
		revision,
	)

	samples, err := b.queryPrometheusSamples(ctx, query)
	if err != nil {
		return gantryDiagnosticSnapshot{}, err
	}

	counters := make(map[string]map[string]float64, len(podNodes))
	for pod := range podNodes {
		counters[pod] = map[string]float64{}
	}

	for _, sample := range samples {
		pod := sample.Metric["pod"]
		if _, ok := podNodes[pod]; !ok {
			return gantryDiagnosticSnapshot{}, fmt.Errorf("diagnostic sample belongs to unexpected pod %q", pod)
		}

		if sample.Value < 0 {
			return gantryDiagnosticSnapshot{}, fmt.Errorf("diagnostic sample for pod %s is negative: %v", pod, sample.Value)
		}

		key, err := diagnosticMetricKey(sample.Metric)
		if err != nil {
			return gantryDiagnosticSnapshot{}, err
		}

		counters[pod][key] = sample.Value
	}

	return gantryDiagnosticSnapshot{PodNodes: podNodes, Counters: counters}, nil
}

func diagnosticMetricKey(labels map[string]string) (string, error) {
	name := labels["__name__"]
	if name == "" {
		return "", fmt.Errorf("diagnostic sample has no __name__ label")
	}

	parts := make([]string, 0, 3)

	for _, label := range []string{"kind", "outcome", "source"} {
		if value := labels[label]; value != "" {
			parts = append(parts, label+"="+value)
		}
	}

	if len(parts) == 0 {
		return name, nil
	}

	return name + "{" + strings.Join(parts, ",") + "}", nil
}

func (b *benchmark) fetchGantryDiagnosticTimestamps(
	ctx context.Context,
	revision string,
	window telemetryWindow,
) (map[string]map[string]float64, error) {
	query := fmt.Sprintf(
		`gantry_mirror_response_completed_timestamp_seconds{namespace=%q,kind="layer",gantry_benchmark="true",controller_revision_hash=%q} or gantry_containerd_commit_observed_timestamp_seconds{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q} or gantry_peer_fetch_last_timestamp_seconds{namespace=%q,outcome=~"busy|stall",gantry_benchmark="true",controller_revision_hash=%q}`,
		b.config.GantryNamespace,
		revision,
		b.config.GantryNamespace,
		revision,
		b.config.GantryNamespace,
		revision,
	)

	raw, err := b.queryPrometheusRange(ctx, query, window, performanceTelemetryStep)
	if err != nil {
		return nil, err
	}

	if err := validatePrometheusRangePodCoverage("gantry diagnostic timestamps", raw, b.config.NodeCount); err != nil {
		return nil, err
	}

	var response struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode diagnostic timestamp range response: %w", err)
	}

	result := map[string]map[string]float64{}

	for _, series := range response.Data.Result {
		key, err := diagnosticMetricKey(series.Metric)
		if err != nil {
			return nil, err
		}

		pod := series.Metric["pod"]
		for _, pair := range series.Values {
			rawValue, ok := pair[1].(string)
			if !ok {
				return nil, fmt.Errorf("diagnostic timestamp sample has non-string value")
			}

			value, err := strconv.ParseFloat(rawValue, 64)
			if err != nil {
				return nil, fmt.Errorf("parse diagnostic timestamp sample %q: %w", rawValue, err)
			}

			observedAt := time.Unix(0, int64(value*float64(time.Second)))
			if observedAt.Before(window.StartedAt) || observedAt.After(window.FinishedAt) {
				continue
			}

			if result[pod] == nil {
				result[pod] = map[string]float64{}
			}

			if value > result[pod][key] {
				result[pod][key] = value
			}
		}
	}

	return result, nil
}

func subtractGantryDiagnosticSnapshots(
	before, after gantryDiagnosticSnapshot,
	timestamps map[string]map[string]float64,
) (gantryDiagnosticPhaseMeasurement, error) {
	if len(before.PodNodes) != len(after.PodNodes) {
		return gantryDiagnosticPhaseMeasurement{}, fmt.Errorf(
			"gantry diagnostic pod set changed during phase: before=%d after=%d",
			len(before.PodNodes),
			len(after.PodNodes),
		)
	}

	pods := make([]gantryPodDiagnosticMeasurement, 0, len(before.PodNodes))
	for _, pod := range sortedMapKeys(before.PodNodes) {
		node := before.PodNodes[pod]
		if after.PodNodes[pod] != node {
			return gantryDiagnosticPhaseMeasurement{}, fmt.Errorf("gantry pod %s disappeared or moved from node %s", pod, node)
		}

		deltas := map[string]float64{}

		for key, afterValue := range after.Counters[pod] {
			beforeValue := before.Counters[pod][key]
			if afterValue < beforeValue {
				return gantryDiagnosticPhaseMeasurement{}, fmt.Errorf(
					"gantry diagnostic counter decreased for pod %s metric %s: before=%v after=%v",
					pod,
					key,
					beforeValue,
					afterValue,
				)
			}

			if delta := afterValue - beforeValue; delta != 0 {
				deltas[key] = delta
			}
		}

		pods = append(pods, gantryPodDiagnosticMeasurement{
			PodName:          pod,
			NodeName:         node,
			CounterDeltas:    deltas,
			TimestampSeconds: timestamps[pod],
			FinalLayerResponseCompletedTimestampSeconds: finalLayerResponseCompletedTimestamp(timestamps[pod]),
		})
	}

	return gantryDiagnosticPhaseMeasurement{
		Pods:     pods,
		Source:   "per-pod Prometheus counter deltas and timestamp gauges",
		Complete: len(pods) > 0,
	}, nil
}

func finalLayerResponseCompletedTimestamp(timestamps map[string]float64) float64 {
	var latest float64
	for key, value := range timestamps {
		if strings.HasPrefix(key, "gantry_mirror_response_completed_timestamp_seconds{kind=layer,") && value > latest {
			latest = value
		}
	}

	return latest
}

func requireFinalLayerResponseTimestamps(
	timestamps map[string]map[string]float64,
	podNodes map[string]string,
) error {
	missing := make([]string, 0)

	for pod := range podNodes {
		if finalLayerResponseCompletedTimestamp(timestamps[pod]) == 0 {
			missing = append(missing, pod)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	sort.Strings(missing)

	return fmt.Errorf(
		"final layer response completion timestamp missing for %d/%d Gantry pods: %s",
		len(missing),
		len(podNodes),
		strings.Join(missing, ","),
	)
}

func (b *benchmark) fetchGantryPeerByteSnapshot(ctx context.Context, revision string) (peerByteSnapshot, error) {
	podNodes, err := b.gantryPodNodes(ctx, revision)
	if err != nil {
		return peerByteSnapshot{}, err
	}

	query := fmt.Sprintf(
		`gantry_peer_serve_bytes_total{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q}`,
		b.config.GantryNamespace,
		revision,
	)

	samples, err := b.queryPrometheusSamples(ctx, query)
	if err != nil {
		return peerByteSnapshot{}, err
	}

	counters := make(map[string]map[string]uint64, len(podNodes))
	for pod := range podNodes {
		counters[pod] = map[string]uint64{}
	}

	for _, sample := range samples {
		pod := sample.Metric["pod"]
		kind := sample.Metric["kind"]

		if _, ok := podNodes[pod]; !ok {
			return peerByteSnapshot{}, fmt.Errorf("peer byte sample belongs to unexpected pod %q", pod)
		}

		if kind == "" {
			return peerByteSnapshot{}, fmt.Errorf("peer byte sample for pod %s has no kind label", pod)
		}

		if sample.Value < 0 || sample.Value > math.MaxUint64 || math.Trunc(sample.Value) != sample.Value {
			return peerByteSnapshot{}, fmt.Errorf("peer byte sample for pod %s kind %s is not a uint64: %v", pod, kind, sample.Value)
		}

		counters[pod][kind] = uint64(sample.Value)
	}

	for _, kinds := range counters {
		for _, kind := range []string{"manifest", "config", "layer"} {
			if _, ok := kinds[kind]; !ok {
				kinds[kind] = 0
			}
		}
	}

	return peerByteSnapshot{PodNodes: podNodes, Counters: counters}, nil
}

func subtractPeerByteSnapshots(before, after peerByteSnapshot) (gantryPeerPhaseMeasurement, error) {
	if len(before.PodNodes) != len(after.PodNodes) {
		return gantryPeerPhaseMeasurement{}, fmt.Errorf("gantry pod set changed during phase: before=%d after=%d", len(before.PodNodes), len(after.PodNodes))
	}

	pods := make([]gantryPeerPodMeasurement, 0, len(before.PodNodes))

	var total uint64

	for _, pod := range sortedMapKeys(before.PodNodes) {
		node := before.PodNodes[pod]
		if after.PodNodes[pod] != node {
			return gantryPeerPhaseMeasurement{}, fmt.Errorf("gantry pod %s disappeared or moved from node %s", pod, node)
		}

		byKind := make(map[string]uint64, 3)

		var podTotal uint64

		for _, kind := range []string{"manifest", "config", "layer"} {
			beforeValue := before.Counters[pod][kind]

			afterValue := after.Counters[pod][kind]
			if afterValue < beforeValue {
				return gantryPeerPhaseMeasurement{}, fmt.Errorf(
					"gantry peer byte counter decreased for pod %s kind %s: before=%d after=%d",
					pod,
					kind,
					beforeValue,
					afterValue,
				)
			}

			delta := afterValue - beforeValue
			byKind[kind] = delta
			podTotal += delta
		}

		pods = append(pods, gantryPeerPodMeasurement{
			PodName:  pod,
			NodeName: node,
			ByKind:   byKind,
			Total:    podTotal,
		})
		total += podTotal
	}

	sort.Slice(pods, func(left, right int) bool { return pods[left].PodName < pods[right].PodName })

	return gantryPeerPhaseMeasurement{
		Pods:     pods,
		Total:    total,
		Source:   "gantry_peer_serve_bytes_total",
		Complete: len(pods) > 0,
	}, nil
}
