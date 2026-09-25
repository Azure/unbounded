// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type artifactStreamingRunner struct {
	recordingRunner
}

func (r *artifactStreamingRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	_, _ = r.recordingRunner.Run(ctx, stdin, name, args...)

	return []byte(`{"status":"Succeeded"}`), nil
}

type artifactStreamingPollingRunner struct {
	recordingRunner
	outputs [][]byte
}

func (r *artifactStreamingPollingRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	_, _ = r.recordingRunner.Run(ctx, stdin, name, args...)
	output := r.outputs[0]
	r.outputs = r.outputs[1:]

	return output, nil
}

func TestPrepareArtifactStreaming(t *testing.T) {
	runner := &artifactStreamingRunner{}
	benchmark := &benchmark{
		config:   benchmarkConfig{GantryACRName: "benchstreamacr"},
		commands: runner,
	}
	state := benchmarkState{
		RunID:                "run_123",
		WorkloadRepository:   "gantry-benchmark-pull",
		GantryACRLoginServer: "benchstreamacr.azurecr.io",
	}
	image := "benchstreamacr.azurecr.io/gantry-benchmark-pull@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := benchmark.prepareArtifactStreaming(context.Background(), state, image); err != nil {
		t.Fatalf("prepareArtifactStreaming: %v", err)
	}

	want := [][]string{
		{"acr", "artifact-streaming", "update", "--name", "benchstreamacr", "--repository", "gantry-benchmark-pull", "--enable-streaming", "true", "--only-show-errors", "--output", "json"},
		{"acr", "artifact-streaming", "create", "--name", "benchstreamacr", "--image", "gantry-benchmark-pull@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--no-wait", "--only-show-errors", "--output", "json"},
	}
	if len(runner.commands) != len(want) {
		t.Fatalf("commands = %d, want %d", len(runner.commands), len(want))
	}

	for index, command := range runner.commands {
		if command.name != "az" || !reflect.DeepEqual(command.args, want[index]) {
			t.Fatalf("command %d = %s %v, want az %v", index, command.name, command.args, want[index])
		}
	}
}

func TestRequireArtifactStreamingSucceededRejectsIncompleteOperation(t *testing.T) {
	for name, output := range map[string]string{
		"pending":   `{"status":"Running"}`,
		"failed":    `{"status":"Failed"}`,
		"malformed": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := requireArtifactStreamingSucceeded([]byte(output)); err == nil {
				t.Fatal("operation unexpectedly succeeded")
			}
		})
	}
}

func TestPrepareArtifactStreamingPollsOperation(t *testing.T) {
	runner := &artifactStreamingPollingRunner{outputs: [][]byte{
		[]byte(`{"status":"Succeeded"}`),
		[]byte(`{"id":"operation-1","status":"Running"}`),
		[]byte(`{"status":"Succeeded"}`),
	}}
	benchmark := &benchmark{
		config: benchmarkConfig{
			GantryACRName:            "benchstreamacr",
			ArtifactStreamingTimeout: time.Second,
			ArtifactStreamingPoll:    time.Millisecond,
		},
		commands: runner,
		stdout:   io.Discard,
	}
	state := benchmarkState{
		WorkloadRepository:   "gantry-benchmark-pull",
		GantryACRLoginServer: "benchstreamacr.azurecr.io",
	}
	image := "benchstreamacr.azurecr.io/gantry-benchmark-pull@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := benchmark.prepareArtifactStreaming(context.Background(), state, image); err != nil {
		t.Fatalf("prepareArtifactStreaming: %v", err)
	}

	if len(runner.commands) != 3 {
		t.Fatalf("commands = %d, want 3", len(runner.commands))
	}
	status := runner.commands[2]
	if status.name != "az" || !slices.Contains(status.args, "operation-1") {
		t.Fatalf("status command = %s %v", status.name, status.args)
	}
}

func TestArtifactStreamingConfigIsOptIn(t *testing.T) {
	classic, err := loadBenchmarkConfig(envFromMap(nil))
	if err != nil {
		t.Fatalf("load classic config: %v", err)
	}
	if classic.ArtifactStreaming || classic.NodePool != "" {
		t.Fatalf("classic config ArtifactStreaming=%t NodePool=%q", classic.ArtifactStreaming, classic.NodePool)
	}

	streaming, err := loadBenchmarkConfig(envFromMap(map[string]string{
		"BENCHMARK_MODE":               "direct",
		"BENCHMARK_ARTIFACT_STREAMING": "true",
		"BENCHMARK_NODE_POOL":          "stream",
	}))
	if err != nil {
		t.Fatalf("load streaming config: %v", err)
	}
	if !streaming.ArtifactStreaming || streaming.NodePool != "stream" {
		t.Fatalf("streaming config ArtifactStreaming=%t NodePool=%q", streaming.ArtifactStreaming, streaming.NodePool)
	}

	if _, err := loadBenchmarkConfig(envFromMap(map[string]string{
		"BENCHMARK_ARTIFACT_STREAMING": "true",
	})); err == nil {
		t.Fatal("proxy mode unexpectedly accepted Artifact Streaming")
	}
}

func TestPreparedStandaloneRequiresArtifactStreamingConversion(t *testing.T) {
	state := benchmarkState{
		StandaloneGantry:      true,
		ArtifactStreaming:     true,
		GantryACRLoginServer:  "benchstreamacr.azurecr.io",
		GantryColdImage:       "benchstreamacr.azurecr.io/gantry-benchmark-pull@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkloadPayloadSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		WorkloadRepository:    "gantry-benchmark-pull",
	}

	if _, _, err := state.preparedImages(); err == nil {
		t.Fatal("preparedImages accepted an unconverted Artifact Streaming image")
	}

	state.ArtifactStreamingPrepared = true
	if _, _, err := state.preparedImages(); err != nil {
		t.Fatalf("preparedImages rejected converted image: %v", err)
	}
}

func TestValidateArtifactStreamingGantryConfig(t *testing.T) {
	const registry = "benchstreamacr.azurecr.io"
	raw := `artifact_streaming_enabled: true
upstream_registries:
  - name: benchstreamacr.azurecr.io
    endpoint: http://127.0.0.1:8578?ns=benchstreamacr.azurecr.io
`

	if err := validateArtifactStreamingGantryConfig(raw, registry); err != nil {
		t.Fatalf("validateArtifactStreamingGantryConfig: %v", err)
	}

	for name, invalid := range map[string]string{
		"disabled":   strings.Replace(raw, "artifact_streaming_enabled: true", "artifact_streaming_enabled: false", 1),
		"direct ACR": strings.Replace(raw, "http://127.0.0.1:8578?ns="+registry, "https://"+registry, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateArtifactStreamingGantryConfig(invalid, registry); err == nil {
				t.Fatal("validation unexpectedly passed")
			}
		})
	}
}
