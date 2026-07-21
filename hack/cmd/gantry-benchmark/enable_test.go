// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"
)

func TestGantryPodMonitor(t *testing.T) {
	benchmark := &benchmark{config: benchmarkConfig{
		Namespace:       "gantry-benchmark",
		GantryNamespace: "gantry-system",
		GantryDaemonSet: "gantry",
		KPSRelease:      "kps",
	}}

	monitor := benchmark.gantryPodMonitor()
	metadata := requireMap(t, monitor["metadata"], "metadata")
	spec := requireMap(t, monitor["spec"], "spec")
	namespaceSelector := requireMap(t, spec["namespaceSelector"], "spec.namespaceSelector")
	selector := requireMap(t, spec["selector"], "spec.selector")

	if monitor["kind"] != "PodMonitor" || metadata["namespace"] != "gantry-benchmark" {
		t.Fatalf("unexpected PodMonitor identity: %+v", monitor)
	}

	if got := namespaceSelector["matchNames"].([]string); len(got) != 1 || got[0] != "gantry-system" {
		t.Fatalf("namespace selector = %v", got)
	}

	matchLabels := selector["matchLabels"].(map[string]string)
	if matchLabels["app.kubernetes.io/name"] != "gantry" {
		t.Fatalf("selector = %v", matchLabels)
	}
}
