// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type monitoringCoverageResult struct {
	count float64
	err   error
}

type monitoringCoverageRunner struct {
	results []monitoringCoverageResult
	calls   int
}

func (r *monitoringCoverageRunner) Run(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
	index := r.calls
	r.calls++
	if index >= len(r.results) {
		index = len(r.results) - 1
	}
	result := r.results[index]
	if result.err != nil {
		return nil, result.err
	}

	return []byte(fmt.Sprintf(
		`{"status":"success","data":{"result":[{"value":[0,%q]}]}}`,
		fmt.Sprintf("%.0f", result.count),
	)), nil
}

func TestWaitForPrometheusMetricCoverageRetriesPartialScrape(t *testing.T) {
	runner := &monitoringCoverageRunner{results: []monitoringCoverageResult{{count: 9}, {count: 1000}}}
	var stdout bytes.Buffer
	benchmark := benchmark{
		config: benchmarkConfig{
			MonitoringNamespace:   "monitoring",
			PrometheusService:     "prometheus",
			NodeCount:             1000,
			TelemetryTimeout:      time.Second,
			TelemetryPollInterval: time.Millisecond,
		},
		commands: runner,
		stdout:   &stdout,
	}

	if err := benchmark.waitForPrometheusMetricCoverage(context.Background(), "containerd build", "count(metric)"); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatalf("query calls = %d, want 2", runner.calls)
	}
	if !strings.Contains(stdout.String(), "9/1000 observer pods") {
		t.Fatalf("progress output = %q", stdout.String())
	}
}

func TestWaitForPrometheusMetricCoverageRetriesQueryError(t *testing.T) {
	runner := &monitoringCoverageRunner{results: []monitoringCoverageResult{{err: errors.New("not ready")}, {count: 1000}}}
	benchmark := benchmark{
		config: benchmarkConfig{
			MonitoringNamespace:   "monitoring",
			PrometheusService:     "prometheus",
			NodeCount:             1000,
			TelemetryTimeout:      time.Second,
			TelemetryPollInterval: time.Millisecond,
		},
		commands: runner,
		stdout:   &bytes.Buffer{},
	}

	if err := benchmark.waitForPrometheusMetricCoverage(context.Background(), "containerd build", "count(metric)"); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 2 {
		t.Fatalf("query calls = %d, want 2", runner.calls)
	}
}

func TestWaitForPrometheusMetricCoverageTimesOut(t *testing.T) {
	runner := &monitoringCoverageRunner{results: []monitoringCoverageResult{{count: 9}}}
	benchmark := benchmark{
		config: benchmarkConfig{
			MonitoringNamespace:   "monitoring",
			PrometheusService:     "prometheus",
			NodeCount:             1000,
			TelemetryTimeout:      time.Nanosecond,
			TelemetryPollInterval: time.Hour,
		},
		commands: runner,
		stdout:   &bytes.Buffer{},
	}

	err := benchmark.waitForPrometheusMetricCoverage(context.Background(), "containerd build", "count(metric)")
	if err == nil || !strings.Contains(err.Error(), "9/1000 observer pods after waiting 1ns") {
		t.Fatalf("timeout error = %v", err)
	}
}
