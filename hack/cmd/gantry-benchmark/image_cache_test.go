// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestCacheEvictorDaemonSetTargetsOnlyPreparedPair(t *testing.T) {
	images := []string{
		"baseline.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("a", 64),
		"gantry.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("b", 64),
	}
	daemonSet := cacheEvictorDaemonSet(
		"gantry-benchmark",
		map[string]string{"kubernetes.io/os": "linux"},
		"gantry-benchmark-pull",
		images,
	)

	script := daemonSetScript(t, daemonSet)
	command := exec.Command("sh", "-n")

	command.Stdin = bytes.NewBufferString(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, output)
	}

	for _, required := range []string{
		`labels."gantry.io/managed"==true`,
		`labels."gantry.io/repository"==`,
		`images rm --sync`,
		`content get`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("cache eviction script is missing %q", required)
		}
	}

	manifest := string(mustJSONMarshal(t, daemonSet))
	for _, image := range images {
		if !strings.Contains(manifest, image) {
			t.Errorf("cache eviction DaemonSet is missing image %s", image)
		}
	}

	if !strings.Contains(manifest, `"privileged":true`) || !strings.Contains(manifest, `"path":"/"`) {
		t.Fatal("cache eviction DaemonSet does not have privileged host-root access")
	}
}

func TestEvictPreparedImagesCoversEveryTargetNode(t *testing.T) {
	runner := &captureApplyRunner{}
	benchmark := &benchmark{
		config: benchmarkConfig{
			Namespace: "gantry-benchmark",
			NodeCount: 1,
		},
		commands: runner,
	}
	state := benchmarkState{
		RunID:                  "run-1",
		Mode:                   benchmarkModeDirect,
		WorkloadRepository:     "gantry-benchmark-pull",
		BaselineACRLoginServer: "baseline.azurecr.io",
		GantryACRLoginServer:   "gantry.azurecr.io",
		BaselineImage:          "baseline.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("a", 64),
		GantryColdImage:        "gantry.azurecr.io/gantry-benchmark-pull@sha256:" + strings.Repeat("b", 64),
		WorkloadPayloadSHA256:  "sha256:" + strings.Repeat("c", 64),
	}

	if err := benchmark.evictPreparedImages(context.Background(), state); err != nil {
		t.Fatalf("evictPreparedImages: %v", err)
	}

	if len(runner.applied) != 1 {
		t.Fatalf("applied objects = %d, want one cache-eviction DaemonSet", len(runner.applied))
	}
}

func mustJSONMarshal(t *testing.T, value any) []byte {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}
