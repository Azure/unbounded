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
	OriginBytes   float64 `json:"origin_bytes"`
	PeerFetchHits float64 `json:"peer_fetch_hits"`
	// OriginLayerPulls counts layer blobs Gantry streamed to completion from the
	// origin registry. It remains a diagnostic alongside the byte counter.
	OriginLayerPulls float64 `json:"origin_layer_pulls"`
	// OriginFallbacks counts NF5 direct-origin fallback pulls. The direct byte
	// counter includes these bytes, but a non-zero value still means the peer
	// distribution path exhausted and the run should not pass its health gates.
	OriginFallbacks float64 `json:"origin_fallbacks"`
}

// originByteSource records how a phase's origin-byte figure was obtained so the
// artifact never presents a derived number as a measured one.
type originByteSource string

const (
	// originBytesProxy is measured by the counting proxy.
	originBytesProxy originByteSource = "proxy_upstream_bytes"
	// originBytesGantryMeasured is measured at Gantry's upstream response-body
	// boundary and includes partial failed transfers and retries.
	originBytesGantryMeasured originByteSource = "gantry_origin_bytes_total"
	// originBytesPrivateEndpoint is measured at the ACR Private Link boundary.
	originBytesPrivateEndpoint originByteSource = "Microsoft.Network/privateEndpoints/PEBytesIn"
	// originBytesAnalyticBaseline assumes every completed pull pod fetched the
	// whole image from ACR, which is what a baseline pull with no peer sharing
	// does.
	originBytesAnalyticBaseline originByteSource = "completed_pods_x_image_size"
)

type phaseResult struct {
	RunID        string                     `json:"run_id"`
	Phase        proxyPhase                 `json:"phase"`
	Image        string                     `json:"image"`
	ImageSizeMiB int                        `json:"image_size_mib"`
	PayloadSHA   string                     `json:"workload_payload_sha256,omitempty"`
	Proxy        proxyPhaseTotals           `json:"proxy"`
	Gantry       gantryMetrics              `json:"gantry"`
	GantryPeer   gantryPeerPhaseMeasurement `json:"gantry_peer"`
	Azure        azurePhaseMeasurement      `json:"azure"`
	Job          jobObservation             `json:"job"`
	// OriginBytes is the phase's ACR traffic as attributed by OriginBytesSource.
	OriginBytes             uint64           `json:"origin_bytes"`
	OriginBytesSource       originByteSource `json:"origin_bytes_source"`
	PodStartupLatency       latencySummary   `json:"pod_startup_latency"`
	PodStartupLatencySource string           `json:"pod_startup_latency_source"`
	RecordedAt              time.Time        `json:"recorded_at"`
}

type benchmarkComparison struct {
	RunID                           string                 `json:"run_id"`
	Mode                            benchmarkMode          `json:"mode"`
	Baseline                        phaseResult            `json:"baseline"`
	GantryCold                      phaseResult            `json:"gantry_cold"`
	OriginByteReduction             float64                `json:"origin_byte_reduction"`
	OriginRequestReduction          float64                `json:"origin_request_reduction"`
	OriginRequestReductionAvailable bool                   `json:"origin_request_reduction_available"`
	OriginRequestSource             string                 `json:"origin_request_source,omitempty"`
	P50StartLatencyReduction        float64                `json:"p50_start_latency_reduction"`
	P95StartLatencyReduction        float64                `json:"p95_start_latency_reduction"`
	Checks                          map[string]resultCheck `json:"checks"`
	Passed                          bool                   `json:"passed"`
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

	peerQuery := fmt.Sprintf(
		`sum(p2p_peer_fetch_total{namespace=%q,outcome="hit",gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	peerHits, err := b.queryPrometheusOrZero(ctx, peerQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry peer hits: %w", err)
	}

	originBytesQuery := fmt.Sprintf(
		`sum(gantry_origin_bytes_total{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	originBytes, err := b.queryPrometheusOrZero(ctx, originBytesQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry origin bytes: %w", err)
	}

	// Successful layer pulls, not started pulls: a started pull that failed
	// transferred no complete blob and must not be billed as origin bytes.
	originLayerQuery := fmt.Sprintf(
		`sum(p2p_origin_pull_success_total{namespace=%q,kind="layer",gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	originLayerPulls, err := b.queryPrometheusOrZero(ctx, originLayerQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry origin layer pulls: %w", err)
	}

	fallbackQuery := fmt.Sprintf(
		`sum(p2p_origin_fallback_total{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	originFallbacks, err := b.queryPrometheusOrZero(ctx, fallbackQuery)
	if err != nil {
		return gantryMetrics{}, fmt.Errorf("query Gantry origin fallbacks: %w", err)
	}

	return gantryMetrics{
		OriginPulls:      originPulls,
		OriginBytes:      originBytes,
		PeerFetchHits:    peerHits,
		OriginLayerPulls: originLayerPulls,
		OriginFallbacks:  originFallbacks,
	}, nil
}

func (b *benchmark) waitForGantryMetricDelta(ctx context.Context, revision string, before gantryMetrics) (gantryMetrics, error) {
	return b.waitForGantryMetricSettlement(ctx, revision, before, true)
}

func (b *benchmark) waitForGantryMetricSettlement(
	ctx context.Context,
	revision string,
	before gantryMetrics,
	requirePeerActivity bool,
) (gantryMetrics, error) {
	deadline := time.Now().Add(2 * time.Minute)
	settlement := gantryMetricSettlement{window: 20 * time.Second, requirePeerActivity: requirePeerActivity}

	inFlightQuery := fmt.Sprintf(
		`sum(p2p_in_flight_pulls{namespace=%q,gantry_benchmark="true",controller_revision_hash=%q})`,
		b.config.GantryNamespace,
		revision,
	)

	var (
		lastDelta    gantryMetrics
		lastInFlight float64
		lastErr      error
	)

	for {
		after, err := b.fetchGantryRevisionMetrics(ctx, revision)
		if err == nil {
			inFlight, queryErr := b.queryPrometheusOrZero(ctx, inFlightQuery)
			if queryErr == nil {
				lastDelta, lastErr = subtractGantryMetrics(after, before)
				if lastErr != nil {
					settlement.initialized = false
					settlement.stableSince = time.Time{}

					continue
				}

				lastInFlight = inFlight

				if settlement.Observe(time.Now(), lastDelta, inFlight) {
					return lastDelta, nil
				}
			} else {
				lastErr = queryErr
			}
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return gantryMetrics{}, fmt.Errorf("gantry cold metrics did not settle: %w", lastErr)
			}

			if requirePeerActivity && lastDelta.PeerFetchHits <= 0 {
				return gantryMetrics{}, fmt.Errorf("gantry peer-hit counter did not increase during the cold phase")
			}

			return gantryMetrics{}, fmt.Errorf(
				"gantry metrics did not remain stable for %s before timeout: in_flight=%.0f delta=%+v",
				settlement.window,
				lastInFlight,
				lastDelta,
			)
		}

		select {
		case <-ctx.Done():
			return gantryMetrics{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// gantryMetricSettlement waits for counters to remain unchanged while no
// background origin pulls are in flight. The stable window spans two 10-second
// PodMonitor scrape intervals, preventing a completed workload Job from racing
// with late prefetches whose counters have not reached Prometheus yet.
type gantryMetricSettlement struct {
	window              time.Duration
	requirePeerActivity bool
	stableSince         time.Time
	last                gantryMetrics
	initialized         bool
}

func (s *gantryMetricSettlement) Observe(now time.Time, current gantryMetrics, inFlight float64) bool {
	if (s.requirePeerActivity && current.PeerFetchHits <= 0) || inFlight > 0 {
		s.initialized = false
		s.stableSince = time.Time{}

		return false
	}

	if !s.initialized || s.last != current {
		s.last = current
		s.stableSince = now
		s.initialized = true

		return false
	}

	return now.Sub(s.stableSince) >= s.window
}

func subtractGantryMetrics(after, before gantryMetrics) (gantryMetrics, error) {
	fields := []struct {
		name   string
		after  float64
		before float64
	}{
		{name: "origin_pulls", after: after.OriginPulls, before: before.OriginPulls},
		{name: "origin_bytes", after: after.OriginBytes, before: before.OriginBytes},
		{name: "peer_fetch_hits", after: after.PeerFetchHits, before: before.PeerFetchHits},
		{name: "origin_layer_pulls", after: after.OriginLayerPulls, before: before.OriginLayerPulls},
		{name: "origin_fallbacks", after: after.OriginFallbacks, before: before.OriginFallbacks},
	}

	for _, field := range fields {
		if field.after < field.before {
			return gantryMetrics{}, fmt.Errorf(
				"gantry counter %s decreased during phase: before=%.0f after=%.0f",
				field.name,
				field.before,
				field.after,
			)
		}
	}

	return gantryMetrics{
		OriginPulls:      after.OriginPulls - before.OriginPulls,
		OriginBytes:      after.OriginBytes - before.OriginBytes,
		PeerFetchHits:    after.PeerFetchHits - before.PeerFetchHits,
		OriginLayerPulls: after.OriginLayerPulls - before.OriginLayerPulls,
		OriginFallbacks:  after.OriginFallbacks - before.OriginFallbacks,
	}, nil
}

// deriveOriginBytes attributes a phase's ACR traffic. Proxy mode measures it
// directly. Direct mode's baseline remains analytic because it bypasses Gantry;
// the Gantry-cold phase uses bytes measured at Gantry's upstream response-body
// boundary, including partial failed transfers and retries.
func deriveOriginBytes(config benchmarkConfig, phase proxyPhase, proxy proxyPhaseTotals, gantry gantryMetrics, job jobObservation) (uint64, originByteSource) {
	if config.usesProxy() {
		return proxy.BytesUpstream, originBytesProxy
	}

	if phase == proxyPhaseBaseline {
		return uint64(len(job.Nodes)) * config.imageBytes(), originBytesAnalyticBaseline
	}

	return uint64(gantry.OriginBytes), originBytesGantryMeasured
}

func compareResults(config benchmarkConfig, baseline, gantry phaseResult) benchmarkComparison {
	comparison := benchmarkComparison{
		RunID:      baseline.RunID,
		Mode:       config.Mode,
		Baseline:   baseline,
		GantryCold: gantry,
		Checks:     make(map[string]resultCheck),
	}

	comparison.OriginByteReduction = reduction(
		float64(baseline.OriginBytes),
		float64(gantry.OriginBytes),
	)
	if baseline.Azure.Complete && gantry.Azure.Complete {
		comparison.OriginRequestReduction = reduction(
			float64(baseline.Azure.ACR.SuccessfulPullCount),
			float64(gantry.Azure.ACR.SuccessfulPullCount),
		)
		comparison.OriginRequestReductionAvailable = true
		comparison.OriginRequestSource = "ContainerRegistryRepositoryEvents"
	} else if config.usesProxy() {
		comparison.OriginRequestReduction = reduction(
			float64(baseline.Proxy.RequestsCompleted),
			float64(gantry.Proxy.RequestsCompleted),
		)
		comparison.OriginRequestReductionAvailable = true
		comparison.OriginRequestSource = "acr-origin-proxy"
	}

	comparison.P50StartLatencyReduction = reduction(
		phaseStartupLatency(baseline).P50Seconds,
		phaseStartupLatency(gantry).P50Seconds,
	)
	comparison.P95StartLatencyReduction = reduction(
		phaseStartupLatency(baseline).P95Seconds,
		phaseStartupLatency(gantry).P95Seconds,
	)

	byteCheck := resultCheck{
		Passed: comparison.OriginByteReduction >= config.MinimumByteReduction,
		Message: fmt.Sprintf(
			"origin byte reduction %.2f%%, minimum %.2f%% (baseline source %s, Gantry source %s)",
			100*comparison.OriginByteReduction,
			100*config.MinimumByteReduction,
			baseline.OriginBytesSource,
			gantry.OriginBytesSource,
		),
	}
	comparison.Checks["origin_byte_reduction"] = byteCheck

	latencyRatio := ratio(
		phaseStartupLatency(gantry).P95Seconds,
		phaseStartupLatency(baseline).P95Seconds,
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

	// Without a recorded source the byte figures are meaningless, and an unset
	// OriginBytes would otherwise read as a clean 0% reduction rather than as an
	// error. Fail loudly instead.
	recordedCheck := resultCheck{
		Passed: baseline.OriginBytesSource != "" && gantry.OriginBytesSource != "",
		Message: fmt.Sprintf(
			"origin byte sources baseline=%q Gantry=%q, want both recorded",
			baseline.OriginBytesSource,
			gantry.OriginBytesSource,
		),
	}
	comparison.Checks["origin_bytes_recorded"] = recordedCheck

	peerBytesCheck := resultCheck{
		Passed: baseline.GantryPeer.Complete && gantry.GantryPeer.Complete,
		Message: fmt.Sprintf(
			"Gantry peer byte measurements baseline=%t Gantry=%t",
			baseline.GantryPeer.Complete,
			gantry.GantryPeer.Complete,
		),
	}
	comparison.Checks["peer_bytes_recorded"] = peerBytesCheck

	comparison.Passed = byteCheck.Passed && latencyCheck.Passed && peerCheck.Passed && recordedCheck.Passed && peerBytesCheck.Passed

	if config.AzureTelemetry {
		azureCheck := resultCheck{
			Passed: baseline.Azure.Complete && gantry.Azure.Complete,
			Message: fmt.Sprintf(
				"Azure telemetry baseline=%t Gantry=%t",
				baseline.Azure.Complete,
				gantry.Azure.Complete,
			),
		}
		comparison.Checks["azure_telemetry_complete"] = azureCheck
		comparison.Passed = comparison.Passed && azureCheck.Passed
	}

	if !config.usesProxy() {
		// Direct mode's baseline remains analytic, so its routing assumption has
		// to be a gate rather than a footnote.

		// Gantry's node configurator routes every registry through the local
		// mirror via _default/hosts.toml. The baseline installs an explicit
		// direct-to-ACR host file to override it; if that override failed the
		// baseline silently ran through Gantry and the comparison is void.
		bypassCheck := resultCheck{
			Passed: baseline.Gantry.OriginPulls == 0 && baseline.Gantry.PeerFetchHits == 0,
			Message: fmt.Sprintf(
				"baseline Gantry activity origin_pulls=%.0f peer_hits=%.0f, want zero so the baseline bypassed Gantry",
				baseline.Gantry.OriginPulls,
				baseline.Gantry.PeerFetchHits,
			),
		}
		comparison.Checks["baseline_bypassed_gantry"] = bypassCheck

		// NF5 fallback bytes are included by gantry_origin_bytes_total, but a
		// fallback means peer distribution exhausted and the run is unhealthy.
		fallbackCheck := resultCheck{
			Passed: gantry.Gantry.OriginFallbacks == 0,
			Message: fmt.Sprintf(
				"Gantry origin fallback pulls %.0f, want zero for a healthy peer-distribution path",
				gantry.Gantry.OriginFallbacks,
			),
		}
		comparison.Checks["no_origin_fallback"] = fallbackCheck

		payloadCheck := resultCheck{
			Passed: baseline.PayloadSHA != "" && baseline.PayloadSHA == gantry.PayloadSHA,
			Message: fmt.Sprintf(
				"workload payload sha256 baseline=%q Gantry=%q, want the same non-empty fingerprint",
				baseline.PayloadSHA,
				gantry.PayloadSHA,
			),
		}
		comparison.Checks["same_workload_payload"] = payloadCheck

		comparison.Passed = comparison.Passed && bypassCheck.Passed && fallbackCheck.Passed && payloadCheck.Passed
	}

	return comparison
}

func phaseStartupLatency(phase phaseResult) latencySummary {
	if phase.PodStartupLatencySource != "" {
		return phase.PodStartupLatency
	}

	return phase.Job.PodStartLatency
}

func phaseStartupLatencySource(phase phaseResult) string {
	if phase.PodStartupLatencySource != "" {
		return phase.PodStartupLatencySource
	}

	return "kubernetes_pod_status"
}

func originByteMetricLabel(phase phaseResult) string {
	if phase.OriginBytesSource == originBytesPrivateEndpoint {
		return "ACR Private Endpoint bytes from ACR"
	}

	if phase.OriginBytesSource == originBytesProxy {
		return "Proxy-measured ACR upstream bytes"
	}

	return "ACR origin bytes"
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

	markdown := renderComparisonMarkdown(comparison)

	return os.WriteFile(
		filepath.Join(b.config.StateRoot, comparison.RunID, "comparison.md"),
		[]byte(markdown),
		0o640,
	)
}

func renderComparisonMarkdown(comparison benchmarkComparison) string {
	result := strings.ToUpper(map[bool]string{true: "pass", false: "fail"}[comparison.Passed])
	baselineLatency := phaseStartupLatency(comparison.Baseline)
	gantryLatency := phaseStartupLatency(comparison.GantryCold)

	if comparison.Mode == benchmarkModeDirect {
		requestReduction := "unavailable without Azure telemetry"
		baselinePulls := "n/a"
		gantryPulls := "n/a"

		if comparison.OriginRequestReductionAvailable {
			requestReduction = fmt.Sprintf("%.2f%%", 100*comparison.OriginRequestReduction)
			baselinePulls = fmt.Sprintf("%d", comparison.Baseline.Azure.ACR.SuccessfulPullCount)
			gantryPulls = fmt.Sprintf("%d", comparison.GantryCold.Azure.ACR.SuccessfulPullCount)
		}

		byteLabel := originByteMetricLabel(comparison.Baseline)

		return fmt.Sprintf(`# Gantry benchmark %s

Mode: **direct**

Shared workload payload: **%s**

- Baseline image: %s
- Gantry image: %s

Origin byte sources:

- Baseline: %s
- Gantry cold: %s

| Metric | Baseline | Gantry cold | Reduction |
| --- | ---: | ---: | ---: |
| %s | %d | %d | %.2f%% |
| ACR successful image pulls | %s | %s | %s |
| Pod start P50 | %.3fs | %.3fs | %.2f%% |
| Pod start P95 | %.3fs | %.3fs | %.2f%% |
| Gantry peer bytes served | %d | %d | n/a |
| Gantry origin pulls | 0 | %.0f | n/a |
| Gantry peer hits | 0 | %.0f | n/a |

Pod startup latency source: **%s / %s**

Result: **%s**

`,
			comparison.RunID,
			comparison.Baseline.PayloadSHA,
			comparison.Baseline.Image,
			comparison.GantryCold.Image,
			comparison.Baseline.OriginBytesSource,
			comparison.GantryCold.OriginBytesSource,
			byteLabel,
			comparison.Baseline.OriginBytes,
			comparison.GantryCold.OriginBytes,
			100*comparison.OriginByteReduction,
			baselinePulls,
			gantryPulls,
			requestReduction,
			baselineLatency.P50Seconds,
			gantryLatency.P50Seconds,
			100*comparison.P50StartLatencyReduction,
			baselineLatency.P95Seconds,
			gantryLatency.P95Seconds,
			100*comparison.P95StartLatencyReduction,
			comparison.Baseline.GantryPeer.Total,
			comparison.GantryCold.GantryPeer.Total,
			comparison.GantryCold.Gantry.OriginPulls,
			comparison.GantryCold.Gantry.PeerFetchHits,
			phaseStartupLatencySource(comparison.Baseline),
			phaseStartupLatencySource(comparison.GantryCold),
			result,
		)
	}

	baselineDigestRequests := digestRequests(comparison.Baseline.Proxy)
	gantryDigestRequests := digestRequests(comparison.GantryCold.Proxy)
	baselineRequests := comparison.Baseline.Proxy.RequestsCompleted
	gantryRequests := comparison.GantryCold.Proxy.RequestsCompleted
	requestMetricLabel := "Proxy requests"
	byteMetricLabel := originByteMetricLabel(comparison.Baseline)

	if comparison.OriginRequestSource == "ContainerRegistryRepositoryEvents" {
		baselineRequests = comparison.Baseline.Azure.ACR.SuccessfulPullCount
		gantryRequests = comparison.GantryCold.Azure.ACR.SuccessfulPullCount
		requestMetricLabel = "ACR successful image pulls"
	}

	return fmt.Sprintf(`# Gantry benchmark %s

| Metric | Baseline | Gantry cold | Reduction |
| --- | ---: | ---: | ---: |
| %s | %d | %d | %.2f%% |
| %s | %d | %d | %.2f%% |
| Digest requests | %d | %d | %.2f%% |
| Pod start P50 | %.3fs | %.3fs | %.2f%% |
| Pod start P95 | %.3fs | %.3fs | %.2f%% |
| Gantry peer bytes served | %d | %d | n/a |
| Gantry origin pulls | 0 | %.0f | n/a |
| Gantry peer hits | 0 | %.0f | n/a |

Image pull count source: **%s**

Pod startup latency source: **%s / %s**

Result: **%s**

`,
		comparison.RunID,
		byteMetricLabel,
		comparison.Baseline.OriginBytes,
		comparison.GantryCold.OriginBytes,
		100*comparison.OriginByteReduction,
		requestMetricLabel,
		baselineRequests,
		gantryRequests,
		100*comparison.OriginRequestReduction,
		baselineDigestRequests,
		gantryDigestRequests,
		100*reduction(float64(baselineDigestRequests), float64(gantryDigestRequests)),
		baselineLatency.P50Seconds,
		gantryLatency.P50Seconds,
		100*comparison.P50StartLatencyReduction,
		baselineLatency.P95Seconds,
		gantryLatency.P95Seconds,
		100*comparison.P95StartLatencyReduction,
		comparison.Baseline.GantryPeer.Total,
		comparison.GantryCold.GantryPeer.Total,
		comparison.GantryCold.Gantry.OriginPulls,
		comparison.GantryCold.Gantry.PeerFetchHits,
		comparison.OriginRequestSource,
		phaseStartupLatencySource(comparison.Baseline),
		phaseStartupLatencySource(comparison.GantryCold),
		result,
	)
}

func digestRequests(totals proxyPhaseTotals) uint64 {
	return totals.ByPathClass["blob"].Requests + totals.ByPathClass["manifest_by_digest"].Requests
}
