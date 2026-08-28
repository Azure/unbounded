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
		`crictl rmi`,
		`labels."gantry.io/managed"==true`,
		`labels."gantry.io/repository"==`,
		`target.digest==${digest}`,
		`images rm --sync`,
		`content prune references`,
		`content get`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("cache eviction script is missing %q", required)
		}
	}

	ordered := []string{`crictl rmi`, `leases rm`, `images rm --sync`, `content prune references`, `content get`}
	for i := 1; i < len(ordered); i++ {
		if strings.Index(script, ordered[i-1]) >= strings.Index(script, ordered[i]) {
			t.Errorf("cache eviction script must run %q before %q", ordered[i-1], ordered[i])
		}
	}

	crictlIndex := strings.Index(script, `crictl rmi`)

	leaseIndex := strings.Index(script, `leases rm`)
	if restoreIndex := strings.Index(script[crictlIndex:leaseIndex], `IFS=${old_ifs}`); restoreIndex < 0 {
		t.Fatal("cache eviction script must restore normal field splitting before deleting leases")
	}

	if imageIFSIndex := strings.Index(script[leaseIndex:], `IFS='|'`); imageIFSIndex < 0 {
		t.Fatal("cache eviction script must restore image-ref field splitting after deleting leases")
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
