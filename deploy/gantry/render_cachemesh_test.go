// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/internal/gantry/config"
)

// renderedAgentConfig returns config.yaml from a rendered ConfigMap.
func renderedAgentConfig(t *testing.T, dir string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, "configmap.yaml"))
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}

	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("unmarshal ConfigMap: %v", err)
	}

	return cm.Data["config.yaml"]
}

func TestDefaultRenderHasNoCachemesh(t *testing.T) {
	t.Parallel()

	for _, operator := range []bool{false, true} {
		dir := renderChart(t, operator)

		if cfg := renderedAgentConfig(t, dir); strings.Contains(cfg, "cachemesh") {
			t.Fatalf("operator=%v: default config renders cachemesh:\n%s", operator, cfg)
		}

		ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
		if err != nil {
			t.Fatal(err)
		}

		if bytes.Contains(ds, []byte("GANTRY_NODE_NAME")) {
			t.Fatalf("operator=%v: default DaemonSet sets GANTRY_NODE_NAME", operator)
		}

		if bytes.Contains(ds, []byte("cachemesh-chunks")) {
			t.Fatalf("operator=%v: default DaemonSet mounts the chunk dir", operator)
		}
	}
}

// Without a sizeLimit the chunk emptyDir renders as an empty object.
func TestRendersCachemeshChunkDirWithoutSizeLimit(t *testing.T) {
	t.Parallel()

	dir := renderChart(t, false, "--set", "gantry.cachemesh.enabled=true")

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(ds, []byte("- name: cachemesh-chunks\n          emptyDir:\n            {}\n")) {
		t.Fatalf("chunk emptyDir not rendered:\n%s", ds)
	}
}

// The chart rejects the removed in-memory budget key.
func TestRejectsChunkCacheBytes(t *testing.T) {
	t.Parallel()

	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte("gantry:\n  cachemesh:\n    enabled: true\n    chunkCacheBytes: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := helmTemplate(t, "--values", values); err == nil {
		t.Fatalf("chart accepted chunkCacheBytes:\n%s", out)
	}
}

func TestRendersEnabledCachemesh(t *testing.T) {
	t.Parallel()

	// A values file makes Helm read numbers as float64, the case where a
	// careless template prints 8.589934592e+09.
	values := filepath.Join(t.TempDir(), "values.yaml")

	err := os.WriteFile(values, []byte(`gantry:
  cachemesh:
    enabled: true
    expectedSize: 150
    maxNeighbors: 32
    chunkSize: 33554432
    diskCacheBytes: 8589934592
    memoryCacheBytes: 134217728
    pageSize: 2097152
    fillPages: 6
    inflightBytes: 536870912
    pageCache: drop
    persist: shared
    chunkDirSizeLimit: 10Gi
    lookahead: 12
    metadataTTL: 10m
    cacheTags: false
    hedge: true
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	dir := renderChart(t, false, "--values", values)
	cfgText := renderedAgentConfig(t, dir)

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(ds, []byte("name: GANTRY_NODE_NAME")) || !bytes.Contains(ds, []byte("fieldPath: spec.nodeName")) {
		t.Fatal("DaemonSet lacks GANTRY_NODE_NAME from spec.nodeName with cachemesh enabled")
	}

	// The chunk directory is an emptyDir at the agent's default chunk_dir.
	for _, want := range []string{
		"- name: cachemesh-chunks\n              mountPath: " + config.DefaultCachemeshChunkDir + "\n",
		"- name: cachemesh-chunks\n          emptyDir:\n            sizeLimit: 10Gi\n",
	} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	for _, want := range []string{
		"cachemesh:\n  enabled: true\n",
		"  disk_cache_bytes: 8589934592\n",
		"  memory_cache_bytes: 134217728\n",
		"  page_size: 2097152\n",
		"  fill_pages: 6\n",
		"  inflight_bytes: 536870912\n",
		`  page_cache: "drop"`,
		`  persist: "shared"`,
		"  chunk_size: 33554432\n",
		"  max_neighbors: 32\n",
		"  expected_size: 150\n",
		"  lookahead: 12\n",
		`  metadata_ttl: "10m"`,
		"  cache_tags: false\n",
		"  hedge: true\n",
	} {
		if !strings.Contains(cfgText, want) {
			t.Fatalf("rendered config lacks %q:\n%s", want, cfgText)
		}
	}

	// Unset knobs are left to the agent defaults.
	if strings.Contains(cfgText, "cell_size") || strings.Contains(cfgText, "chunk_timeout") {
		t.Fatalf("unset knobs rendered:\n%s", cfgText)
	}

	c := config.NewDefault()
	if err := c.LoadYAML(strings.NewReader(cfgText)); err != nil {
		t.Fatalf("agent rejects rendered config: %v", err)
	}

	m := c.Cachemesh
	if !m.Enabled || m.DiskCacheBytes != 8<<30 || m.MemoryCacheBytes != 128<<20 || m.PageSize != 2<<20 ||
		m.FillPages != 6 || m.InflightBytes != 512<<20 || m.PageCache != "drop" || m.Persist != "shared" || m.ChunkSize != 32<<20 || m.MaxNeighbors != 32 ||
		m.ExpectedSize != 150 || m.Lookahead != 12 || m.MetadataTTL != 10*time.Minute || m.CacheTags {
		t.Fatalf("parsed cachemesh config: %+v", m)
	}

	c.PodIP = "10.0.0.1"
	if err := c.Validate(); err != nil {
		t.Fatalf("rendered config does not validate: %v", err)
	}
}

// helmTemplate renders the chart with extra args and returns helm's output
// and error, without failing the test.
func helmTemplate(t *testing.T, extraArgs ...string) (string, error) {
	t.Helper()

	deployDir := filepath.Dir(sourceFile(t))

	helm := filepath.Join(deployDir, "..", "..", "bin", "helm")
	if _, err := os.Stat(helm); err != nil {
		var lookupErr error

		helm, lookupErr = exec.LookPath("helm")
		if lookupErr != nil {
			t.Fatal("helm not found; run make install-helm")
		}
	}

	args := append([]string{
		"template", "gantry", filepath.Join(deployDir, "chart"),
		"--namespace", "unbounded-system",
		"--set-string", "image.reference=gantry:test",
	}, extraArgs...)

	out, err := exec.Command(helm, args...).CombinedOutput()

	return string(out), err
}
