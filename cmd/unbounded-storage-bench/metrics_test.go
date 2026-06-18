// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParsePrometheusCanonicalizesLabels(t *testing.T) {
	samples, err := parsePrometheus(strings.NewReader(`
# HELP ignored ignored
unbounded_storage_frontend_requests_total{status="200",frontend="loadgen",method="GET"} 7
process_resident_memory_bytes 1048576
`))
	if err != nil {
		t.Fatalf("parsePrometheus: %v", err)
	}

	key := metricKey{Name: "unbounded_storage_frontend_requests_total", Labels: `frontend="loadgen",method="GET",status="200"`}
	if samples[key] != 7 {
		t.Fatalf("request sample = %v", samples[key])
	}

	if samples[metricKey{Name: "process_resident_memory_bytes"}] != 1048576 {
		t.Fatalf("rss sample = %v", samples[metricKey{Name: "process_resident_memory_bytes"}])
	}
}

func TestBuildIntervalReportComputesRates(t *testing.T) {
	start := time.Unix(100, 0)
	prev := scrapePoint{At: start, Samples: sampleSet{
		{Name: "unbounded_storage_frontend_requests_total", Labels: `frontend="loadgen",method="GET",status="200"`}: 10,
		{Name: "unbounded_storage_frontend_response_bytes_total", Labels: `frontend="loadgen"`}:                     1024 * 1024,
		{Name: "unbounded_storage_frontend_request_duration_seconds_sum", Labels: `frontend="loadgen"`}:             2,
		{Name: "unbounded_storage_frontend_request_duration_seconds_count", Labels: `frontend="loadgen"`}:           10,
		{Name: "unbounded_storage_disk_ops_total", Labels: `disk="d",op="read",outcome="ok"`}:                       100,
		{Name: "unbounded_storage_fabric_rpc_served_total", Labels: `outcome="ok"`}:                                 20,
		{Name: "unbounded_storage_fabric_bytes_written_total"}:                                                      2 * 1024 * 1024,
		{Name: "process_cpu_seconds_total"}:                                                                         4,
	}}
	current := scrapePoint{At: start.Add(5 * time.Second), Samples: sampleSet{
		{Name: "unbounded_storage_frontend_requests_total", Labels: `frontend="loadgen",method="GET",status="200"`}: 60,
		{Name: "unbounded_storage_frontend_response_bytes_total", Labels: `frontend="loadgen"`}:                     11 * 1024 * 1024,
		{Name: "unbounded_storage_frontend_request_duration_seconds_sum", Labels: `frontend="loadgen"`}:             7,
		{Name: "unbounded_storage_frontend_request_duration_seconds_count", Labels: `frontend="loadgen"`}:           60,
		{Name: "unbounded_storage_disk_ops_total", Labels: `disk="d",op="read",outcome="ok"`}:                       150,
		{Name: "unbounded_storage_fabric_rpc_served_total", Labels: `outcome="ok"`}:                                 45,
		{Name: "unbounded_storage_fabric_bytes_written_total"}:                                                      7 * 1024 * 1024,
		{Name: "process_cpu_seconds_total"}:                                                                         6,
		{Name: "process_resident_memory_bytes"}:                                                                     256 * 1024 * 1024,
	}}

	report := buildIntervalReport("measure", 5*time.Second, prev, current)

	if report.RequestsPerSec != 10 {
		t.Fatalf("req/s = %v", report.RequestsPerSec)
	}

	if report.MiBPerSec != 2 {
		t.Fatalf("MiB/s = %v", report.MiBPerSec)
	}

	if report.AvgLatencyMs != 100 {
		t.Fatalf("avg latency = %v", report.AvgLatencyMs)
	}

	if report.DiskOpsPerSec != 10 || report.FabricRPCPerSec != 5 || report.FabricMiBPerSec != 1 {
		t.Fatalf("path rates = disk %v fabric rpc %v fabric MiB %v", report.DiskOpsPerSec, report.FabricRPCPerSec, report.FabricMiBPerSec)
	}

	if report.CPU != 0.4 || report.RSSMiB != 256 {
		t.Fatalf("process metrics = cpu %v rss %v", report.CPU, report.RSSMiB)
	}
}

func TestHistogramQuantile(t *testing.T) {
	prev := sampleSet{
		{Name: "latency_bucket", Labels: `le="0.1"`}:  0,
		{Name: "latency_bucket", Labels: `le="0.2"`}:  0,
		{Name: "latency_bucket", Labels: `le="+Inf"`}: 0,
	}
	current := sampleSet{
		{Name: "latency_bucket", Labels: `le="0.1"`}:  50,
		{Name: "latency_bucket", Labels: `le="0.2"`}:  100,
		{Name: "latency_bucket", Labels: `le="+Inf"`}: 100,
	}

	got := histogramQuantile(prev, current, "latency_bucket", 0.75)
	if got < 0.149 || got > 0.151 {
		t.Fatalf("p75 = %v, want about 0.15", got)
	}
}

func TestValidateScenarioResultRequiresPathCounters(t *testing.T) {
	result := scenarioResult{
		Start: scrapePoint{Samples: sampleSet{
			{Name: "unbounded_storage_frontend_requests_total"}: 0,
		}},
		End: scrapePoint{Samples: sampleSet{
			{Name: "unbounded_storage_frontend_requests_total"}: 10,
		}},
	}

	if err := validateScenarioResult(scenarioBlockDisk, result); err == nil {
		t.Fatal("block-disk validation succeeded without disk counters")
	}
}

func TestScrapeLoopStartsMeasurementAtWarmupBoundary(t *testing.T) {
	key := metricKey{Name: "unbounded_storage_frontend_requests_total"}
	base := time.Unix(100, 0)
	sequence := 0
	scrape := func(context.Context, []nodeSpec) (scrapePoint, error) {
		sequence++

		return scrapePoint{
			At: base.Add(time.Duration(sequence) * time.Second),
			Samples: sampleSet{
				key: float64(sequence),
			},
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := scrapeLoopWithScraper(ctx, scenarioFabricRPC, []nodeSpec{{ID: 1}}, time.Nanosecond, time.Second, time.Millisecond, scrape)
	if err != nil {
		t.Fatalf("scrapeLoopWithScraper: %v", err)
	}

	if got := result.Start.Samples[key]; got != 2 {
		t.Fatalf("measurement start sample = %v, want boundary scrape sample 2", got)
	}

	if got := result.End.Samples[key]; got != 3 {
		t.Fatalf("measurement end sample = %v, want sample 3", got)
	}
}
