// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestParseJobObservation(t *testing.T) {
	phaseStartedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	pods := map[string]any{"items": []any{
		testSucceededPod("pod-a", "node-a", phaseStartedAt.Add(10*time.Second), phaseStartedAt.Add(11*time.Second)),
		testSucceededPod("pod-b", "node-b", phaseStartedAt.Add(20*time.Second), phaseStartedAt.Add(21*time.Second)),
		testSucceededPod("pod-c", "node-c", phaseStartedAt.Add(30*time.Second), phaseStartedAt.Add(31*time.Second)),
		testSucceededPod("pod-d", "node-d", phaseStartedAt.Add(40*time.Second), phaseStartedAt.Add(41*time.Second)),
	}}

	raw, err := json.Marshal(pods)
	if err != nil {
		t.Fatal(err)
	}

	observation, err := parseJobObservation(raw, 4, phaseStartedAt)
	if err != nil {
		t.Fatalf("parseJobObservation: %v", err)
	}

	if len(observation.Nodes) != 4 || observation.Nodes[0] != "node-a" || observation.Nodes[3] != "node-d" {
		t.Fatalf("nodes = %v", observation.Nodes)
	}

	if observation.PodStartLatency.P50Seconds != 20 || observation.PodStartLatency.P95Seconds != 40 || observation.PodStartLatency.P100Seconds != 40 {
		t.Fatalf("start latency = %+v", observation.PodStartLatency)
	}
}

func TestParseJobObservationRejectsDuplicateNode(t *testing.T) {
	phaseStartedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	pods := map[string]any{"items": []any{
		testSucceededPod("pod-a", "node-a", phaseStartedAt.Add(time.Second), phaseStartedAt.Add(2*time.Second)),
		testSucceededPod("pod-b", "node-a", phaseStartedAt.Add(time.Second), phaseStartedAt.Add(2*time.Second)),
	}}

	raw, err := json.Marshal(pods)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := parseJobObservation(raw, 2, phaseStartedAt); err == nil {
		t.Fatal("parseJobObservation accepted duplicate node placement")
	}
}

func testSucceededPod(name, node string, startedAt, finishedAt time.Time) map[string]any {
	return map[string]any{
		"metadata": map[string]string{"name": name},
		"spec":     map[string]string{"nodeName": node},
		"status": map[string]any{
			"phase": "Succeeded",
			"containerStatuses": []any{
				map[string]any{
					"name": "pull",
					"state": map[string]any{
						"terminated": map[string]any{
							"exitCode":   0,
							"startedAt":  startedAt.Format(time.RFC3339Nano),
							"finishedAt": finishedAt.Format(time.RFC3339Nano),
						},
					},
				},
			},
		},
	}
}

func Example_summarizeLatencies() {
	summary := summarizeLatencies([]time.Duration{time.Second, 3 * time.Second, 2 * time.Second})
	fmt.Printf("%.0f %.0f %.0f\n", summary.P50Seconds, summary.P95Seconds, summary.P100Seconds)
	// Output: 2 3 3
}
