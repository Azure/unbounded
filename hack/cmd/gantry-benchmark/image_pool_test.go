// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func poolTestStates() (benchmarkState, benchmarkState) {
	current := benchmarkState{
		RunID: "run-new", Mode: benchmarkModeDirect, NodeCount: 1000,
		ImagePlatform: "linux/amd64", ImageSizeMiB: 40960, ImageLayers: 40,
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "baseline.example",
		GantryACRLoginServer:   "gantry.example",
	}
	baseline := current
	baseline.RunID = "run-baseline"
	baseline.BaselineImage = "baseline.example/gantry-benchmark-pull@sha256:" + repeatHex("a")
	baseline.WorkloadPayloadSHA256 = "sha256:" + repeatHex("b")

	return current, baseline
}

func poolTestEntry(id, imageHex, payloadHex string) gantryImagePoolEntry {
	return gantryImagePoolEntry{
		SchemaVersion:        gantryImagePoolSchemaVersion,
		ID:                   id,
		CreatedAt:            time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC),
		Image:                "gantry.example/gantry-benchmark-pull@sha256:" + repeatHex(imageHex),
		PayloadSHA256:        "sha256:" + repeatHex(payloadHex),
		ImageSizeMiB:         40960,
		ImageLayers:          40,
		ImagePlatform:        "linux/amd64",
		WorkloadRepository:   "gantry-benchmark-pull",
		GantryACRLoginServer: "gantry.example",
	}
}

func TestValidateImagePoolEntry(t *testing.T) {
	current, baseline := poolTestStates()
	config := benchmarkConfig{
		Mode: benchmarkModeDirect, ImageSizeMiB: 40960, ImageLayers: 40,
		ImagePlatform: "linux/amd64", WorkloadRepository: "gantry-benchmark-pull",
		GantryACRLoginServer: "gantry.example",
	}
	entry := poolTestEntry("pool-a", "c", "d")

	if err := validateImagePoolEntry(entry, config, current, baseline); err != nil {
		t.Fatalf("validate matching entry: %v", err)
	}

	entry.ImageLayers++
	if err := validateImagePoolEntry(entry, config, current, baseline); err == nil {
		t.Fatal("expected mismatched layer count to fail")
	}

	entry = poolTestEntry("..", "c", "d")
	if err := validateImagePoolEntry(entry, config, current, baseline); err == nil {
		t.Fatal("expected unsafe entry ID to fail")
	}
}

func TestClaimImagePoolEntryMovesReadyEntryOnce(t *testing.T) {
	root := t.TempDir()
	current, baseline := poolTestStates()

	benchmark := &benchmark{config: benchmarkConfig{
		Mode: benchmarkModeDirect, ImagePoolRoot: root,
		ImageSizeMiB: 40960, ImageLayers: 40, ImagePlatform: "linux/amd64",
		WorkloadRepository: "gantry-benchmark-pull", GantryACRLoginServer: "gantry.example",
	}}
	if err := benchmark.ensureImagePoolDirectories(); err != nil {
		t.Fatal(err)
	}

	entry := poolTestEntry("pool-a", "c", "d")

	readyPath := benchmark.imagePoolReadyPath(entry.ID)
	if err := writeJSONAtomic(readyPath, entry); err != nil {
		t.Fatal(err)
	}

	claimed, gotReadyPath, claimedPath, err := benchmark.claimImagePoolEntry(current, baseline)
	if err != nil {
		t.Fatalf("claimImagePoolEntry: %v", err)
	}

	if claimed.ID != entry.ID || gotReadyPath != readyPath || claimed.ClaimedByRunID != current.RunID || claimed.ClaimedAt == nil {
		t.Fatalf("claimed entry = %#v, readyPath=%q", claimed, gotReadyPath)
	}

	if _, err := os.Stat(readyPath); !os.IsNotExist(err) {
		t.Fatalf("ready entry still exists: %v", err)
	}

	if _, err := os.Stat(claimedPath); err != nil {
		t.Fatalf("claimed entry missing: %v", err)
	}

	if _, _, _, err := benchmark.claimImagePoolEntry(current, baseline); err == nil {
		t.Fatal("second claim unexpectedly succeeded")
	}
}

func TestReadImagePoolDirectorySortsEntries(t *testing.T) {
	directory := t.TempDir()
	for _, id := range []string{"pool-z", "pool-a"} {
		if err := writeJSONAtomic(filepath.Join(directory, id+".json"), poolTestEntry(id, "c", "d")); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := readImagePoolDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 || entries[0].ID != "pool-a" || entries[1].ID != "pool-z" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestPoolEntryClaimFieldsOmitWhenCleared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool-a.json")
	entry := poolTestEntry("pool-a", "c", "d")
	entry.ClaimedByRunID = ""

	entry.ClaimedAt = nil
	if err := writeJSONAtomic(path, entry); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(content), "claimed_by_run_id") || strings.Contains(string(content), "claimed_at") {
		t.Fatalf("cleared claim fields persisted: %s", content)
	}
}

func TestParsePrebuildCount(t *testing.T) {
	if count, err := parsePrebuildCount("10"); err != nil || count != 10 {
		t.Fatalf("parsePrebuildCount = %d, %v", count, err)
	}

	if _, err := parsePrebuildCount("ten"); err == nil {
		t.Fatal("expected invalid count to fail")
	}
}

func TestPrebuildGantryImagesCreatesReadyEntriesAndCleansBuilds(t *testing.T) {
	poolRoot := filepath.Join(t.TempDir(), "pool")
	buildRoot := filepath.Join(t.TempDir(), "build")
	runner := &dualACRImageRunner{}

	var progress bytes.Buffer

	benchmark := &benchmark{
		config: benchmarkConfig{
			Mode:                 benchmarkModeDirect,
			ImagePoolRoot:        poolRoot,
			ImagePoolBuildRoot:   buildRoot,
			ContainerEngine:      "podman",
			ImagePlatform:        "linux/amd64",
			ImageSizeMiB:         2,
			ImageLayers:          2,
			WorkloadRepository:   "gantry-benchmark-pull",
			GantryACRLoginServer: "gantry.azurecr.io",
			GantryACRUsername:    "user",
			GantryACRPassword:    "password",
		},
		commands: runner,
		stdout:   &progress,
		stderr:   &progress,
	}

	if err := benchmark.prebuildGantryImages(context.Background(), 2); err != nil {
		t.Fatalf("prebuildGantryImages: %v", err)
	}

	entries, err := readImagePoolDirectory(benchmark.imagePoolReadyDirectory())
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 2 {
		t.Fatalf("ready entries = %d, want 2", len(entries))
	}

	if entries[0].PayloadSHA256 == entries[1].PayloadSHA256 {
		t.Fatal("pool entries unexpectedly reused random payload bytes")
	}

	buildFiles, err := os.ReadDir(buildRoot)
	if err != nil {
		t.Fatal(err)
	}

	if len(buildFiles) != 0 {
		t.Fatalf("pool build root contains %d entries after cleanup", len(buildFiles))
	}

	commands := strings.Join(runner.commands, "\n")
	if got := strings.Count(commands, "podman login "); got != 1 {
		t.Fatalf("registry login count = %d, want 1:\n%s", got, commands)
	}

	if got := strings.Count(commands, "podman push "); got != 2 {
		t.Fatalf("push count = %d, want 2:\n%s", got, commands)
	}

	if got := strings.Count(commands, "podman image rm -f "); got != 2 {
		t.Fatalf("local image removal count = %d, want 2:\n%s", got, commands)
	}

	if strings.Contains(commands, "kubectl") {
		t.Fatalf("standalone prebuild unexpectedly invoked Kubernetes:\n%s", commands)
	}
}
