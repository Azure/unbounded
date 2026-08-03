// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
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
