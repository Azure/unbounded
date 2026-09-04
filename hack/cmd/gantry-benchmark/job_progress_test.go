// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func pullProgressFixture(t *testing.T, pods []map[string]any) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]any{"items": pods})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	return raw
}

func waitingPod(node, reason string) map[string]any {
	return map[string]any{
		"spec": map[string]any{"nodeName": node},
		"status": map[string]any{
			"phase": "Pending",
			"containerStatuses": []any{map[string]any{
				"name":  "pull",
				"state": map[string]any{"waiting": map[string]any{"reason": reason}},
			}},
		},
	}
}

func phasePod(node, phase string) map[string]any {
	return map[string]any{
		"spec":   map[string]any{"nodeName": node},
		"status": map[string]any{"phase": phase},
	}
}

func TestSummarizePullProgressClassifiesEveryPodState(t *testing.T) {
	raw := pullProgressFixture(t, []map[string]any{
		phasePod("node-a", "Succeeded"),
		phasePod("node-b", "Succeeded"),
		phasePod("node-c", "Running"),
		phasePod("node-d", "Failed"),
		waitingPod("node-e", "ContainerCreating"),
		waitingPod("node-f", "PodInitializing"),
		waitingPod("node-g", "ImagePullBackOff"),
		waitingPod("node-h", "ErrImagePull"),
		waitingPod("node-i", "CreateContainerConfigError"),
		// Scheduled with no waiting state: kubelet is pulling the image.
		phasePod("node-j", "Pending"),
		// No node assigned yet.
		phasePod("", "Pending"),
	})

	progress, err := summarizePullProgress(raw, 11)
	if err != nil {
		t.Fatalf("summarizePullProgress: %v", err)
	}

	for _, check := range []struct {
		name string
		got  int
		want int
	}{
		{"total", progress.Total, 11},
		{"succeeded", progress.Succeeded, 2},
		{"running", progress.Running, 1},
		{"failed", progress.Failed, 1},
		{"containerCreating", progress.ContainerCreating, 2},
		{"imagePullBackOff", progress.ImagePullBackOff, 2},
		{"other", progress.Other, 1},
		{"pullingImage", progress.PullingImage, 1},
		{"unscheduled", progress.Unscheduled, 1},
	} {
		if check.got != check.want {
			t.Errorf("%s = %d, want %d", check.name, check.got, check.want)
		}
	}

	rendered := progress.String()
	for _, want := range []string{
		"2/11 succeeded",
		"1 running",
		"2 creating",
		"1 unscheduled",
		"1 failed",
		"2 image-pull-backoff (ErrImagePull=1,ImagePullBackOff=1)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered progress %q is missing %q", rendered, want)
		}
	}
}

func TestSummarizePullProgressOmitsEmptyCategories(t *testing.T) {
	raw := pullProgressFixture(t, []map[string]any{
		phasePod("node-a", "Succeeded"),
		phasePod("node-b", "Succeeded"),
	})

	progress, err := summarizePullProgress(raw, 2)
	if err != nil {
		t.Fatalf("summarizePullProgress: %v", err)
	}

	if got := progress.String(); got != "2/2 succeeded" {
		t.Fatalf("rendered progress = %q, want %q", got, "2/2 succeeded")
	}
}

type progressPollRunner struct {
	mu     sync.Mutex
	calls  int
	output []byte
}

func (r *progressPollRunner) Run(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls++

	return r.output, nil
}

func (r *progressPollRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls
}

func TestStartPullJobProgressReportsUntilCancelled(t *testing.T) {
	runner := &progressPollRunner{
		output: pullProgressFixture(t, []map[string]any{
			phasePod("node-a", "Succeeded"),
			waitingPod("node-b", "ImagePullBackOff"),
		}),
	}

	var progress lockedBuffer

	bench := &benchmark{
		config: benchmarkConfig{
			Namespace:           "gantry-benchmark",
			NodeCount:           2,
			JobProgressInterval: time.Millisecond,
		},
		commands: runner,
		stdout:   &progress,
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := bench.startPullJobProgress(ctx, "pull-job", time.Now())

	deadline := time.After(10 * time.Second)

	for runner.callCount() == 0 {
		select {
		case <-deadline:
			cancel()
			<-stopped
			t.Fatal("progress reporter never polled")
		default:
		}
	}

	cancel()
	<-stopped

	if !strings.Contains(progress.String(), "1/2 succeeded") ||
		!strings.Contains(progress.String(), "image-pull-backoff") {
		t.Fatalf("progress output = %q", progress.String())
	}
}

func TestStartPullJobProgressDisabledWhenIntervalUnset(t *testing.T) {
	runner := &progressPollRunner{}
	bench := &benchmark{
		config:   benchmarkConfig{Namespace: "gantry-benchmark", NodeCount: 2},
		commands: runner,
		stdout:   &bytes.Buffer{},
	}

	stopped := bench.startPullJobProgress(context.Background(), "pull-job", time.Now())

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("disabled reporter did not close its channel")
	}

	if got := runner.callCount(); got != 0 {
		t.Fatalf("poll calls = %d, want 0", got)
	}
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}
