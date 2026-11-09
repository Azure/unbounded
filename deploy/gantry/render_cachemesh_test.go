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

		if bytes.Contains(ds, []byte("containerd-content")) || bytes.Contains(ds, []byte("io.containerd.content.v1.content")) {
			t.Fatalf("operator=%v: default DaemonSet mounts the content store", operator)
		}
	}
}

// With cachemesh enabled the content store is mounted read-only from the
// host at the agent's default content_root, and no chunk volume exists.
func TestRendersCachemeshContentRootMount(t *testing.T) {
	t.Parallel()

	dir := renderChart(t, false, "--set", "gantry.cachemesh.enabled=true")

	ds, err := os.ReadFile(filepath.Join(dir, "daemonset.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	root := config.DefaultCachemeshContentRoot

	for _, want := range []string{
		"- name: containerd-content\n              mountPath: " + root + "\n              readOnly: true\n",
		"- name: containerd-content\n          hostPath:\n            path: " + root + "\n            type: Directory\n",
	} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	if bytes.Contains(ds, []byte("cachemesh-chunks")) || bytes.Contains(ds, []byte("emptyDir:\n            {}")) {
		t.Fatalf("DaemonSet still has a chunk volume:\n%s", ds)
	}

	if cfg := renderedAgentConfig(t, dir); !strings.Contains(cfg, `  content_root: "`+root+`"`) {
		t.Fatalf("config lacks content_root:\n%s", cfg)
	}
}

// The chart rejects the removed private chunk store keys.
func TestRejectsRemovedChunkStoreKeys(t *testing.T) {
	t.Parallel()

	for _, kv := range []string{"diskCacheBytes: 1", "chunkDirSizeLimit: 1Gi", "pageCache: keep", "persist: all", "contentRoot: relative"} {
		values := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(values, []byte("gantry:\n  cachemesh:\n    enabled: true\n    "+kv+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if out, err := helmTemplate(t, "--values", values); err == nil {
			t.Errorf("chart accepted %q:\n%s", kv, out)
		}
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
    contentRoot: /data/containerd/io.containerd.content.v1.content
    memoryCacheBytes: 134217728
    pageSize: 2097152
    fillPages: 6
    inflightBytes: 536870912
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

	// A custom content root is mounted at the same path.
	for _, want := range []string{
		"mountPath: /data/containerd/io.containerd.content.v1.content\n              readOnly: true\n",
		"path: /data/containerd/io.containerd.content.v1.content\n            type: Directory\n",
	} {
		if !bytes.Contains(ds, []byte(want)) {
			t.Fatalf("DaemonSet lacks %q:\n%s", want, ds)
		}
	}

	for _, want := range []string{
		"cachemesh:\n  enabled: true\n",
		`  content_root: "/data/containerd/io.containerd.content.v1.content"`,
		"  memory_cache_bytes: 134217728\n",
		"  page_size: 2097152\n",
		"  fill_pages: 6\n",
		"  inflight_bytes: 536870912\n",
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
	if !m.Enabled || m.ContentRoot != "/data/containerd/io.containerd.content.v1.content" || m.MemoryCacheBytes != 128<<20 || m.PageSize != 2<<20 ||
		m.FillPages != 6 || m.InflightBytes != 512<<20 || m.ChunkSize != 32<<20 || m.MaxNeighbors != 32 ||
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
