// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperatorSelectImagePairChoosesNewestCompatibleRun(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}

	artifactRoot := t.TempDir()
	writeSelectorState(t, artifactRoot, selectorState("run-20260801-old", 1000, "a"))
	writeSelectorState(t, artifactRoot, selectorState("run-20260802-new", 1000, "d"))
	writeSelectorState(t, artifactRoot, selectorState("run-20260803-wrong", 5, "e"))
	randomShape := selectorState("run-20260804-random", 1000, "f")
	randomShape.WorkloadComparisonMode = workloadComparisonRandomShape
	writeSelectorState(t, artifactRoot, randomShape)

	command := exec.Command(
		filepath.Join(repoRoot, "hack/gantry-benchmark/operator-vm-select-image-pair.sh"),
		artifactRoot,
		"direct",
		"1000",
		"linux/amd64",
		"40960",
		"40",
		"gantry-benchmark-pull",
		"base.azurecr.io",
		"gantry.azurecr.io",
	)

	output, err := command.Output()
	if err != nil {
		t.Fatalf("select image pair: %v", err)
	}

	firstLine := strings.Split(strings.TrimSpace(string(output)), "\n")[0]

	fields := strings.Split(firstLine, "\t")
	if len(fields) != 4 || fields[0] != "run-20260802-new" {
		t.Fatalf("selection = %q, want newest compatible run", output)
	}
}

func TestOperatorSelectImagePairFailsWithoutMatch(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}

	artifactRoot := t.TempDir()
	writeSelectorState(t, artifactRoot, selectorState("run-20260803-wrong", 5, "e"))

	command := exec.Command(
		filepath.Join(repoRoot, "hack/gantry-benchmark/operator-vm-select-image-pair.sh"),
		artifactRoot,
		"direct",
		"1000",
		"linux/amd64",
		"40960",
		"40",
		"gantry-benchmark-pull",
		"base.azurecr.io",
		"gantry.azurecr.io",
	)
	if err := command.Run(); err == nil {
		t.Fatal("selector succeeded without a compatible pair")
	}
}

func selectorState(runID string, nodeCount int, baselineDigest string) benchmarkState {
	return benchmarkState{
		RunID:                  runID,
		Mode:                   benchmarkModeDirect,
		NodeCount:              nodeCount,
		ImagePlatform:          "linux/amd64",
		ImageSizeMiB:           40960,
		ImageLayers:            40,
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "base.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
		BaselineImage:          "base.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat(baselineDigest, 64),
		GantryColdImage:        "gantry.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("b", 64),
		WorkloadPayloadSHA256:  "sha256:" + strings.Repeat("c", 64),
	}
}

func writeSelectorState(t *testing.T, artifactRoot string, state benchmarkState) {
	t.Helper()

	directory := filepath.Join(artifactRoot, state.RunID)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(directory, "state.json"), encoded, 0o640); err != nil {
		t.Fatal(err)
	}
}
