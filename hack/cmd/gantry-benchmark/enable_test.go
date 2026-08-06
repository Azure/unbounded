// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
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

	rendered, err := benchmark.renderManifest(proxyManifestPath, proxyManifestData{
		Namespace:       "gantry-benchmark",
		GantryNamespace: "gantry-system",
		MonitoringLabel: "kps",
		ProxyImage:      "bench.azurecr.io/acr-origin-proxy:test",
		ACRLoginServer:  "bench.azurecr.io",
		RunID:           "run-1",
	})
	if err != nil {
		t.Fatalf("renderManifest: %v", err)
	}

	if strings.Contains(string(rendered), "{{") {
		t.Fatalf("rendered manifest contains an unresolved template expression")
	}

	if !strings.Contains(string(rendered), `targetLabel: gantry_benchmark`) {
		t.Fatalf("rendered manifest is missing the benchmark scrape label")
	}

	kinds := decodeManifestKinds(t, rendered)

	want := []string{"Deployment", "Service", "PodMonitor"}
	if len(kinds) != len(want) {
		t.Fatalf("rendered kinds = %v, want %v", kinds, want)
	}

	for index := range want {
		if kinds[index] != want[index] {
			t.Fatalf("rendered kinds = %v, want %v", kinds, want)
		}
	}
}

// The Gantry PodMonitor lives outside the proxy template because direct mode
// installs no proxy but still needs gantry_benchmark-labelled agent samples.
func TestRenderMonitoringManifest(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	benchmark := &benchmark{config: benchmarkConfig{RepoRoot: repoRoot}}

	rendered, err := benchmark.renderManifest(monitoringManifestPath, proxyManifestData{
		Namespace:       "gantry-benchmark",
		GantryNamespace: "gantry-system",
		MonitoringLabel: "kps",
		NodeOS:          "linux",
		NodeArch:        "amd64",
		RunID:           "run-1",
	})
	if err != nil {
		t.Fatalf("renderManifest: %v", err)
	}

	if strings.Contains(string(rendered), "{{") {
		t.Fatalf("rendered manifest contains an unresolved template expression")
	}

	if !strings.Contains(string(rendered), `targetLabel: gantry_benchmark`) ||
		!strings.Contains(string(rendered), `- controller-revision-hash`) {
		t.Fatalf("rendered manifest is missing benchmark scrape or Gantry revision labels")
	}

	if strings.Count(string(rendered), `gantry_benchmark: "true"`) != 2 {
		t.Fatalf("rendered manifest does not label both benchmark PodMonitors for discovery")
	}

	if !strings.Contains(string(rendered), `action: keep`) {
		t.Fatalf("rendered manifest does not limit Gantry metric cardinality")
	}

	if !strings.Contains(string(rendered), `systemctl show --property MainPID --value containerd`) {
		t.Fatalf("rendered manifest does not validate the running containerd debug configuration")
	}

	if !strings.Contains(string(rendered), `- port: ctr-metrics`) || strings.Contains(string(rendered), `- port: containerd-metrics`) {
		t.Fatalf("rendered manifest does not use the Kubernetes-valid containerd metrics port name")
	}

	if !strings.Contains(string(rendered), `--web.listen-address=:29100`) {
		t.Fatalf("rendered manifest does not use the benchmark node-exporter port")
	}

	for _, metric := range []string{
		"p2p_peer_fetch_duration_seconds_(bucket|sum|count)",
		"p2p_dht_lookup_duration_seconds_(bucket|sum|count)",
		"gantry_peer_fetch_last_timestamp_seconds",
		"gantry_mirror_response_completed_timestamp_seconds",
		"gantry_containerd_commit_observation_duration_seconds_(bucket|sum|count)",
		"gantry_containerd_commit_latest_observation_duration_seconds",
		"node_uname_info",
		"node_disk_(read|written)_bytes_total",
		"node_network_speed_bytes",
		"node_network_(receive|transmit)_(bytes|drop|errs)_total",
		"containerd_.*|grpc_server_.*",
		"process_(cpu_seconds_total|resident_memory_bytes|virtual_memory_bytes)",
	} {
		if !strings.Contains(string(rendered), metric) {
			t.Fatalf("rendered manifest does not retain required metric %q", metric)
		}
	}

	if strings.Contains(string(rendered), "acr-origin-proxy") {
		t.Fatalf("monitoring manifest must not reference the proxy")
	}

	kinds := decodeManifestKinds(t, rendered)

	wantKinds := []string{"PodMonitor", "DaemonSet", "PodMonitor"}
	if !slices.Equal(kinds, wantKinds) {
		t.Fatalf("rendered kinds = %v, want %v", kinds, wantKinds)
	}
}

func TestContainerdBenchmarkManifest(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(repoRoot, "hack/gantry-benchmark/manifests/containerd.yaml"))
	if err != nil {
		t.Fatalf("read containerd manifest: %v", err)
	}

	wantKinds := []string{"ConfigMap", "DaemonSet"}
	if kinds := decodeManifestKinds(t, manifest); !slices.Equal(kinds, wantKinds) {
		t.Fatalf("manifest kinds = %v, want %v", kinds, wantKinds)
	}

	for _, setting := range []string{
		`level = "debug"`,
		`image_pull_progress_timeout = "15m"`,
		`max_concurrent_downloads = 6`,
		`systemd-run`,
	} {
		if !bytes.Contains(manifest, []byte(setting)) {
			t.Fatalf("containerd manifest is missing %q", setting)
		}
	}
}

func decodeManifestKinds(t *testing.T, rendered []byte) []string {
	t.Helper()

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

	return kinds
}
