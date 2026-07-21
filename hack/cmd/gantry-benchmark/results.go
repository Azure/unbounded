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

type originMetrics struct {
	ACR            acrPullMetrics `json:"acr"`
	EstimatedBytes uint64         `json:"estimated_bytes"`
	EstimateMethod string         `json:"estimate_method"`
}

type gantryMetrics struct {
	OriginPulls              float64            `json:"origin_pulls"`
	OriginPullSuccesses      float64            `json:"origin_pull_successes"`
	OriginLayerPullSuccesses float64            `json:"origin_layer_pull_successes"`
	OriginFailures           map[string]float64 `json:"origin_failures"`
	PeerFetchHits            float64            `json:"peer_fetch_hits"`
	PeerFetchOutcomes        map[string]float64 `json:"peer_fetch_outcomes"`
}

type phaseResult struct {
	RunID        string             `json:"run_id"`
	Phase        benchmarkPhase     `json:"phase"`
	Image        string             `json:"image"`
	ImageSizeMiB int                `json:"image_size_mib"`
	Origin       originMetrics      `json:"origin"`
	Kubelet      kubeletPullMetrics `json:"kubelet"`
	Issues       pullIssues         `json:"issues"`
	Gantry       gantryMetrics      `json:"gantry"`
	Job          jobObservation     `json:"job"`
	RecordedAt   time.Time          `json:"recorded_at"`
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
	Gating  bool   `json:"gating"`
	Message string `json:"message"`
}

func (b *benchmark) gantryRevision(ctx context.Context) (string, error) {
	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.GantryNamespace,
		"get", "pods", "-l", "app.kubernetes.io/name="+b.config.GantryDaemonSet,
		"-o", "json",
	)
	if err != nil {
		return "", err
	}

	var pods struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(output, &pods); err != nil {
		return "", fmt.Errorf("decode Gantry pods: %w", err)
	}

	revisionCounts := make(map[string]int)

	for _, pod := range pods.Items {
		ready := false

		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true

				break
			}
		}

		if !ready {
			continue
		}

		revision := strings.TrimSpace(pod.Metadata.Labels["controller-revision-hash"])
		if revision == "" {
			return "", fmt.Errorf("ready Gantry pod %s has no controller-revision-hash label", pod.Metadata.Name)
		}

		revisionCounts[revision]++
	}

	if len(revisionCounts) != 1 {
		return "", fmt.Errorf("ready Gantry pods span %d controller revisions, want exactly 1", len(revisionCounts))
	}

	for revision, count := range revisionCounts {
		if count != b.config.NodeCount {
			return "", fmt.Errorf("ready Gantry pods for revision %s = %d, want %d", revision, count, b.config.NodeCount)
		}

		return revision, nil
	}

	return "", fmt.Errorf("no ready Gantry pods found")
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

	originPulls, err := b.queryPrometheusOrZero(ctx, originQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry origin pulls: %w", err)
	}

	originSuccessQuery := fmt.Sprintf(
		`sum(p2p_origin_pull_success_total{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	originPullSuccesses, err := b.queryPrometheusOrZero(ctx, originSuccessQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query successful Gantry origin pulls: %w", err)
	}

	originLayerSuccessQuery := fmt.Sprintf(
		`sum(p2p_origin_pull_success_total{namespace=%q,kind="layer",gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	originLayerPullSuccesses, err := b.queryPrometheusOrZero(ctx, originLayerSuccessQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query successful Gantry origin layer pulls: %w", err)
	}

	peerFetchOutcomes := make(map[string]float64)

	for _, outcome := range []string{"busy", "error", "hit", "notfound", "stall", "unavailable"} {
		query := fmt.Sprintf(
			`sum(p2p_peer_fetch_total{namespace=%q,outcome=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
			b.config.GantryNamespace,
			outcome,
			revision,
		)

		value, err := b.queryPrometheusOrZero(ctx, query)
		if err != nil {
			return gantryMetrics{}, fmt.Errorf("query Gantry peer fetch outcome %s: %w", outcome, err)
		}

		peerFetchOutcomes[outcome] = value
	}

	originFailures := make(map[string]float64)

	for _, failure := range []struct {
		key   string
		label string
	}{
		{key: "auth", label: "auth"},
		{key: "not_found", label: "not_found"},
		{key: "rate_limited", label: "rate_limited"},
		{key: "transient", label: "transient"},
		{key: "unspecified", label: ""},
	} {
		query := fmt.Sprintf(
			`sum(p2p_origin_pull_failure_total{namespace=%q,class=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
			b.config.GantryNamespace,
			failure.label,
			revision,
		)

		value, err := b.queryPrometheusOrZero(ctx, query)
		if err != nil {
			return gantryMetrics{}, fmt.Errorf("query Gantry origin failure class %s: %w", failure.key, err)
		}

		originFailures[failure.key] = value
	}

	return gantryMetrics{
		OriginPulls:              originPulls,
		OriginPullSuccesses:      originPullSuccesses,
		OriginLayerPullSuccesses: originLayerPullSuccesses,
		OriginFailures:           originFailures,
		PeerFetchHits:            peerFetchOutcomes["hit"],
		PeerFetchOutcomes:        peerFetchOutcomes,
	}, nil
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
		OriginPulls:              nonNegativeDifference(after.OriginPulls, before.OriginPulls),
		OriginPullSuccesses:      nonNegativeDifference(after.OriginPullSuccesses, before.OriginPullSuccesses),
		OriginLayerPullSuccesses: nonNegativeDifference(after.OriginLayerPullSuccesses, before.OriginLayerPullSuccesses),
		OriginFailures:           subtractMetricMap(after.OriginFailures, before.OriginFailures),
		PeerFetchHits:            nonNegativeDifference(after.PeerFetchHits, before.PeerFetchHits),
		PeerFetchOutcomes:        subtractMetricMap(after.PeerFetchOutcomes, before.PeerFetchOutcomes),
	}
}

func subtractMetricMap(after, before map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(after))
	for key, value := range after {
		result[key] = nonNegativeDifference(value, before[key])
	}

	return result
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
		float64(baseline.Origin.EstimatedBytes),
		float64(gantry.Origin.EstimatedBytes),
	)
	comparison.OriginRequestReduction = reduction(
		float64(baseline.Origin.ACR.Successful),
		float64(gantry.Origin.ACR.Successful),
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
		Gating: true,
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
		Gating: false,
		Message: fmt.Sprintf(
			"informational: Gantry P95/baseline P95 ratio %.3f, reference maximum %.3f",
			latencyRatio,
			config.MaximumLatencyRatio,
		),
	}
	comparison.Checks["p95_latency_ratio"] = latencyCheck

	peerCheck := resultCheck{
		Passed:  gantry.Gantry.PeerFetchHits > 0,
		Gating:  true,
		Message: fmt.Sprintf("Gantry peer fetch hits %.0f, want greater than zero", gantry.Gantry.PeerFetchHits),
	}
	comparison.Checks["peer_activity"] = peerCheck

	comparison.Passed = byteCheck.Passed && peerCheck.Passed

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

func (b *benchmark) regenerateComparison(runID string) error {
	if !strings.HasPrefix(runID, "run-") || filepath.Base(runID) != runID {
		return fmt.Errorf("invalid benchmark run ID %q", runID)
	}

	readPhase := func(filename string) (phaseResult, error) {
		raw, err := os.ReadFile(filepath.Join(b.config.StateRoot, runID, filename))
		if err != nil {
			return phaseResult{}, fmt.Errorf("read %s: %w", filename, err)
		}

		var result phaseResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return phaseResult{}, fmt.Errorf("decode %s: %w", filename, err)
		}

		return result, nil
	}

	baseline, err := readPhase("baseline.json")
	if err != nil {
		return err
	}

	gantry, err := readPhase("gantry-cold.json")
	if err != nil {
		return err
	}

	if baseline.RunID != runID || gantry.RunID != runID {
		return fmt.Errorf("phase artifact run IDs do not match %q", runID)
	}

	return b.writeComparisonArtifacts(compareResults(b.config, baseline, gantry))
}

func (b *benchmark) writeComparisonArtifacts(comparison benchmarkComparison) error {
	if err := b.writeJSONArtifact(comparison.RunID, "comparison.json", comparison); err != nil {
		return err
	}

	var markdown strings.Builder

	fmt.Fprintf(&markdown, "# Gantry benchmark %s\n\n", comparison.RunID)
	markdown.WriteString("| Metric | Baseline | Gantry cold | Reduction |\n")
	markdown.WriteString("| --- | ---: | ---: | ---: |\n")
	fmt.Fprintf(&markdown, "| Estimated origin bytes | %d | %d | %.2f%% |\n", comparison.Baseline.Origin.EstimatedBytes, comparison.GantryCold.Origin.EstimatedBytes, 100*comparison.OriginByteReduction)
	fmt.Fprintf(&markdown, "| ACR total pull count | %d | %d | %.2f%% |\n", comparison.Baseline.Origin.ACR.Total, comparison.GantryCold.Origin.ACR.Total, 100*reduction(float64(comparison.Baseline.Origin.ACR.Total), float64(comparison.GantryCold.Origin.ACR.Total)))
	fmt.Fprintf(&markdown, "| ACR successful pull count | %d | %d | %.2f%% |\n", comparison.Baseline.Origin.ACR.Successful, comparison.GantryCold.Origin.ACR.Successful, 100*comparison.OriginRequestReduction)
	fmt.Fprintf(&markdown, "| ACR unsuccessful pull count | %d | %d | n/a |\n", comparison.Baseline.Origin.ACR.Failed, comparison.GantryCold.Origin.ACR.Failed)
	fmt.Fprintf(&markdown, "| Kubelet pull operations | %.0f | %.0f | n/a |\n", comparison.Baseline.Kubelet.Operations, comparison.GantryCold.Kubelet.Operations)
	fmt.Fprintf(&markdown, "| Kubelet pull errors | %.0f | %.0f | n/a |\n", comparison.Baseline.Kubelet.Errors, comparison.GantryCold.Kubelet.Errors)
	fmt.Fprintf(&markdown, "| Kubernetes warning events | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, ""), issueCount(comparison.GantryCold.Issues, ""))
	fmt.Fprintf(&markdown, "| HTTP 429 markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "http_429"), issueCount(comparison.GantryCold.Issues, "http_429"))
	fmt.Fprintf(&markdown, "| HTTP 5xx markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "http_5xx"), issueCount(comparison.GantryCold.Issues, "http_5xx"))
	fmt.Fprintf(&markdown, "| ACR egress-limit markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "acr_egress_limit"), issueCount(comparison.GantryCold.Issues, "acr_egress_limit"))
	fmt.Fprintf(&markdown, "| Authentication markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "auth"), issueCount(comparison.GantryCold.Issues, "auth"))
	fmt.Fprintf(&markdown, "| Timeout markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "timeout"), issueCount(comparison.GantryCold.Issues, "timeout"))
	fmt.Fprintf(&markdown, "| Connection-refused markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "connection_refused"), issueCount(comparison.GantryCold.Issues, "connection_refused"))
	fmt.Fprintf(&markdown, "| Connection-reset markers | %s | %s | n/a |\n", issueCount(comparison.Baseline.Issues, "connection_reset"), issueCount(comparison.GantryCold.Issues, "connection_reset"))
	fmt.Fprintf(&markdown, "| Kubelet average pull duration | %.3fs | %.3fs | n/a |\n", comparison.Baseline.Kubelet.AverageDurationSeconds, comparison.GantryCold.Kubelet.AverageDurationSeconds)
	fmt.Fprintf(&markdown, "| Pod start P50 (informational) | %.3fs | %.3fs | %.2f%% |\n", comparison.Baseline.Job.PodStartLatency.P50Seconds, comparison.GantryCold.Job.PodStartLatency.P50Seconds, 100*comparison.P50StartLatencyReduction)
	fmt.Fprintf(&markdown, "| Pod start P95 (informational) | %.3fs | %.3fs | %.2f%% |\n", comparison.Baseline.Job.PodStartLatency.P95Seconds, comparison.GantryCold.Job.PodStartLatency.P95Seconds, 100*comparison.P95StartLatencyReduction)
	fmt.Fprintf(&markdown, "| Gantry origin pulls | 0 | %.0f | n/a |\n", comparison.GantryCold.Gantry.OriginPulls)
	fmt.Fprintf(&markdown, "| Gantry successful origin layer pulls | 0 | %.0f | n/a |\n", comparison.GantryCold.Gantry.OriginLayerPullSuccesses)
	fmt.Fprintf(&markdown, "| Gantry origin rate-limit failures | 0 | %s | n/a |\n", metricCountString(comparison.GantryCold.Gantry.OriginFailures, "rate_limited"))
	fmt.Fprintf(&markdown, "| Gantry origin transient failures | 0 | %s | n/a |\n", metricCountString(comparison.GantryCold.Gantry.OriginFailures, "transient"))
	fmt.Fprintf(&markdown, "| Gantry peer hits | 0 | %.0f | n/a |\n", comparison.GantryCold.Gantry.PeerFetchHits)
	fmt.Fprintf(&markdown, "| Gantry peer busy outcomes | 0 | %s | n/a |\n", metricCountString(comparison.GantryCold.Gantry.PeerFetchOutcomes, "busy"))
	fmt.Fprintf(&markdown, "| Gantry peer stall outcomes | 0 | %s | n/a |\n", metricCountString(comparison.GantryCold.Gantry.PeerFetchOutcomes, "stall"))
	fmt.Fprintf(&markdown, "| Gantry peer not-found outcomes | 0 | %s | n/a |\n", metricCountString(comparison.GantryCold.Gantry.PeerFetchOutcomes, "notfound"))
	fmt.Fprintf(&markdown, "| Gantry peer unavailable outcomes | 0 | %s | n/a |\n", metricCountString(comparison.GantryCold.Gantry.PeerFetchOutcomes, "unavailable"))

	markdown.WriteString("\nOrigin bytes are estimates. ACR counts are registry-wide one-minute Azure Monitor metrics for each phase window.\n")
	markdown.WriteString("Warning markers are bounded, may overlap, and intentionally omit raw event messages because ACR errors can contain signed URLs.\n")
	markdown.WriteString("Latency is informational and does not affect the gating result.\n\n")
	fmt.Fprintf(&markdown, "Gating result: **%s**\n\n", strings.ToUpper(map[bool]string{true: "pass", false: "fail"}[comparison.Passed]))

	return os.WriteFile(
		filepath.Join(b.config.StateRoot, comparison.RunID, "comparison.md"),
		[]byte(markdown.String()),
		0o640,
	)
}

func issueCount(issues pullIssues, marker string) string {
	if !issues.Captured {
		return "n/a"
	}

	if marker == "" {
		return fmt.Sprintf("%d", issues.WarningEvents)
	}

	return fmt.Sprintf("%d", issues.Markers[marker])
}

func metricCountString(values map[string]float64, key string) string {
	if values == nil {
		return "n/a"
	}

	return fmt.Sprintf("%.0f", values[key])
}
