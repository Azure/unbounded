// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRenderProxyManifest(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	benchmark := &benchmark{config: benchmarkConfig{RepoRoot: repoRoot}}

	rendered, err := benchmark.renderProxyManifest(proxyManifestData{
		Namespace:       "gantry-benchmark",
		GantryNamespace: "gantry-system",
		MonitoringLabel: "kps",
		ProxyImage:      "bench.azurecr.io/acr-origin-proxy:test",
		ACRLoginServer:  "bench.azurecr.io",
		RunID:           "run-1",
	})
	if err != nil {
		t.Fatalf("renderProxyManifest: %v", err)
	}

	if strings.Contains(string(rendered), "{{") {
		t.Fatalf("rendered manifest contains an unresolved template expression")
	}

	if !strings.Contains(string(rendered), `targetLabel: gantry_benchmark`) ||
		!strings.Contains(string(rendered), `- controller-revision-hash`) {
		t.Fatalf("rendered manifest is missing benchmark scrape or Gantry revision labels")
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)

	var kinds []string

	for {
		var object struct {
			Kind string `json:"kind"`
		}
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}

			t.Fatalf("decode rendered manifest: %v", err)
		}

		if object.Kind != "" {
			kinds = append(kinds, object.Kind)
		}
	}

	want := []string{"Deployment", "Service", "PodMonitor", "PodMonitor"}
	if len(kinds) != len(want) {
		t.Fatalf("rendered kinds = %v, want %v", kinds, want)
	}

	for index := range want {
		if kinds[index] != want[index] {
			t.Fatalf("rendered kinds = %v, want %v", kinds, want)
		}
	}
}
