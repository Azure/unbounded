// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
)

// Direct mode must write an explicit direct-to-ACR hosts.toml for the baseline.
// Omitting the file is not equivalent: Gantry's node configurator owns
// /etc/containerd/certs.d/_default/hosts.toml and routes every registry through
// the local mirror, so an absent ACR-specific file would send the baseline
// through Gantry and void the comparison.
func TestRenderHostsDirectBaselineOverridesGantryDefault(t *testing.T) {
	state := benchmarkState{
		RunID:                  "run-1",
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}

	baseline, err := renderHosts(state, hostsModeBaseline)
	if err != nil {
		t.Fatalf("render baseline: %v", err)
	}

	if !strings.Contains(baseline, `server = "https://baseline.azurecr.io"`) ||
		!strings.Contains(baseline, `[host."https://baseline.azurecr.io"]`) {
		t.Fatalf("direct baseline must point containerd straight at ACR:\n%s", baseline)
	}

	if strings.Contains(baseline, "5002") || strings.Contains(baseline, "acr-origin-proxy") {
		t.Fatalf("direct baseline must not reference the counting proxy:\n%s", baseline)
	}

	if strings.Contains(baseline, "127.0.0.1") {
		t.Fatalf("direct baseline must not reference the local Gantry mirror:\n%s", baseline)
	}
}

// The Gantry phase renders identically in both modes: strict, no `server=`
// fall-through, so containerd can never bypass Gantry to the origin.
func TestRenderHostsDirectGantryPhaseIsStrict(t *testing.T) {
	state := benchmarkState{
		RunID:                  "run-1",
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}

	gantry, err := renderHosts(state, hostsModeGantry)
	if err != nil {
		t.Fatalf("render Gantry: %v", err)
	}

	if strings.Contains(gantry, "server =") ||
		!strings.Contains(gantry, `[host."http://127.0.0.1:5000"]`) ||
		strings.Contains(gantry, "gantry.azurecr.io") {
		t.Fatalf("unexpected direct-mode Gantry hosts.toml:\n%s", gantry)
	}
}

func TestRenderHostsDirectBaselineRequiresRegistry(t *testing.T) {
	state := benchmarkState{RunID: "run-1", Mode: benchmarkModeDirect}

	if _, err := renderHosts(state, hostsModeBaseline); err == nil {
		t.Fatalf("expected an error when the direct baseline has no ACR login server")
	}
}

func TestRenderHostsProxyBaselineRequiresClusterIP(t *testing.T) {
	state := benchmarkState{RunID: "run-1", Mode: benchmarkModeProxy}

	if _, err := renderHosts(state, hostsModeBaseline); err == nil {
		t.Fatalf("expected an error when the proxy baseline has no ClusterIP")
	}
}

func TestDeriveOriginBytes(t *testing.T) {
	// 8 GiB across 8 layers, matching the multi-layer benchmark shape.
	directConfig := benchmarkConfig{
		Mode:         benchmarkModeDirect,
		ImageSizeMiB: 8192,
		ImageLayers:  8,
	}
	proxyConfig := benchmarkConfig{
		Mode:         benchmarkModeProxy,
		ImageSizeMiB: 8192,
		ImageLayers:  8,
	}

	const gibibyte = 1024 * 1024 * 1024

	job := jobObservation{Nodes: []string{"node-a", "node-b", "node-c"}}

	proxyBytes, proxySource := deriveOriginBytes(
		proxyConfig,
		proxyPhaseBaseline,
		proxyPhaseTotals{BytesUpstream: 12345},
		gantryMetrics{},
		job,
	)
	if proxyBytes != 12345 || proxySource != originBytesProxy {
		t.Fatalf("proxy mode = (%d, %s), want (12345, %s)", proxyBytes, proxySource, originBytesProxy)
	}

	// Baseline: every completed pod pulls the whole 8 GiB image from ACR.
	baselineBytes, baselineSource := deriveOriginBytes(
		directConfig,
		proxyPhaseBaseline,
		proxyPhaseTotals{},
		gantryMetrics{},
		job,
	)
	if want := uint64(3 * 8 * gibibyte); baselineBytes != want {
		t.Fatalf("direct baseline bytes = %d, want %d", baselineBytes, want)
	}

	if baselineSource != originBytesAnalyticBaseline {
		t.Fatalf("direct baseline source = %s, want %s", baselineSource, originBytesAnalyticBaseline)
	}

	// Gantry cold: use the bytes measured at Gantry's upstream response-body
	// boundary. Layer counts remain diagnostic and do not drive accounting.
	gantryBytes, gantrySource := deriveOriginBytes(
		directConfig,
		proxyPhaseGantryCold,
		proxyPhaseTotals{},
		gantryMetrics{OriginBytes: 9 * gibibyte, OriginLayerPulls: 9},
		job,
	)
	if want := uint64(9 * gibibyte); gantryBytes != want {
		t.Fatalf("direct Gantry bytes = %d, want %d", gantryBytes, want)
	}

	if gantrySource != originBytesGantryMeasured {
		t.Fatalf("direct Gantry source = %s, want %s", gantrySource, originBytesGantryMeasured)
	}
}

func directComparisonConfig() benchmarkConfig {
	return benchmarkConfig{
		Mode:                 benchmarkModeDirect,
		ImageSizeMiB:         8192,
		ImageLayers:          8,
		MinimumByteReduction: 0.90,
		MaximumLatencyRatio:  1.0,
	}
}

func healthyDirectPhases() (phaseResult, phaseResult) {
	baseline := phaseResult{
		RunID:             "run-1",
		Phase:             proxyPhaseBaseline,
		PayloadSHA:        "sha256:shared-payload",
		OriginBytes:       1000,
		OriginBytesSource: originBytesAnalyticBaseline,
		GantryPeer:        gantryPeerPhaseMeasurement{Complete: true},
		Job: jobObservation{
			PodStartLatency: latencySummary{P50Seconds: 400, P95Seconds: 440},
		},
	}
	gantry := phaseResult{
		RunID:             "run-1",
		Phase:             proxyPhaseGantryCold,
		PayloadSHA:        "sha256:shared-payload",
		OriginBytes:       50,
		OriginBytesSource: originBytesGantryMeasured,
		Gantry:            gantryMetrics{OriginBytes: 50, PeerFetchHits: 620, OriginLayerPulls: 9},
		GantryPeer:        gantryPeerPhaseMeasurement{Complete: true, Total: 950},
		Job: jobObservation{
			PodStartLatency: latencySummary{P50Seconds: 49, P95Seconds: 66},
		},
	}

	return baseline, gantry
}

func TestCompareResultsDirectModePasses(t *testing.T) {
	baseline, gantry := healthyDirectPhases()

	comparison := compareResults(directComparisonConfig(), baseline, gantry)

	if comparison.Mode != benchmarkModeDirect {
		t.Fatalf("comparison mode = %s, want %s", comparison.Mode, benchmarkModeDirect)
	}

	if !comparison.Passed {
		t.Fatalf("expected a passing comparison, got checks %+v", comparison.Checks)
	}

	if comparison.OriginRequestReductionAvailable {
		t.Fatalf("direct mode must not report proxy-derived request reduction as available")
	}

	for _, name := range []string{"same_workload_payload", "baseline_bypassed_gantry", "no_origin_fallback"} {
		if _, ok := comparison.Checks[name]; !ok {
			t.Fatalf("direct mode must add the %q check, got %+v", name, comparison.Checks)
		}
	}
}

// If the baseline shows Gantry activity, the direct-to-ACR hosts.toml override
// did not take effect and the baseline was served through Gantry.
func TestCompareResultsDirectModeFailsWhenBaselineUsedGantry(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	baseline.Gantry.OriginPulls = 4

	comparison := compareResults(directComparisonConfig(), baseline, gantry)

	if comparison.Checks["baseline_bypassed_gantry"].Passed {
		t.Fatalf("expected the baseline bypass check to fail")
	}

	if comparison.Passed {
		t.Fatalf("expected the comparison to fail when the baseline ran through Gantry")
	}
}

// NF5 fallback pulls reach ACR outside the counted origin path, so the derived
// byte total would be an undercount.
func TestCompareResultsDirectModeFailsOnOriginFallback(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	gantry.Gantry.OriginFallbacks = 3

	comparison := compareResults(directComparisonConfig(), baseline, gantry)

	if comparison.Checks["no_origin_fallback"].Passed {
		t.Fatalf("expected the origin fallback check to fail")
	}

	if comparison.Passed {
		t.Fatalf("expected the comparison to fail when Gantry fell back to the origin")
	}
}

func TestCompareResultsDirectModeRejectsDifferentPayloads(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	gantry.PayloadSHA = "sha256:different-payload"

	comparison := compareResults(directComparisonConfig(), baseline, gantry)

	check := comparison.Checks["same_workload_payload"]
	if comparison.Passed || check.Passed {
		t.Fatalf("comparison passed with different payloads: %+v", check)
	}
}

func TestCompareResultsDirectModeAcceptsEquivalentRandomPayloadShape(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	baseline.PayloadSHA = "sha256:baseline-random"
	gantry.PayloadSHA = "sha256:fresh-gantry-random"
	baseline.ImageSizeMiB = 40960
	gantry.ImageSizeMiB = 40960
	baseline.ImageLayers = 40
	gantry.ImageLayers = 40
	baseline.WorkloadComparisonMode = workloadComparisonRandomShape
	gantry.WorkloadComparisonMode = workloadComparisonRandomShape

	comparison := compareResults(directComparisonConfig(), baseline, gantry)

	check := comparison.Checks["same_workload_payload"]
	if !comparison.Passed || !check.Passed {
		t.Fatalf("equivalent random workload shape did not pass: %+v", check)
	}
	if !strings.Contains(check.Message, "fingerprints intentionally differ") {
		t.Fatalf("workload check does not explain random equivalence: %q", check.Message)
	}

	gantry.ImageLayers = 39
	comparison = compareResults(directComparisonConfig(), baseline, gantry)
	if comparison.Checks["same_workload_payload"].Passed {
		t.Fatal("comparison passed with different random payload layer counts")
	}
}

// Proxy mode must keep its original gate set so existing runs are unchanged.
func TestCompareResultsProxyModeOmitsDirectChecks(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	baseline.OriginBytesSource = originBytesProxy
	gantry.OriginBytesSource = originBytesProxy
	baseline.Proxy.RequestsCompleted = 100
	gantry.Proxy.RequestsCompleted = 20

	config := directComparisonConfig()
	config.Mode = benchmarkModeProxy

	comparison := compareResults(config, baseline, gantry)

	for _, name := range []string{"same_workload_payload", "baseline_bypassed_gantry", "no_origin_fallback"} {
		if _, ok := comparison.Checks[name]; ok {
			t.Fatalf("proxy mode must not add the %q check", name)
		}
	}

	if !comparison.Passed {
		t.Fatalf("expected a passing proxy comparison, got checks %+v", comparison.Checks)
	}

	if !comparison.OriginRequestReductionAvailable || comparison.OriginRequestReduction != 0.8 {
		t.Fatalf(
			"proxy request reduction = %.3f available=%t, want 0.8 and available",
			comparison.OriginRequestReduction,
			comparison.OriginRequestReductionAvailable,
		)
	}
}

func TestRenderComparisonMarkdownDirectOmitsProxyMetrics(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	comparison := compareResults(directComparisonConfig(), baseline, gantry)

	markdown := renderComparisonMarkdown(comparison)

	for _, forbidden := range []string{"Proxy requests", "Digest requests"} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("direct report contains unavailable metric %q:\n%s", forbidden, markdown)
		}
	}

	for _, required := range []string{
		"Mode: **direct**",
		"ACR origin bytes",
		string(originBytesAnalyticBaseline),
		string(originBytesGantryMeasured),
		"unavailable without Azure telemetry",
	} {
		if !strings.Contains(markdown, required) {
			t.Fatalf("direct report is missing %q:\n%s", required, markdown)
		}
	}
}

func TestCompareResultsFailsWithoutPeerByteMeasurements(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	gantry.GantryPeer.Complete = false

	comparison := compareResults(directComparisonConfig(), baseline, gantry)
	if comparison.Checks["peer_bytes_recorded"].Passed {
		t.Fatal("expected peer byte measurement check to fail")
	}

	if comparison.Passed {
		t.Fatal("comparison passed without complete peer byte measurements")
	}
}

func TestRenderComparisonMarkdownProxyRetainsProxyMetrics(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	baseline.OriginBytesSource = originBytesProxy
	gantry.OriginBytesSource = originBytesProxy
	baseline.Proxy.RequestsCompleted = 100
	gantry.Proxy.RequestsCompleted = 20

	config := directComparisonConfig()
	config.Mode = benchmarkModeProxy

	markdown := renderComparisonMarkdown(compareResults(config, baseline, gantry))

	for _, required := range []string{"Proxy requests", "Digest requests"} {
		if !strings.Contains(markdown, required) {
			t.Fatalf("proxy report is missing %q:\n%s", required, markdown)
		}
	}
}

func TestCompareResultsPrefersACRPullCounts(t *testing.T) {
	baseline, gantry := healthyDirectPhases()
	baseline.Azure = azurePhaseMeasurement{
		ACR:      acrPhaseMeasurement{SuccessfulPullCount: 100, Complete: true},
		Complete: true,
	}
	baseline.PodStartupLatency = latencySummary{P50Seconds: 4, P95Seconds: 5}
	baseline.PodStartupLatencySource = "AKSAuditAdmin"
	baseline.OriginBytesSource = originBytesPrivateEndpoint
	gantry.Azure = azurePhaseMeasurement{
		ACR:      acrPhaseMeasurement{SuccessfulPullCount: 20, Complete: true},
		Complete: true,
	}
	gantry.PodStartupLatency = latencySummary{P50Seconds: 2, P95Seconds: 3}
	gantry.PodStartupLatencySource = "AKSAuditAdmin"
	gantry.OriginBytesSource = originBytesPrivateEndpoint

	config := directComparisonConfig()
	config.AzureTelemetry = true

	comparison := compareResults(config, baseline, gantry)
	if comparison.OriginRequestReduction != 0.8 || comparison.OriginRequestSource != "ContainerRegistryRepositoryEvents" {
		t.Fatalf(
			"request reduction = %.3f source=%q, want 0.8 from ACR",
			comparison.OriginRequestReduction,
			comparison.OriginRequestSource,
		)
	}

	markdown := renderComparisonMarkdown(comparison)
	for _, expected := range []string{"ACR Private Endpoint bytes from ACR", "ACR successful image pulls", "100", "20", "AKSAuditAdmin / AKSAuditAdmin"} {
		if !strings.Contains(markdown, expected) {
			t.Fatalf("Azure report is missing %q:\n%s", expected, markdown)
		}
	}
}

func TestSubtractGantryMetricsCoversDerivedCounters(t *testing.T) {
	delta, err := subtractGantryMetrics(
		gantryMetrics{OriginPulls: 12, OriginBytes: 900, PeerFetchHits: 700, OriginLayerPulls: 9, OriginFallbacks: 2},
		gantryMetrics{OriginPulls: 3, OriginBytes: 100, PeerFetchHits: 80, OriginLayerPulls: 1, OriginFallbacks: 2},
	)
	if err != nil {
		t.Fatalf("subtractGantryMetrics: %v", err)
	}

	want := gantryMetrics{OriginPulls: 9, OriginBytes: 800, PeerFetchHits: 620, OriginLayerPulls: 8, OriginFallbacks: 0}
	if delta != want {
		t.Fatalf("delta = %+v, want %+v", delta, want)
	}
}

func TestSubtractGantryMetricsRejectsCounterReset(t *testing.T) {
	_, err := subtractGantryMetrics(
		gantryMetrics{OriginBytes: 9},
		gantryMetrics{OriginBytes: 10},
	)
	if err == nil || !strings.Contains(err.Error(), "origin_bytes decreased") {
		t.Fatalf("error = %v, want origin byte counter reset", err)
	}
}

func envFromMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLoadBenchmarkConfigModeDefaultsToProxy(t *testing.T) {
	config, err := loadBenchmarkConfig(envFromMap(nil))
	if err != nil {
		t.Fatalf("loadBenchmarkConfig: %v", err)
	}

	if config.Mode != benchmarkModeProxy || !config.usesProxy() {
		t.Fatalf("mode = %q, want %q so existing runs are unchanged", config.Mode, benchmarkModeProxy)
	}
}

func TestLoadBenchmarkConfigRejectsUnknownMode(t *testing.T) {
	_, err := loadBenchmarkConfig(envFromMap(map[string]string{"BENCHMARK_MODE": "proxyless"}))
	if err == nil {
		t.Fatalf("expected an error for an unknown BENCHMARK_MODE")
	}
}

// Direct mode measures origin bytes directly, so uneven image layers are valid.
func TestLoadBenchmarkConfigDirectAllowsUnevenLayerSplit(t *testing.T) {
	config, err := loadBenchmarkConfig(envFromMap(map[string]string{
		"BENCHMARK_MODE":           "direct",
		"BENCHMARK_IMAGE_SIZE_MIB": "8192",
		"BENCHMARK_IMAGE_LAYERS":   "7",
	}))
	if err != nil {
		t.Fatalf("loadBenchmarkConfig: %v", err)
	}

	if got, want := config.imageBytes(), uint64(8*1024*1024*1024); got != want {
		t.Fatalf("imageBytes = %d, want %d", got, want)
	}
}

// Proxy mode also keeps uneven layer shapes available.
func TestLoadBenchmarkConfigProxyAllowsUnevenLayerSplit(t *testing.T) {
	if _, err := loadBenchmarkConfig(envFromMap(map[string]string{
		"BENCHMARK_IMAGE_SIZE_MIB": "8192",
		"BENCHMARK_IMAGE_LAYERS":   "7",
	})); err != nil {
		t.Fatalf("loadBenchmarkConfig: %v", err)
	}
}

func TestValidateEnableIsModeAware(t *testing.T) {
	direct := benchmarkConfig{
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}
	if err := direct.validateEnable(); err != nil {
		t.Fatalf("direct enable must not require proxy image or push credentials: %v", err)
	}

	proxy := benchmarkConfig{Mode: benchmarkModeProxy, ACRLoginServer: "bench.azurecr.io"}
	if err := proxy.validateEnable(); err == nil {
		t.Fatalf("proxy enable must still require the proxy image and credentials")
	}
}

func TestValidateEnableAcceptsCompleteAzureTelemetryConfig(t *testing.T) {
	config := benchmarkConfig{
		Mode:                      benchmarkModeDirect,
		BaselineACRLoginServer:    "baseline.azurecr.io",
		GantryACRLoginServer:      "gantry.azurecr.io",
		AzureTelemetry:            true,
		LogAnalyticsWorkspaceID:   "workspace-id",
		BaselineACRResourceID:     "/subscriptions/s/registries/baseline",
		BaselinePrivateEndpointID: "/subscriptions/s/privateEndpoints/baseline",
		GantryACRResourceID:       "/subscriptions/s/registries/gantry",
		GantryPrivateEndpointID:   "/subscriptions/s/privateEndpoints/gantry",
		AKSResourceID:             "/subscriptions/s/managedClusters/bench",
	}

	if err := config.validateEnable(); err != nil {
		t.Fatalf("validateEnable: %v", err)
	}
}

func TestValidateEnableRejectsSharedDirectRegistry(t *testing.T) {
	config := benchmarkConfig{
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "shared.azurecr.io",
		GantryACRLoginServer:   "shared.azurecr.io",
	}

	if err := config.validateEnable(); err == nil || !strings.Contains(err.Error(), "must be different") {
		t.Fatalf("error = %v, want distinct registry rejection", err)
	}
}

func TestRegistryForPhaseUsesSeparateDirectResources(t *testing.T) {
	config := benchmarkConfig{
		Mode:                      benchmarkModeDirect,
		BaselineACRLoginServer:    "baseline.azurecr.io",
		BaselineACRResourceID:     "baseline-resource",
		BaselinePrivateEndpointID: "baseline-endpoint",
		GantryACRLoginServer:      "gantry.azurecr.io",
		GantryACRResourceID:       "gantry-resource",
		GantryPrivateEndpointID:   "gantry-endpoint",
	}

	baseline, err := config.registryForPhase(proxyPhaseBaseline)
	if err != nil {
		t.Fatal(err)
	}

	gantry, err := config.registryForPhase(proxyPhaseGantryCold)
	if err != nil {
		t.Fatal(err)
	}

	if baseline.LoginServer != "baseline.azurecr.io" || baseline.ResourceID != "baseline-resource" || baseline.PrivateEndpointID != "baseline-endpoint" {
		t.Fatalf("baseline registry = %+v", baseline)
	}

	if gantry.LoginServer != "gantry.azurecr.io" || gantry.ResourceID != "gantry-resource" || gantry.PrivateEndpointID != "gantry-endpoint" {
		t.Fatalf("Gantry registry = %+v", gantry)
	}
}

func TestStateModeControlsProxyUsage(t *testing.T) {
	// State written before direct mode existed has no mode and described a
	// proxy run.
	if !(benchmarkState{}).usesProxy() {
		t.Fatalf("state with an empty mode must be treated as a proxy run")
	}

	if (benchmarkState{Mode: benchmarkModeDirect}).usesProxy() {
		t.Fatalf("direct-mode state must not be treated as a proxy run")
	}
}

func TestPreparedImagesRequiresBothDigestReferences(t *testing.T) {
	state := benchmarkState{
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
		WorkloadRepository:     "pull",
		BaselineImage:          "baseline.azurecr.io/pull@sha256:abc",
		GantryColdImage:        "gantry.azurecr.io/pull@sha256:def",
		WorkloadPayloadSHA256:  "sha256:payload",
	}

	baseline, gantry, err := state.preparedImages()
	if err != nil {
		t.Fatalf("preparedImages: %v", err)
	}

	if baseline != state.BaselineImage || gantry != state.GantryColdImage {
		t.Fatalf("prepared images = %q, %q", baseline, gantry)
	}

	state.GantryColdImage = ""
	if _, _, err := state.preparedImages(); err == nil || !strings.Contains(err.Error(), "run prepare") {
		t.Fatalf("missing-image error = %v, want prepare guidance", err)
	}
}

func TestPreparedImagesRejectsIdenticalDigests(t *testing.T) {
	state := benchmarkState{
		Mode:                   benchmarkModeDirect,
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
		WorkloadRepository:     "pull",
		BaselineImage:          "baseline.azurecr.io/pull@sha256:same",
		GantryColdImage:        "gantry.azurecr.io/pull@sha256:same",
		WorkloadPayloadSHA256:  "sha256:payload",
	}

	if _, _, err := state.preparedImages(); err == nil || !strings.Contains(err.Error(), "would reuse") {
		t.Fatalf("error = %v, want cache reuse rejection", err)
	}
}
