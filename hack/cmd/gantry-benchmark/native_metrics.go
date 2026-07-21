// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

const mebibyte = 1024 * 1024

type acrPullMetrics struct {
	Total         uint64    `json:"total"`
	Successful    uint64    `json:"successful"`
	Failed        uint64    `json:"failed"`
	WindowStarted time.Time `json:"window_started_at"`
	WindowEnded   time.Time `json:"window_ended_at"`
}

type kubeletPullMetrics struct {
	Operations             float64 `json:"operations"`
	Errors                 float64 `json:"errors"`
	DurationSamples        float64 `json:"duration_samples"`
	DurationSeconds        float64 `json:"duration_seconds"`
	AverageDurationSeconds float64 `json:"average_duration_seconds"`
}

func (b *benchmark) validateACRMonitoring(ctx context.Context) error {
	resourceID, err := b.acrResourceID(ctx)
	if err != nil {
		return err
	}

	output, err := b.commands.Run(
		ctx,
		nil,
		"az", "monitor", "metrics", "list-definitions",
		"--resource", resourceID,
		"-o", "json",
	)
	if err != nil {
		return fmt.Errorf("list ACR metric definitions: %w", err)
	}

	var definitions []struct {
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
	}
	if err := json.Unmarshal(output, &definitions); err != nil {
		return fmt.Errorf("decode ACR metric definitions: %w", err)
	}

	found := make(map[string]bool, 2)
	for _, definition := range definitions {
		found[definition.Name.Value] = true
	}

	for _, required := range []string{"TotalPullCount", "SuccessfulPullCount"} {
		if !found[required] {
			return fmt.Errorf("ACR does not expose required metric %s", required)
		}
	}

	return nil
}

func (b *benchmark) waitForACRPullMetrics(ctx context.Context, startedAt, finishedAt time.Time) (acrPullMetrics, error) {
	availableAt := finishedAt.Add(b.config.MetricsSettleTime)
	if delay := time.Until(availableAt); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return acrPullMetrics{}, ctx.Err()
		case <-timer.C:
		}
	}

	deadline := time.Now().Add(b.config.RolloutTimeout)
	for {
		metrics, err := b.queryACRPullMetrics(ctx, startedAt, finishedAt)
		if err == nil && metrics.Total > 0 {
			return metrics, nil
		}

		if time.Now().After(deadline) {
			if err != nil {
				return acrPullMetrics{}, err
			}

			return acrPullMetrics{}, fmt.Errorf("ACR pull metrics remained empty for window %s to %s", metrics.WindowStarted, metrics.WindowEnded)
		}

		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()

			return acrPullMetrics{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (b *benchmark) queryACRPullMetrics(ctx context.Context, startedAt, finishedAt time.Time) (acrPullMetrics, error) {
	resourceID, err := b.acrResourceID(ctx)
	if err != nil {
		return acrPullMetrics{}, err
	}

	windowStart := startedAt.UTC().Truncate(time.Minute)
	windowEnd := finishedAt.UTC().Truncate(time.Minute).Add(time.Minute)

	output, err := b.commands.Run(
		ctx,
		nil,
		"az", "monitor", "metrics", "list",
		"--resource", resourceID,
		"--metrics", "TotalPullCount", "SuccessfulPullCount",
		"--interval", "PT1M",
		"--aggregation", "Total",
		"--start-time", windowStart.Format(time.RFC3339),
		"--end-time", windowEnd.Format(time.RFC3339),
		"-o", "json",
	)
	if err != nil {
		return acrPullMetrics{}, fmt.Errorf("query ACR pull metrics: %w", err)
	}

	return parseACRPullMetrics(output, windowStart, windowEnd)
}

func (b *benchmark) acrResourceID(ctx context.Context) (string, error) {
	name, err := acrRegistryName(b.config.ACRLoginServer)
	if err != nil {
		return "", err
	}

	output, err := b.commands.Run(
		ctx,
		nil,
		"az", "acr", "show",
		"--name", name,
		"--query", "id",
		"-o", "tsv",
	)
	if err != nil {
		return "", fmt.Errorf("resolve ACR resource ID: %w", err)
	}

	resourceID := strings.TrimSpace(string(output))
	if resourceID == "" {
		return "", fmt.Errorf("ACR %s has an empty resource ID", name)
	}

	return resourceID, nil
}

func acrRegistryName(loginServer string) (string, error) {
	host := strings.TrimSpace(loginServer)
	if strings.Contains(host, "://") {
		parsed, err := url.Parse(host)
		if err != nil {
			return "", fmt.Errorf("parse ACR login server: %w", err)
		}

		host = parsed.Hostname()
	}

	name, _, _ := strings.Cut(host, ".")
	if name == "" {
		return "", fmt.Errorf("ACR login server %q has no registry name", loginServer)
	}

	return name, nil
}

func parseACRPullMetrics(raw []byte, windowStart, windowEnd time.Time) (acrPullMetrics, error) {
	var response struct {
		Value []struct {
			Name struct {
				Value string `json:"value"`
			} `json:"name"`
			TimeSeries []struct {
				Data []struct {
					Total *float64 `json:"total"`
				} `json:"data"`
			} `json:"timeseries"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return acrPullMetrics{}, fmt.Errorf("decode ACR pull metrics: %w", err)
	}

	counts := make(map[string]float64, 2)

	for _, metric := range response.Value {
		for _, series := range metric.TimeSeries {
			for _, point := range series.Data {
				if point.Total != nil {
					counts[metric.Name.Value] += *point.Total
				}
			}
		}
	}

	total, err := metricCount(counts["TotalPullCount"], "TotalPullCount")
	if err != nil {
		return acrPullMetrics{}, err
	}

	successful, err := metricCount(counts["SuccessfulPullCount"], "SuccessfulPullCount")
	if err != nil {
		return acrPullMetrics{}, err
	}

	failed := uint64(0)
	if total > successful {
		failed = total - successful
	}

	return acrPullMetrics{
		Total:         total,
		Successful:    successful,
		Failed:        failed,
		WindowStarted: windowStart,
		WindowEnded:   windowEnd,
	}, nil
}

func metricCount(value float64, name string) (uint64, error) {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("ACR metric %s has invalid value %v", name, value)
	}

	return uint64(math.Round(value)), nil
}

func (b *benchmark) fetchKubeletPullMetrics(ctx context.Context) (kubeletPullMetrics, error) {
	queries := []struct {
		name  string
		query string
		set   func(*kubeletPullMetrics, float64)
	}{
		{
			name:  "operations",
			query: `sum(kubelet_runtime_operations_total{operation_type="pull_image"})`,
			set:   func(metrics *kubeletPullMetrics, value float64) { metrics.Operations = value },
		},
		{
			name:  "errors",
			query: `sum(kubelet_runtime_operations_errors_total{operation_type="pull_image"})`,
			set:   func(metrics *kubeletPullMetrics, value float64) { metrics.Errors = value },
		},
		{
			name:  "duration samples",
			query: `sum(kubelet_image_pull_duration_seconds_count)`,
			set:   func(metrics *kubeletPullMetrics, value float64) { metrics.DurationSamples = value },
		},
		{
			name:  "duration seconds",
			query: `sum(kubelet_image_pull_duration_seconds_sum)`,
			set:   func(metrics *kubeletPullMetrics, value float64) { metrics.DurationSeconds = value },
		},
	}

	var result kubeletPullMetrics

	for _, query := range queries {
		value, err := b.queryPrometheusOrZero(ctx, query.query)
		if err != nil {
			return kubeletPullMetrics{}, fmt.Errorf("query kubelet pull %s: %w", query.name, err)
		}

		query.set(&result, value)
	}

	return result, nil
}

func subtractKubeletPullMetrics(after, before kubeletPullMetrics) kubeletPullMetrics {
	result := kubeletPullMetrics{
		Operations:      nonNegativeDifference(after.Operations, before.Operations),
		Errors:          nonNegativeDifference(after.Errors, before.Errors),
		DurationSamples: nonNegativeDifference(after.DurationSamples, before.DurationSamples),
		DurationSeconds: nonNegativeDifference(after.DurationSeconds, before.DurationSeconds),
	}
	if result.DurationSamples > 0 {
		result.AverageDurationSeconds = result.DurationSeconds / result.DurationSamples
	}

	return result
}

func estimatedBaselineBytes(nodeCount, imageSizeMiB int) uint64 {
	return uint64(nodeCount) * uint64(imageSizeMiB) * mebibyte
}

func estimatedGantryOriginBytes(layerPulls float64, imageSizeMiB, imageLayers int) uint64 {
	if layerPulls <= 0 || imageSizeMiB <= 0 || imageLayers <= 0 {
		return 0
	}

	averageLayerBytes := float64(imageSizeMiB*mebibyte) / float64(imageLayers)

	return uint64(math.Round(layerPulls * averageLayerBytes))
}
