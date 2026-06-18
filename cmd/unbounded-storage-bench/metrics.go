// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type metricKey struct {
	Name   string
	Labels string
}

type sampleSet map[metricKey]float64

type scrapePoint struct {
	At      time.Time
	Samples sampleSet
}

type intervalReport struct {
	Phase           string
	Elapsed         time.Duration
	RequestsPerSec  float64
	ErrorsPerSec    float64
	MiBPerSec       float64
	AvgLatencyMs    float64
	P50Ms           float64
	P90Ms           float64
	P99Ms           float64
	DiskOpsPerSec   float64
	FabricRPCPerSec float64
	FabricMiBPerSec float64
	BackendPerSec   float64
	CPU             float64
	RSSMiB          float64
}

type scenarioResult struct {
	Start scrapePoint
	End   scrapePoint
	Last  intervalReport
}

func scrapeLoop(ctx context.Context, client *http.Client, scenario scenarioKind, nodes []nodeSpec, warmup, duration, interval time.Duration) (scenarioResult, error) {
	return scrapeLoopWithScraper(ctx, scenario, nodes, warmup, duration, interval, func(ctx context.Context, nodes []nodeSpec) (scrapePoint, error) {
		return scrapeNodes(ctx, client, nodes)
	})
}

func scrapeLoopWithScraper(ctx context.Context, scenario scenarioKind, nodes []nodeSpec, warmup, duration, interval time.Duration, scrape func(context.Context, []nodeSpec) (scrapePoint, error)) (scenarioResult, error) {
	start := time.Now()
	prev, err := scrape(ctx, nodes)
	if err != nil {
		return scenarioResult{}, err
	}

	measurementStarted := false
	var measurementStart scrapePoint
	var last intervalReport
	if warmup <= 0 {
		measurementStarted = true
		measurementStart = prev
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return scenarioResult{}, ctx.Err()
		case now := <-ticker.C:
			current, err := scrape(ctx, nodes)
			if err != nil {
				return scenarioResult{}, err
			}

			phase := "measure"
			elapsed := now.Sub(start)
			if elapsed < warmup {
				phase = "warmup"
			} else if !measurementStarted {
				measurementStarted = true
				measurementStart = current
				prev = current

				continue
			}

			last = buildIntervalReport(phase, elapsed, prev, current)
			printInterval(scenario, last)
			prev = current

			if measurementStarted && current.At.Sub(measurementStart.At) >= duration {
				return scenarioResult{
					Start: measurementStart,
					End:   current,
					Last:  buildIntervalReport("summary", current.At.Sub(measurementStart.At), measurementStart, current),
				}, nil
			}
		}
	}
}

func waitForMetrics(ctx context.Context, client *http.Client, nodes []nodeSpec, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		allReady := true
		for _, node := range nodes {
			if _, err := scrapeURL(ctx, client, node.ForwardURL); err != nil {
				allReady = false
				break
			}
		}

		if allReady {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for metrics after %s", timeout)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func scrapeNodes(ctx context.Context, client *http.Client, nodes []nodeSpec) (scrapePoint, error) {
	merged := sampleSet{}
	for _, node := range nodes {
		samples, err := scrapeURL(ctx, client, node.ForwardURL)
		if err != nil {
			return scrapePoint{}, fmt.Errorf("scrape node %d: %w", node.ID, err)
		}

		for key, value := range samples {
			merged[key] += value
		}
	}

	return scrapePoint{At: time.Now(), Samples: merged}, nil
}

func scrapeURL(ctx context.Context, client *http.Client, url string) (sampleSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s", url, resp.Status)
	}

	return parsePrometheus(resp.Body)
}

func parsePrometheus(r io.Reader) (sampleSet, error) {
	samples := sampleSet{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, labels, value, ok := parseMetricLine(line)
		if !ok {
			continue
		}

		samples[metricKey{Name: name, Labels: labels}] += value
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return samples, nil
}

func parseMetricLine(line string) (string, string, float64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", "", 0, false
	}

	metric := fields[0]
	value, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return "", "", 0, false
	}

	name := metric
	labels := ""
	if idx := strings.IndexByte(metric, '{'); idx >= 0 {
		name = metric[:idx]
		if !strings.HasSuffix(metric, "}") {
			return "", "", 0, false
		}

		labels = canonicalLabels(metric[idx+1 : len(metric)-1])
	}

	return name, labels, value, true
}

func canonicalLabels(raw string) string {
	if raw == "" {
		return ""
	}

	parts := splitLabels(raw)
	sort.Strings(parts)

	return strings.Join(parts, ",")
}

func splitLabels(raw string) []string {
	parts := []string{}
	start := 0
	inString := false
	escaped := false

	for i, r := range raw {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case r == ',' && !inString:
			parts = append(parts, strings.TrimSpace(raw[start:i]))
			start = i + 1
		}
	}

	parts = append(parts, strings.TrimSpace(raw[start:]))

	return parts
}

func buildIntervalReport(phase string, elapsed time.Duration, prev, current scrapePoint) intervalReport {
	seconds := current.At.Sub(prev.At).Seconds()
	if seconds <= 0 {
		seconds = 1
	}

	reqDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_frontend_requests_total", nil)
	errorDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_frontend_requests_total", func(labels string) bool {
		return !strings.Contains(labels, `status="200"`)
	})
	byteDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_frontend_response_bytes_total", nil)
	durationSumDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_frontend_request_duration_seconds_sum", nil)
	durationCountDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_frontend_request_duration_seconds_count", nil)
	diskDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_disk_ops_total", nil)
	fabricRPCDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_fabric_rpc_served_total", nil)
	fabricByteDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_fabric_bytes_written_total", nil)
	backendDelta := deltaMatching(prev.Samples, current.Samples, "unbounded_storage_backend_fetches_total", nil)
	cpuDelta := deltaMatching(prev.Samples, current.Samples, "process_cpu_seconds_total", nil)
	rss := latestMatching(current.Samples, "process_resident_memory_bytes")

	avgMs := 0.0
	if durationCountDelta > 0 {
		avgMs = durationSumDelta / durationCountDelta * 1000
	}

	return intervalReport{
		Phase:           phase,
		Elapsed:         elapsed,
		RequestsPerSec:  reqDelta / seconds,
		ErrorsPerSec:    errorDelta / seconds,
		MiBPerSec:       byteDelta / seconds / 1024 / 1024,
		AvgLatencyMs:    avgMs,
		P50Ms:           histogramQuantile(prev.Samples, current.Samples, "unbounded_storage_frontend_request_duration_seconds_bucket", 0.50) * 1000,
		P90Ms:           histogramQuantile(prev.Samples, current.Samples, "unbounded_storage_frontend_request_duration_seconds_bucket", 0.90) * 1000,
		P99Ms:           histogramQuantile(prev.Samples, current.Samples, "unbounded_storage_frontend_request_duration_seconds_bucket", 0.99) * 1000,
		DiskOpsPerSec:   diskDelta / seconds,
		FabricRPCPerSec: fabricRPCDelta / seconds,
		FabricMiBPerSec: fabricByteDelta / seconds / 1024 / 1024,
		BackendPerSec:   backendDelta / seconds,
		CPU:             cpuDelta / seconds,
		RSSMiB:          rss / 1024 / 1024,
	}
}

func deltaMatching(prev, current sampleSet, name string, pred func(string) bool) float64 {
	total := 0.0
	for key, currentValue := range current {
		if key.Name != name {
			continue
		}

		if pred != nil && !pred(key.Labels) {
			continue
		}

		delta := currentValue - prev[key]
		if delta > 0 {
			total += delta
		}
	}

	return total
}

func latestMatching(samples sampleSet, name string) float64 {
	total := 0.0
	for key, value := range samples {
		if key.Name == name {
			total += value
		}
	}

	return total
}

func histogramQuantile(prev, current sampleSet, name string, q float64) float64 {
	buckets := map[float64]float64{}
	for key, currentValue := range current {
		if key.Name != name {
			continue
		}

		le, ok := labelValue(key.Labels, "le")
		if !ok {
			continue
		}

		upper, err := strconv.ParseFloat(le, 64)
		if err != nil {
			if le == "+Inf" {
				upper = math.Inf(1)
			} else {
				continue
			}
		}

		delta := currentValue - prev[key]
		if delta > 0 {
			buckets[upper] += delta
		}
	}

	if len(buckets) == 0 {
		return 0
	}

	uppers := make([]float64, 0, len(buckets))
	for upper := range buckets {
		uppers = append(uppers, upper)
	}
	sort.Float64s(uppers)

	total := buckets[uppers[len(uppers)-1]]
	if total <= 0 || math.IsInf(total, 0) {
		return 0
	}

	target := total * q
	prevUpper := 0.0
	prevCount := 0.0
	for _, upper := range uppers {
		count := buckets[upper]
		if count >= target {
			if math.IsInf(upper, 1) {
				return prevUpper
			}

			bucketCount := count - prevCount
			if bucketCount <= 0 {
				return upper
			}

			fraction := (target - prevCount) / bucketCount

			return prevUpper + (upper-prevUpper)*fraction
		}

		prevUpper = upper
		prevCount = count
	}

	return 0
}

func labelValue(labels, name string) (string, bool) {
	prefix := name + "="
	for _, part := range splitLabels(labels) {
		if strings.HasPrefix(part, prefix) {
			return strings.Trim(strings.TrimPrefix(part, prefix), `"`), true
		}
	}

	return "", false
}

func validateScenarioResult(scenario scenarioKind, result scenarioResult) error {
	requests := deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_frontend_requests_total", nil)
	if requests <= 0 {
		return fmt.Errorf("scenario %s did not complete any frontend requests", scenario)
	}

	switch scenario {
	case scenarioBlockDisk:
		if deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_disk_ops_total", nil) <= 0 {
			return fmt.Errorf("scenario %s did not advance disk ops", scenario)
		}
	case scenarioFabricRPC:
		if deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_fabric_rpc_served_total", nil) <= 0 && deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_fabric_bytes_written_total", nil) <= 0 {
			return fmt.Errorf("scenario %s did not advance fabric counters", scenario)
		}
	case scenarioIntegrated:
		if deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_disk_ops_total", nil) <= 0 {
			return fmt.Errorf("scenario %s did not advance disk ops", scenario)
		}

		if deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_fabric_rpc_served_total", nil) <= 0 && deltaMatching(result.Start.Samples, result.End.Samples, "unbounded_storage_fabric_bytes_written_total", nil) <= 0 {
			return fmt.Errorf("scenario %s did not advance fabric counters", scenario)
		}
	}

	return nil
}

func printInterval(scenario scenarioKind, r intervalReport) {
	fmt.Printf("time=%s phase=%s scenario=%s req/s=%.1f err/s=%.1f MiB/s=%.1f avg_ms=%.2f p50_ms=%.2f p90_ms=%.2f p99_ms=%.2f disk_ops/s=%.1f fabric_rpc/s=%.1f fabric_MiB/s=%.1f backend_fetch/s=%.1f cpu=%.2f rss_mib=%.1f\n",
		r.Elapsed.Truncate(time.Second), r.Phase, scenario, r.RequestsPerSec, r.ErrorsPerSec, r.MiBPerSec, r.AvgLatencyMs, r.P50Ms, r.P90Ms, r.P99Ms, r.DiskOpsPerSec, r.FabricRPCPerSec, r.FabricMiBPerSec, r.BackendPerSec, r.CPU, r.RSSMiB)
}

func printSummary(scenario scenarioKind, result scenarioResult) {
	r := result.Last
	fmt.Printf("summary scenario=%s duration=%s req/s=%.1f err/s=%.1f MiB/s=%.1f avg_ms=%.2f p50_ms=%.2f p90_ms=%.2f p99_ms=%.2f disk_ops/s=%.1f fabric_rpc/s=%.1f fabric_MiB/s=%.1f backend_fetch/s=%.1f cpu=%.2f rss_mib=%.1f assertions=ok\n",
		scenario, r.Elapsed.Truncate(time.Second), r.RequestsPerSec, r.ErrorsPerSec, r.MiBPerSec, r.AvgLatencyMs, r.P50Ms, r.P90Ms, r.P99Ms, r.DiskOpsPerSec, r.FabricRPCPerSec, r.FabricMiBPerSec, r.BackendPerSec, r.CPU, r.RSSMiB)
}
