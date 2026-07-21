// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

func (b *benchmark) preflight(ctx context.Context) error {
	state, err := b.loadState(ctx)
	if err != nil {
		return fmt.Errorf("load enabled benchmark: %w", err)
	}

	if err := b.requireLock(ctx, state.RunID); err != nil {
		return err
	}

	if state.Status != "enabled" && state.Status != "preflight-passed" {
		return fmt.Errorf("benchmark state is %q, complete enable or run disable before preflight", state.Status)
	}

	if err := b.validateContext(ctx); err != nil {
		return err
	}

	if _, err := b.targetNodes(ctx); err != nil {
		return err
	}

	if err := b.validateGantry(ctx); err != nil {
		return err
	}

	if err := b.validateACRMonitoring(ctx); err != nil {
		return err
	}

	if err := b.checkMonitoring(ctx, state); err != nil {
		return err
	}

	state.Status = "preflight-passed"
	if err := b.saveState(ctx, state); err != nil {
		return err
	}

	writeAll(b.stdout, fmt.Sprintf("preflight passed for %s on %d nodes\n", state.RunID, b.config.NodeCount))

	return nil
}

func (b *benchmark) checkMonitoring(ctx context.Context, state benchmarkState) error {
	revision, err := b.gantryRevision(ctx)
	if err != nil {
		return err
	}

	if err := b.waitForGantryRevisionScrape(ctx, revision); err != nil {
		return err
	}

	dhtCountQuery := fmt.Sprintf(
		`count(p2p_dht_health_score{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	dhtCount, err := b.queryPrometheus(ctx, dhtCountQuery)
	if err != nil {
		return fmt.Errorf("query Gantry DHT metric count: %w", err)
	}

	if int(dhtCount) != b.config.NodeCount {
		return fmt.Errorf("prometheus reports DHT health for %.0f/%d Gantry pods", dhtCount, b.config.NodeCount)
	}

	dhtQuery := fmt.Sprintf(
		`min(p2p_dht_health_score{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	dhtHealth, err := b.queryPrometheus(ctx, dhtQuery)
	if err != nil {
		return fmt.Errorf("query Gantry DHT health: %w", err)
	}

	if dhtHealth <= 0 {
		return fmt.Errorf("minimum Gantry DHT health is %.3f, want greater than zero", dhtHealth)
	}

	return nil
}

func (b *benchmark) queryPrometheus(ctx context.Context, query string) (float64, error) {
	return b.queryPrometheusValue(ctx, query, false)
}

func (b *benchmark) queryPrometheusOrZero(ctx context.Context, query string) (float64, error) {
	return b.queryPrometheusValue(ctx, query, true)
}

func (b *benchmark) queryPrometheusValue(ctx context.Context, query string, zeroOnEmpty bool) (float64, error) {
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:%s:9090/proxy/api/v1/query?query=%s",
		b.config.MonitoringNamespace,
		b.config.PrometheusService,
		url.QueryEscape(query),
	)

	output, err := b.commands.Run(ctx, nil, "kubectl", "get", "--raw", rawPath)
	if err != nil {
		return 0, err
	}

	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return 0, fmt.Errorf("decode Prometheus response: %w", err)
	}

	if response.Status != "success" {
		return 0, fmt.Errorf("prometheus query status is %q", response.Status)
	}

	if len(response.Data.Result) == 0 {
		if zeroOnEmpty {
			return 0, nil
		}

		return 0, fmt.Errorf("prometheus query returned no samples")
	}

	var total float64

	for _, result := range response.Data.Result {
		value, ok := result.Value[1].(string)
		if !ok {
			return 0, fmt.Errorf("prometheus sample has non-string value")
		}

		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, fmt.Errorf("parse Prometheus sample %q: %w", value, err)
		}

		total += parsed
	}

	return total, nil
}
