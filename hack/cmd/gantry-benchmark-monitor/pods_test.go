// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func waitingPod(reason, node string) *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "pull",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: reason},
				},
			}},
		},
	}
}

func TestClassifyPod(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want podState
	}{
		{name: "completed", pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}, want: podStateCompleted},
		{name: "failed", pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}}, want: podStateFailed},
		{name: "running", pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}, want: podStateRunning},
		{name: "creating", pod: waitingPod("ContainerCreating", "node-a"), want: podStateCreating},
		{name: "initializing", pod: waitingPod("PodInitializing", "node-a"), want: podStateCreating},
		{name: "pull backoff", pod: waitingPod("ImagePullBackOff", "node-a"), want: podStateImagePull},
		{name: "pull error", pod: waitingPod("ErrImagePull", "node-a"), want: podStateImagePull},
		{name: "registry unavailable", pod: waitingPod("RegistryUnavailable", "node-a"), want: podStateImagePull},
		{name: "other waiting", pod: waitingPod("CreateContainerConfigError", "node-a"), want: podStateOther},
		{name: "unscheduled", pod: &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, want: podStateUnscheduled},
		{name: "scheduled pulling", pod: &corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-a"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}, want: podStateCreating},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := classifyPod(test.pod); got != test.want {
				t.Fatalf("classifyPod() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPodStateTrackerSnapshot(t *testing.T) {
	t.Parallel()

	tracker := &podStateTracker{
		states: map[string]podState{
			"a": podStateCompleted,
			"b": podStateRunning,
			"c": podStateCreating,
			"d": podStateImagePull,
			"e": podStateFailed,
			"f": podStateUnscheduled,
			"g": podStateOther,
		},
		err: errors.New("watch reconnecting"),
	}

	counts, err := tracker.snapshot()
	if err == nil || err.Error() != "watch reconnecting" {
		t.Fatalf("snapshot error = %v", err)
	}

	want := podStateCounts{Completed: 1, Running: 1, Creating: 1, ImagePull: 1, Failed: 1, Unscheduled: 1, Other: 1}
	if counts != want {
		t.Fatalf("counts = %#v, want %#v", counts, want)
	}
}

func TestPodStateTrackerSnapshotNodes(t *testing.T) {
	t.Parallel()

	tracker := &podStateTracker{nodes: map[string]string{
		"a": "node-c",
		"b": "node-a",
		"c": "node-c",
		"d": "",
	}}

	got := tracker.snapshotNodes()
	want := []string{"node-a", "node-c"}

	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("snapshotNodes() = %v, want %v", got, want)
	}
}
