// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestStageChartSetsImageRepository(t *testing.T) {
	source := filepath.Join(t.TempDir(), "chart")
	if err := os.MkdirAll(filepath.Join(source, "templates"), 0o755); err != nil {
		t.Fatalf("create source: %v", err)
	}

	writeTestFile(t, filepath.Join(source, "values.yaml"), "image:\n  repository: old.example/gantry\n  tag: dev\n")
	writeTestFile(t, filepath.Join(source, "templates", "daemonset.yaml"), "kind: DaemonSet\n")

	output := filepath.Join(t.TempDir(), "staged")
	if err := stageChart(options{source: source, output: output, imageRepository: "registry.example/project/gantry"}); err != nil {
		t.Fatalf("stage chart: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(output, "values.yaml"))
	if err != nil {
		t.Fatalf("read staged values: %v", err)
	}

	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode staged values: %v", err)
	}

	if values.Image.Repository != "registry.example/project/gantry" {
		t.Fatalf("image repository = %q", values.Image.Repository)
	}

	if _, err := os.Stat(filepath.Join(output, "templates", "daemonset.yaml")); err != nil {
		t.Fatalf("staged template: %v", err)
	}

	sourceRaw, err := os.ReadFile(filepath.Join(source, "values.yaml"))
	if err != nil {
		t.Fatalf("read source values: %v", err)
	}

	if string(sourceRaw) != "image:\n  repository: old.example/gantry\n  tag: dev\n" {
		t.Fatalf("source values changed: %s", sourceRaw)
	}
}

func TestStageChartRejectsExistingOutput(t *testing.T) {
	output := t.TempDir()

	err := stageChart(options{source: t.TempDir(), output: output, imageRepository: "registry.example/gantry"})
	if err == nil {
		t.Fatal("stageChart returned nil")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
