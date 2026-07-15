// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type proxyTrafficTotals struct {
	Requests      uint64 `json:"requests"`
	BytesUpstream uint64 `json:"bytes_upstream"`
	BytesToClient uint64 `json:"bytes_to_client"`
}

type proxyPhaseTotals struct {
	RequestsCompleted uint64                        `json:"requests_completed"`
	BytesUpstream     uint64                        `json:"bytes_upstream"`
	BytesToClient     uint64                        `json:"bytes_to_client"`
	ByPathClass       map[string]proxyTrafficTotals `json:"by_path_class"`
	ByClientClass     map[string]proxyTrafficTotals `json:"by_client_class"`
}

type proxySummary struct {
	RunID  string     `json:"run_id"`
	Phase  proxyPhase `json:"phase"`
	Totals struct {
		ByPhase map[proxyPhase]proxyPhaseTotals `json:"by_phase"`
	} `json:"totals"`
}

type gantryMetrics struct {
	OriginPulls   float64 `json:"origin_pulls"`
	PeerFetchHits float64 `json:"peer_fetch_hits"`
}

type phaseResult struct {
	RunID        string           `json:"run_id"`
	Phase        proxyPhase       `json:"phase"`
	Image        string           `json:"image"`
	ImageSizeMiB int              `json:"image_size_mib"`
	Proxy        proxyPhaseTotals `json:"proxy"`
	Gantry       gantryMetrics    `json:"gantry"`
	Job          jobObservation   `json:"job"`
	RecordedAt   time.Time        `json:"recorded_at"`
}

type benchmarkComparison struct {
	RunID                    string                 `json:"run_id"`
	Baseline                 phaseResult            `json:"baseline"`
	GantryCold               phaseResult            `json:"gantry_cold"`
	OriginByteReduction      float64                `json:"origin_byte_reduction"`
	OriginRequestReduction   float64                `json:"origin_request_reduction"`
	P50StartLatencyReduction float64                `json:"p50_start_latency_reduction"`
	P95StartLatencyReduction float64                `json:"p95_start_latency_reduction"`
	Checks                   map[string]resultCheck `json:"checks"`
	Passed                   bool                   `json:"passed"`
}

type resultCheck struct {
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

func (b *benchmark) fetchProxyTotals(ctx context.Context, state benchmarkState, phase proxyPhase) (proxyPhaseTotals, error) {
	rawPath := fmt.Sprintf(
		"/api/v1/namespaces/%s/services/http:acr-origin-proxy:9090/proxy/debug/summary",
		b.config.Namespace,
	)

	output, err := b.commands.Run(ctx, nil, "kubectl", "get", "--raw", rawPath)
	if err != nil {
		return proxyPhaseTotals{}, err
	}

	var summary proxySummary
	if err := json.Unmarshal(output, &summary); err != nil {
		return proxyPhaseTotals{}, fmt.Errorf("decode proxy summary: %w", err)
	}

	if summary.RunID != state.RunID {
		return proxyPhaseTotals{}, fmt.Errorf("proxy run ID is %q, want %q", summary.RunID, state.RunID)
	}

	totals, ok := summary.Totals.ByPhase[phase]
	if !ok {
		return proxyPhaseTotals{}, fmt.Errorf("proxy summary has no totals for phase %q", phase)
	}

	return totals, nil
}

func (b *benchmark) gantryRevision(ctx context.Context) (string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.GantryNamespace,
		"get", "daemonset", b.config.GantryDaemonSet,
		"-o", "jsonpath={.status.updateRevision}",
	)
	if err != nil {
		return "", err
	}

	revision := strings.TrimSpace(string(output))
	if revision == "" {
		return "", fmt.Errorf("gantry DaemonSet has no updateRevision") //nolint:staticcheck // Kubernetes field name is updateRevision.
	}

	return revision, nil
}

func (b *benchmark) waitForGantryRevisionScrape(ctx context.Context, revision string) error {
	deadline := time.Now().Add(2 * time.Minute)

	for {
		query := fmt.Sprintf(
			`count(gantry_storage_mode_info{namespace=%q,mode="containerd",gantry_benchmark="true",controller_revision_hash=%q})`,
			b.config.GantryNamespace,
			revision,
		)

		count, err := b.queryPrometheus(ctx, query)
		if err == nil && int(count) == b.config.NodeCount {
			return nil
		}

		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("gantry revision %s metrics were not scraped: %w", revision, err)
			}

			return fmt.Errorf("prometheus reports revision %s metrics for %.0f/%d Gantry pods", revision, count, b.config.NodeCount)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *benchmark) fetchGantryRevisionMetrics(ctx context.Context, revision string) (gantryMetrics, error) {
	originQuery := fmt.Sprintf(
		`sum(p2p_origin_pull_total{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	originPulls, err := b.queryPrometheus(ctx, originQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry origin pulls: %w", err)
	}

	peerQuery := fmt.Sprintf(
		`sum(p2p_peer_fetch_total{namespace=%q,outcome="hit",gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	peerHits, err := b.queryPrometheus(ctx, peerQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry peer hits: %w", err)
	}

	return gantryMetrics{OriginPulls: originPulls, PeerFetchHits: peerHits}, nil
}

func (b *benchmark) waitForGantryMetricDelta(ctx context.Context, revision string, before gantryMetrics) (gantryMetrics, error) {
	deadline := time.Now().Add(2 * time.Minute)

	for {
		after, err := b.fetchGantryRevisionMetrics(ctx, revision)
		if err == nil {
			delta := subtractGantryMetrics(after, before)
			if delta.PeerFetchHits > 0 {
				return delta, nil
			}
		}

		if time.Now().After(deadline) {
			if err != nil {
				return gantryMetrics{}, fmt.Errorf("gantry cold metrics were not scraped: %w", err)
			}

			return gantryMetrics{}, fmt.Errorf("gantry peer-hit counter did not increase during the cold phase")
		}

		select {
		case <-ctx.Done():
			return gantryMetrics{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func subtractGantryMetrics(after, before gantryMetrics) gantryMetrics {
	return gantryMetrics{
		OriginPulls:   nonNegativeDifference(after.OriginPulls, before.OriginPulls),
		PeerFetchHits: nonNegativeDifference(after.PeerFetchHits, before.PeerFetchHits),
	}
}

func nonNegativeDifference(after, before float64) float64 {
	if after < before {
		return 0
	}

	return after - before
}

func compareResults(config benchmarkConfig, baseline, gantry phaseResult) benchmarkComparison {
	comparison := benchmarkComparison{
		RunID:      baseline.RunID,
		Baseline:   baseline,
		GantryCold: gantry,
		Checks:     make(map[string]resultCheck),
	}
	comparison.OriginByteReduction = reduction(
		float64(baseline.Proxy.BytesUpstream),
		float64(gantry.Proxy.BytesUpstream),
	)
	comparison.OriginRequestReduction = reduction(
		float64(baseline.Proxy.RequestsCompleted),
		float64(gantry.Proxy.RequestsCompleted),
	)
	comparison.P50StartLatencyReduction = reduction(
		baseline.Job.PodStartLatency.P50Seconds,
		gantry.Job.PodStartLatency.P50Seconds,
	)
	comparison.P95StartLatencyReduction = reduction(
		baseline.Job.PodStartLatency.P95Seconds,
		gantry.Job.PodStartLatency.P95Seconds,
	)

	byteCheck := resultCheck{
		Passed: comparison.OriginByteReduction >= config.MinimumByteReduction,
		Message: fmt.Sprintf(
			"origin byte reduction %.2f%%, minimum %.2f%%",
			100*comparison.OriginByteReduction,
			100*config.MinimumByteReduction,
		),
	}
	comparison.Checks["origin_byte_reduction"] = byteCheck

	latencyRatio := ratio(
		gantry.Job.PodStartLatency.P95Seconds,
		baseline.Job.PodStartLatency.P95Seconds,
	)
	latencyCheck := resultCheck{
		Passed: latencyRatio <= config.MaximumLatencyRatio,
		Message: fmt.Sprintf(
			"Gantry P95/baseline P95 ratio %.3f, maximum %.3f",
			latencyRatio,
			config.MaximumLatencyRatio,
		),
	}
	comparison.Checks["p95_latency_ratio"] = latencyCheck

	peerCheck := resultCheck{
		Passed:  gantry.Gantry.PeerFetchHits > 0,
		Message: fmt.Sprintf("Gantry peer fetch hits %.0f, want greater than zero", gantry.Gantry.PeerFetchHits),
	}
	comparison.Checks["peer_activity"] = peerCheck

	comparison.Passed = byteCheck.Passed && latencyCheck.Passed && peerCheck.Passed

	return comparison
}

func reduction(baseline, candidate float64) float64 {
	if baseline <= 0 {
		return 0
	}

	return 1 - candidate/baseline
}

func ratio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 1e9
	}

	return numerator / denominator
}

func (b *benchmark) writeJSONArtifact(runID, filename string, value any) error {
	directory := filepath.Join(b.config.StateRoot, runID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(directory, filename), append(encoded, '\n'), 0o640)
}

func (b *benchmark) writeComparisonArtifacts(comparison benchmarkComparison) error {
	if err := b.writeJSONArtifact(comparison.RunID, "comparison.json", comparison); err != nil {
		return err
	}

	baselineDigestRequests := digestRequests(comparison.Baseline.Proxy)
	gantryDigestRequests := digestRequests(comparison.GantryCold.Proxy)
	markdown := fmt.Sprintf(`# Gantry benchmark %s

| Metric | Baseline | Gantry cold | Reduction |
| --- | ---: | ---: | ---: |
| ACR upstream bytes | %d | %d | %.2f%% |
| Proxy requests | %d | %d | %.2f%% |
| Digest requests | %d | %d | %.2f%% |
| Pod start P50 | %.3fs | %.3fs | %.2f%% |
| Pod start P95 | %.3fs | %.3fs | %.2f%% |
| Gantry origin pulls | 0 | %.0f | n/a |
| Gantry peer hits | 0 | %.0f | n/a |

Result: **%s**

`,
		comparison.RunID,
		comparison.Baseline.Proxy.BytesUpstream,
		comparison.GantryCold.Proxy.BytesUpstream,
		100*comparison.OriginByteReduction,
		comparison.Baseline.Proxy.RequestsCompleted,
		comparison.GantryCold.Proxy.RequestsCompleted,
		100*comparison.OriginRequestReduction,
		baselineDigestRequests,
		gantryDigestRequests,
		100*reduction(float64(baselineDigestRequests), float64(gantryDigestRequests)),
		comparison.Baseline.Job.PodStartLatency.P50Seconds,
		comparison.GantryCold.Job.PodStartLatency.P50Seconds,
		100*comparison.P50StartLatencyReduction,
		comparison.Baseline.Job.PodStartLatency.P95Seconds,
		comparison.GantryCold.Job.PodStartLatency.P95Seconds,
		100*comparison.P95StartLatencyReduction,
		comparison.GantryCold.Gantry.OriginPulls,
		comparison.GantryCold.Gantry.PeerFetchHits,
		strings.ToUpper(map[bool]string{true: "pass", false: "fail"}[comparison.Passed]),
	)

	return os.WriteFile(
		filepath.Join(b.config.StateRoot, comparison.RunID, "comparison.md"),
		[]byte(markdown),
		0o640,
	)
}

func digestRequests(totals proxyPhaseTotals) uint64 {
	return totals.ByPathClass["blob"].Requests + totals.ByPathClass["manifest_by_digest"].Requests
}
