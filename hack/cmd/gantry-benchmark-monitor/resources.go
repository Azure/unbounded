// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

type nodeResourceGrid struct {
	Nodes         []string
	CPUPercent    map[string]float64
	MemoryPercent map[string]float64
}

type resourceSummary struct {
	Samples int
	P50     float64
	P95     float64
	Max     float64
	MaxNode string
}

func nodeResourceExpression(config monitorConfig) string {
	labels := fmt.Sprintf(`namespace=%s,gantry_benchmark="true"`, strconv.Quote(config.benchmarkNamespace))
	cpu := fmt.Sprintf(
		`label_replace(((1-avg by(instance)(rate(node_cpu_seconds_total{%s,mode="idle"}[1m])))*100) * on(instance) group_left(nodename) node_uname_info{%s},"resource","cpu","nodename",".*")`,
		labels,
		labels,
	)
	memory := fmt.Sprintf(
		`label_replace(((1-node_memory_MemAvailable_bytes{%s}/node_memory_MemTotal_bytes{%s})*100) * on(instance) group_left(nodename) node_uname_info{%s},"resource","memory","nodename",".*")`,
		labels,
		labels,
		labels,
	)

	return cpu + " or " + memory
}

func aggregateNodeResources(response instantResponse, nodes []string) nodeResourceGrid {
	resources := nodeResourceGrid{
		Nodes:         append([]string(nil), nodes...),
		CPUPercent:    map[string]float64{},
		MemoryPercent: map[string]float64{},
	}
	sort.Strings(resources.Nodes)

	for _, series := range response.Series {
		node := strings.ToLower(series.Metric["nodename"])
		if node == "" {
			node = strings.ToLower(series.Metric["node"])
		}

		if node == "" {
			continue
		}

		value := min(100, max(0, series.Value))

		switch series.Metric["resource"] {
		case "cpu":
			resources.CPUPercent[node] = value
		case "memory":
			resources.MemoryPercent[node] = value
		}
	}

	return resources
}

func renderNodeResources(builder *strings.Builder, snapshot monitorSnapshot) {
	if snapshot.GridError != "" {
		return
	}

	resources := snapshot.Resources
	if len(resources.Nodes) == 0 || len(resources.CPUPercent)+len(resources.MemoryPercent) == 0 {
		fmt.Fprintln(builder, "\nnode resources: waiting for node-exporter samples")

		return
	}

	nodes, page, pages := pageNodes(resources.Nodes, snapshot.NodePage, snapshot.NodesPerPage)

	fmt.Fprintln(builder, "\n=== Node CPU and memory x nodes ===")
	fmt.Fprintf(builder, "page %d/%d; nodes %d; showing %s .. %s\n", page, pages, len(resources.Nodes), nodes[0], nodes[len(nodes)-1])
	renderNodeHeader(builder, nodes)
	renderResourceRow(builder, "CPU 1m", nodes, resources.CPUPercent)
	renderResourceRow(builder, "MEM used", nodes, resources.MemoryPercent)
	fmt.Fprintln(builder, "legend: 0=0-9%, 1=10-19%, ..., 9=90-100%, .=sample unavailable")
	renderResourceSummary(builder, "fleet CPU 1m", summarizeResource(resources.CPUPercent, resources.Nodes), len(resources.Nodes))
	renderResourceSummary(builder, "fleet memory used", summarizeResource(resources.MemoryPercent, resources.Nodes), len(resources.Nodes))
}

func renderResourceRow(builder *strings.Builder, label string, nodes []string, values map[string]float64) {
	fmt.Fprintf(builder, "%-10s", label)

	for _, node := range nodes {
		builder.WriteByte(resourceCell(values, node))
	}

	builder.WriteByte('\n')
}

func resourceCell(values map[string]float64, node string) byte {
	value, ok := values[node]
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return '.'
	}

	decile := int(min(99.999, max(0, value)) / 10)

	return byte('0' + decile)
}

func summarizeResource(values map[string]float64, nodes []string) resourceSummary {
	summary := resourceSummary{}
	samples := make([]float64, 0, len(nodes))

	for _, node := range nodes {
		value, ok := values[node]
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}

		samples = append(samples, value)
		if summary.MaxNode == "" || value > summary.Max {
			summary.Max = value
			summary.MaxNode = node
		}
	}

	sort.Float64s(samples)

	summary.Samples = len(samples)
	if len(samples) > 0 {
		summary.P50 = resourceQuantile(samples, 0.50)
		summary.P95 = resourceQuantile(samples, 0.95)
	}

	return summary
}

func resourceQuantile(sortedValues []float64, quantile float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}

	position := quantile * float64(len(sortedValues)-1)
	lower := int(math.Floor(position))

	upper := int(math.Ceil(position))
	if lower == upper {
		return sortedValues[lower]
	}

	weight := position - float64(lower)

	return sortedValues[lower]*(1-weight) + sortedValues[upper]*weight
}

func renderResourceSummary(builder *strings.Builder, label string, summary resourceSummary, total int) {
	if summary.Samples == 0 {
		fmt.Fprintf(builder, "%s: 0/%d sampled\n", label, total)

		return
	}

	fmt.Fprintf(builder, "%s: %d/%d sampled, p50 %.1f%%, p95 %.1f%%, max %.1f%% (%s)\n",
		label,
		summary.Samples,
		total,
		summary.P50,
		summary.P95,
		summary.Max,
		summary.MaxNode,
	)
}
