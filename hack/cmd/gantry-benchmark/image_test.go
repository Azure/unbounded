// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteRandomPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.bin")
	if err := writeRandomPayload(path, 1024*1024+17); err != nil {
		t.Fatalf("writeRandomPayload: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat payload: %v", err)
	}

	if info.Size() != 1024*1024+17 {
		t.Fatalf("payload size = %d, want %d", info.Size(), 1024*1024+17)
	}
}

type dualACRImageRunner struct {
	commands []string
}

func (r *dualACRImageRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, name+" "+strings.Join(args, " "))

	if name == "podman" && len(args) > 0 && args[0] == "push" {
		digestPath := ""

		for index, arg := range args {
			if arg == "--digestfile" && index+1 < len(args) {
				digestPath = args[index+1]
			}
		}

		digest := "sha256:" + strings.Repeat("a", 64)
		if strings.Contains(args[len(args)-1], "gantry.azurecr.io") {
			digest = "sha256:" + strings.Repeat("b", 64)
		}

		if err := os.WriteFile(digestPath, []byte(digest+"\n"), 0o600); err != nil {
			return nil, err
		}
	}

	return nil, nil
}

func TestBuildDualACRImagesUsesSharedPayloadAndSameImageName(t *testing.T) {
	runner := &dualACRImageRunner{}

	var progress bytes.Buffer

	benchmark := &benchmark{
		config: benchmarkConfig{
			StateRoot:          t.TempDir(),
			ContainerEngine:    "podman",
			ImagePlatform:      "linux/amd64",
			ImageSizeMiB:       2,
			ImageLayers:        2,
			WorkloadRepository: "benchmark-pull",
		},
		commands: runner,
		stdout:   &progress,
	}
	state := benchmarkState{
		RunID:                  "run-1",
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}

	baseline, gantry, payloadSHA, err := benchmark.buildDualACRImages(context.Background(), state)
	if err != nil {
		t.Fatalf("buildDualACRImages: %v", err)
	}

	if !strings.HasPrefix(baseline, "baseline.azurecr.io/benchmark-pull@sha256:") ||
		!strings.HasPrefix(gantry, "gantry.azurecr.io/benchmark-pull@sha256:") {
		t.Fatalf("prepared images = %q and %q", baseline, gantry)
	}

	if !strings.HasPrefix(payloadSHA, "sha256:") || len(payloadSHA) != len("sha256:")+64 {
		t.Fatalf("payload SHA = %q", payloadSHA)
	}

	joinedCommands := strings.Join(runner.commands, "\n")
	for _, image := range []string{
		"baseline.azurecr.io/benchmark-pull:run-1",
		"gantry.azurecr.io/benchmark-pull:run-1",
	} {
		if !strings.Contains(joinedCommands, image) {
			t.Fatalf("commands are missing image %q:\n%s", image, joinedCommands)
		}
	}

	buildDirectory := filepath.Join(benchmark.config.StateRoot, state.RunID, "build", "shared-payload")

	baselineDockerfile, err := os.ReadFile(filepath.Join(buildDirectory, "Dockerfile.baseline"))
	if err != nil {
		t.Fatal(err)
	}

	gantryDockerfile, err := os.ReadFile(filepath.Join(buildDirectory, "Dockerfile.gantry_cold"))
	if err != nil {
		t.Fatal(err)
	}

	for _, dockerfile := range [][]byte{baselineDockerfile, gantryDockerfile} {
		if !strings.Contains(string(dockerfile), payloadSHA) {
			t.Fatalf("Dockerfile is missing shared payload SHA:\n%s", dockerfile)
		}
	}

	if !strings.Contains(string(baselineDockerfile), "/gantry-benchmark-payload/baseline/") ||
		!strings.Contains(string(gantryDockerfile), "/gantry-benchmark-payload/gantry-cold/") ||
		string(baselineDockerfile) == string(gantryDockerfile) {
		t.Fatalf("phase Dockerfiles do not isolate content cache:\nbaseline:\n%s\nGantry:\n%s", baselineDockerfile, gantryDockerfile)
	}

	for _, want := range []string{
		"payload layer 1/2",
		"payload layer 2/2",
		"payload generation complete",
		"shared payload fingerprint sha256:",
		"image 1 of 2: baseline -> baseline.azurecr.io/benchmark-pull:run-1",
		"image 2 of 2: Gantry -> gantry.azurecr.io/benchmark-pull:run-1",
		"[baseline] build complete",
		"[baseline] push complete",
		"[gantry_cold] build complete",
		"[gantry_cold] push complete",
	} {
		if !strings.Contains(progress.String(), want) {
			t.Fatalf("progress output is missing %q:\n%s", want, progress.String())
		}
	}
}

func TestAdoptPreparedImages(t *testing.T) {
	state := benchmarkState{
		Mode:                   benchmarkModeDirect,
		Status:                 "enabled",
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}
	baseline := "baseline.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("a", 64)
	gantry := "gantry.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("b", 64)
	payload := "sha256:" + strings.Repeat("c", 64)

	adopted, err := adoptPreparedImages(state, baseline, gantry, payload)
	if err != nil {
		t.Fatalf("adoptPreparedImages: %v", err)
	}

	if adopted.Status != "images-prepared" || adopted.BaselineImage != baseline ||
		adopted.GantryColdImage != gantry || adopted.WorkloadPayloadSHA256 != payload ||
		adopted.WorkloadComparisonMode != workloadComparisonIdenticalPayload {
		t.Fatalf("adopted state = %+v", adopted)
	}
}

func TestAdoptPreparedImagesRejectsInvalidInputs(t *testing.T) {
	state := benchmarkState{
		Mode:                   benchmarkModeDirect,
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
	}
	digestValue := "sha256:" + strings.Repeat("a", 64)
	baseline := "baseline.azurecr.io/gantry-benchmark-pull@" + digestValue
	gantry := "gantry.azurecr.io/gantry-benchmark-pull@" + digestValue

	if _, err := adoptPreparedImages(state, baseline, gantry, "not-a-digest"); err == nil {
		t.Fatal("expected invalid payload digest rejection")
	}

	if _, err := adoptPreparedImages(state, baseline, gantry, "sha256:"+strings.Repeat("c", 64)); err == nil ||
		!strings.Contains(err.Error(), "would reuse") {
		t.Fatalf("error = %v, want identical image digest rejection", err)
	}
}
