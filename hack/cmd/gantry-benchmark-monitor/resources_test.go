// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
)

func TestAggregateAndRenderNodeResources(t *testing.T) {
	response := instantResponse{Series: []instantSeries{
		{Metric: map[string]string{"resource": "cpu", "nodename": "NODE-A"}, Value: 12},
		{Metric: map[string]string{"resource": "cpu", "nodename": "node-b"}, Value: 89},
		{Metric: map[string]string{"resource": "memory", "nodename": "NODE-A"}, Value: 55},
		{Metric: map[string]string{"resource": "memory", "nodename": "node-b"}, Value: 99},
	}}
	resources := aggregateNodeResources(response, []string{"node-c", "node-b", "node-a"})

	if resources.CPUPercent["node-a"] != 12 || resources.MemoryPercent["node-b"] != 99 {
		t.Fatalf("resources = %#v", resources)
	}

	var builder strings.Builder
	renderNodeResources(&builder, monitorSnapshot{
		Resources:    resources,
		NodePage:     1,
		NodesPerPage: 2,
	})

	output := builder.String()
	for _, want := range []string{
		"=== Node CPU and memory x nodes ===",
		"page 1/2; nodes 3; showing node-a .. node-b",
		"CPU 1m    18",
		"MEM used  59",
		"fleet CPU 1m: 2/3 sampled",
		"max 89.0% (node-b)",
		"fleet memory used: 2/3 sampled",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("resource grid missing %q:\n%s", want, output)
		}
	}
}

func TestNodeResourceExpression(t *testing.T) {
	expression := nodeResourceExpression(monitorConfig{benchmarkNamespace: "gantry-benchmark"})

	for _, want := range []string{
		`node_cpu_seconds_total`,
		`mode="idle"`,
		`node_memory_MemAvailable_bytes`,
		`node_memory_MemTotal_bytes`,
		`node_uname_info`,
		`namespace="gantry-benchmark"`,
		`"resource","cpu"`,
		`"resource","memory"`,
	} {
		if !strings.Contains(expression, want) {
			t.Errorf("expression %q is missing %q", expression, want)
		}
	}
}

func TestResourceCell(t *testing.T) {
	values := map[string]float64{"low": 0, "middle": 55, "high": 100}
	for node, want := range map[string]byte{
		"low":     '0',
		"middle":  '5',
		"high":    '9',
		"missing": '.',
	} {
		if got := resourceCell(values, node); got != want {
			t.Errorf("resourceCell(%q) = %q, want %q", node, got, want)
		}
	}
}
