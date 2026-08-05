// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type latencySummary struct {
	P50Seconds  float64 `json:"p50_seconds"`
	P95Seconds  float64 `json:"p95_seconds"`
	P100Seconds float64 `json:"p100_seconds"`
}

type jobObservation struct {
	JobName          string                          `json:"job_name"`
	PhaseStartedAt   time.Time                       `json:"phase_started_at"`
	PhaseFinishedAt  time.Time                       `json:"phase_finished_at"`
	Nodes            []string                        `json:"nodes"`
	Pods             []string                        `json:"pods"`
	PodNodes         map[string]string               `json:"pod_nodes"`
	PodTimings       map[string]podTimingObservation `json:"pod_timings"`
	PodStartLatency  latencySummary                  `json:"pod_start_latency"`
	PodFinishLatency latencySummary                  `json:"pod_finish_latency"`
}

type podTimingObservation struct {
	NodeName             string    `json:"node_name"`
	ContainerStartedAt   time.Time `json:"container_started_at"`
	ContainerFinishedAt  time.Time `json:"container_finished_at"`
	StartLatencySeconds  float64   `json:"start_latency_seconds"`
	FinishLatencySeconds float64   `json:"finish_latency_seconds"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Terminated *struct {
						ExitCode   int       `json:"exitCode"`
						StartedAt  time.Time `json:"startedAt"`
						FinishedAt time.Time `json:"finishedAt"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

func (b *benchmark) runPullJob(ctx context.Context, state benchmarkState, phase proxyPhase, image string) (jobObservation, error) {
	phaseLabel := strings.ReplaceAll(string(phase), "_", "-")

	jobName := "gantry-benchmark-" + phaseLabel + "-" + state.RunID
	if len(jobName) > 63 {
		return jobObservation{}, fmt.Errorf("generated Job name %q exceeds 63 characters", jobName)
	}

	platformParts := strings.SplitN(b.config.ImagePlatform, "/", 2)
	if len(platformParts) != 2 || platformParts[0] == "" || platformParts[1] == "" {
		return jobObservation{}, fmt.Errorf("image platform BENCHMARK_IMAGE_PLATFORM=%q must have os/architecture form", b.config.ImagePlatform)
	}

	podLabels := map[string]string{
		"app.kubernetes.io/name":           "gantry-benchmark-pull",
		"app.kubernetes.io/part-of":        "gantry-benchmark",
		"gantry.unbounded-cloud.io/run-id": state.RunID,
		"gantry.unbounded-cloud.io/phase":  phaseLabel,
	}
	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      jobName,
			"namespace": b.config.Namespace,
			"labels":    podLabels,
		},
		"spec": map[string]any{
			"completions":             b.config.NodeCount,
			"parallelism":             b.config.NodeCount,
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 86400,
			"activeDeadlineSeconds":   int64(b.config.JobTimeout.Seconds()),
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec": map[string]any{
					"restartPolicy": "Never",
					"nodeSelector": map[string]string{
						"kubernetes.io/os":   platformParts[0],
						"kubernetes.io/arch": platformParts[1],
					},
					"tolerations": []any{map[string]any{"operator": "Exists"}},
					"affinity": map[string]any{
						"podAntiAffinity": map[string]any{
							"requiredDuringSchedulingIgnoredDuringExecution": []any{
								map[string]any{
									"labelSelector": map[string]any{
										"matchLabels": map[string]string{
											"gantry.unbounded-cloud.io/run-id": state.RunID,
											"gantry.unbounded-cloud.io/phase":  phaseLabel,
										},
									},
									"topologyKey": "kubernetes.io/hostname",
								},
							},
						},
					},
					"containers": []any{
						pullContainer(image),
					},
				},
			},
		},
	}

	phaseStartedAt := time.Now().UTC()

	if err := b.applyObject(ctx, job); err != nil {
		return jobObservation{}, err
	}

	waitContext, cancel := context.WithTimeout(ctx, b.config.JobTimeout)
	defer cancel()

	if _, err := b.commands.Run(
		waitContext,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"wait", "--for=condition=complete", "job/"+jobName,
		"--timeout", b.config.JobTimeout.String(),
	); err != nil {
		return jobObservation{}, err
	}

	phaseFinishedAt := time.Now().UTC()

	output, err := b.commands.Run(
		ctx,
		nil,
		"kubectl", "-n", b.config.Namespace,
		"get", "pods", "-l", "job-name="+jobName,
		"-o", "json",
	)
	if err != nil {
		return jobObservation{}, err
	}

	observation, err := parseJobObservation(output, b.config.NodeCount, phaseStartedAt)
	if err != nil {
		return jobObservation{}, err
	}

	observation.JobName = jobName
	observation.PhaseStartedAt = phaseStartedAt
	observation.PhaseFinishedAt = phaseFinishedAt

	return observation, nil
}

func pullContainer(image string) map[string]any {
	return map[string]any{
		"name":            "pull",
		"image":           image,
		"imagePullPolicy": "Always",
		// Keep the container Running long enough for kubelet to publish a
		// running status patch for audit-derived startup latency.
		"command": []string{"sh", "-c", "sleep 15"},
	}
}

func parseJobObservation(raw []byte, expectedPods int, phaseStartedAt time.Time) (jobObservation, error) {
	var pods podList
	if err := json.Unmarshal(raw, &pods); err != nil {
		return jobObservation{}, fmt.Errorf("decode pull Job pods: %w", err)
	}

	if len(pods.Items) != expectedPods {
		return jobObservation{}, fmt.Errorf("pull Job has %d pods, want %d", len(pods.Items), expectedPods)
	}

	nodeSet := make(map[string]struct{}, expectedPods)
	podNames := make([]string, 0, expectedPods)
	podNodes := make(map[string]string, expectedPods)
	podTimings := make(map[string]podTimingObservation, expectedPods)
	startLatencies := make([]time.Duration, 0, expectedPods)
	finishLatencies := make([]time.Duration, 0, expectedPods)

	for _, pod := range pods.Items {
		if pod.Status.Phase != "Succeeded" {
			return jobObservation{}, fmt.Errorf("pod %s phase is %q, want Succeeded", pod.Metadata.Name, pod.Status.Phase)
		}

		if pod.Spec.NodeName == "" {
			return jobObservation{}, fmt.Errorf("pod %s has no nodeName", pod.Metadata.Name)
		}

		if _, exists := nodeSet[pod.Spec.NodeName]; exists {
			return jobObservation{}, fmt.Errorf("multiple pull pods ran on node %s", pod.Spec.NodeName)
		}

		nodeSet[pod.Spec.NodeName] = struct{}{}
		podNames = append(podNames, pod.Metadata.Name)
		podNodes[pod.Metadata.Name] = pod.Spec.NodeName

		var terminatedFound bool

		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != "pull" || status.State.Terminated == nil {
				continue
			}

			terminated := status.State.Terminated
			if terminated.ExitCode != 0 {
				return jobObservation{}, fmt.Errorf("pod %s pull container exit code is %d", pod.Metadata.Name, terminated.ExitCode)
			}

			if terminated.StartedAt.Before(phaseStartedAt) || terminated.FinishedAt.Before(terminated.StartedAt) {
				return jobObservation{}, fmt.Errorf("pod %s has invalid container timestamps", pod.Metadata.Name)
			}

			startLatencies = append(startLatencies, terminated.StartedAt.Sub(phaseStartedAt))
			finishLatencies = append(finishLatencies, terminated.FinishedAt.Sub(phaseStartedAt))
			podTimings[pod.Metadata.Name] = podTimingObservation{
				NodeName:             pod.Spec.NodeName,
				ContainerStartedAt:   terminated.StartedAt,
				ContainerFinishedAt:  terminated.FinishedAt,
				StartLatencySeconds:  terminated.StartedAt.Sub(phaseStartedAt).Seconds(),
				FinishLatencySeconds: terminated.FinishedAt.Sub(phaseStartedAt).Seconds(),
			}
			terminatedFound = true

			break
		}

		if !terminatedFound {
			return jobObservation{}, fmt.Errorf("pod %s has no terminated pull container", pod.Metadata.Name)
		}
	}

	nodes := make([]string, 0, len(nodeSet))
	for node := range nodeSet {
		nodes = append(nodes, node)
	}

	sort.Strings(nodes)
	sort.Strings(podNames)

	return jobObservation{
		Nodes:            nodes,
		Pods:             podNames,
		PodNodes:         podNodes,
		PodTimings:       podTimings,
		PodStartLatency:  summarizeLatencies(startLatencies),
		PodFinishLatency: summarizeLatencies(finishLatencies),
	}, nil
}

func summarizeLatencies(values []time.Duration) latencySummary {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left] < sorted[right] })

	return latencySummary{
		P50Seconds:  percentile(sorted, 0.50).Seconds(),
		P95Seconds:  percentile(sorted, 0.95).Seconds(),
		P100Seconds: percentile(sorted, 1.00).Seconds(),
	}
}

func percentile(sorted []time.Duration, quantile float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	index := int(quantile*float64(len(sorted)) + 0.999999999)
	if index < 1 {
		index = 1
	}

	if index > len(sorted) {
		index = len(sorted)
	}

	return sorted[index-1]
}
