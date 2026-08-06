// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexedPayloadPathsUsesNumericLayerOrder(t *testing.T) {
	root := t.TempDir()

	nested := filepath.Join(root, "gantry-cold")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"payload2.bin", "payload0.bin", "payload1.bin"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte(name), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := indexedPayloadPaths(root, 3)
	if err != nil {
		t.Fatal(err)
	}

	for index, path := range paths {
		want := filepath.Join(nested, "payload"+string(rune('0'+index))+".bin")
		if path != want {
			t.Fatalf("paths[%d] = %q, want %q", index, path, want)
		}
	}
}

func TestValidateGantryOnlySource(t *testing.T) {
	current := benchmarkState{
		RunID: "new", Mode: benchmarkModeDirect, NodeCount: 1000,
		ImagePlatform: "linux/amd64", ImageSizeMiB: 40960, ImageLayers: 40,
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "baseline.example", GantryACRLoginServer: "gantry.example",
	}
	baseline := current
	baseline.RunID = "old"
	baseline.BaselineImage = "baseline.example/gantry-benchmark-pull@sha256:" + repeatHex("a")
	baseline.WorkloadPayloadSHA256 = "sha256:" + repeatHex("b")
	result := phaseResult{
		RunID: "old", Phase: proxyPhaseBaseline, Image: baseline.BaselineImage,
		PayloadSHA: baseline.WorkloadPayloadSHA256,
		Job:        jobObservation{Nodes: make([]string, 1000)},
	}

	if err := validateGantryOnlySource(current, baseline, result); err != nil {
		t.Fatalf("validate matching source: %v", err)
	}

	result.PayloadSHA = "sha256:" + repeatHex("c")
	if err := validateGantryOnlySource(current, baseline, result); err == nil {
		t.Fatal("expected mismatched payload to fail")
	}
}

func TestValidatePreparedGantryOnlySource(t *testing.T) {
	current := benchmarkState{
		RunID: "new", Mode: benchmarkModeDirect, NodeCount: 1000,
		ImagePlatform: "linux/amd64", ImageSizeMiB: 40960, ImageLayers: 40,
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "baseline.example", GantryACRLoginServer: "gantry.example",
	}
	baseline := current
	baseline.WorkloadPayloadSHA256 = "sha256:" + repeatHex("b")
	prepared := current
	prepared.WorkloadPayloadSHA256 = baseline.WorkloadPayloadSHA256
	prepared.GantryColdImage = "gantry.example/gantry-benchmark-pull@sha256:" + repeatHex("c")

	if err := validatePreparedGantryOnlySource(current, baseline, prepared); err != nil {
		t.Fatalf("validate matching prepared source: %v", err)
	}

	prepared.ImageSizeMiB++
	if err := validatePreparedGantryOnlySource(current, baseline, prepared); err == nil {
		t.Fatal("expected mismatched image size to fail")
	}
}

func TestValidateAdoptedFreshGantryImage(t *testing.T) {
	current := benchmarkState{
		WorkloadRepository:    "gantry-benchmark-pull",
		GantryACRLoginServer:  "gantry.example",
		WorkloadPayloadSHA256: "sha256:" + repeatHex("b"),
	}
	baseline := current
	baseline.BaselineImage = "baseline.example/gantry-benchmark-pull@sha256:" + repeatHex("a")
	baseline.WorkloadPayloadSHA256 = "sha256:" + repeatHex("b")
	image := "gantry.example/gantry-benchmark-pull@sha256:" + repeatHex("c")
	payloadSHA := "sha256:" + repeatHex("d")

	if err := validateAdoptedFreshGantryImage(current, baseline, image, payloadSHA); err != nil {
		t.Fatalf("validate adopted image: %v", err)
	}

	tests := []struct {
		name       string
		image      string
		payloadSHA string
	}{
		{name: "tagged image", image: "gantry.example/gantry-benchmark-pull:fresh", payloadSHA: payloadSHA},
		{name: "wrong repository", image: "gantry.example/other@sha256:" + repeatHex("c"), payloadSHA: payloadSHA},
		{name: "invalid image digest", image: "gantry.example/gantry-benchmark-pull@sha256:invalid", payloadSHA: payloadSHA},
		{name: "invalid payload digest", image: image, payloadSHA: "sha256:invalid"},
		{name: "baseline payload", image: image, payloadSHA: baseline.WorkloadPayloadSHA256},
		{name: "baseline image digest", image: "gantry.example/gantry-benchmark-pull@sha256:" + repeatHex("a"), payloadSHA: payloadSHA},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateAdoptedFreshGantryImage(current, baseline, test.image, test.payloadSHA); err == nil {
				t.Fatal("expected adopted image validation to fail")
			}
		})
	}
}

func repeatHex(value string) string {
	result := ""
	for range 64 {
		result += value
	}

	return result
}
