// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"bytes"
	"os"
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
    chunkCacheBytes: 8589934592
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

	for _, want := range []string{
		"cachemesh:\n  enabled: true\n",
		"  chunk_cache_bytes: 8589934592\n",
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
	if !m.Enabled || m.ChunkCacheBytes != 8<<30 || m.ChunkSize != 32<<20 || m.MaxNeighbors != 32 ||
		m.ExpectedSize != 150 || m.Lookahead != 12 || m.MetadataTTL != 10*time.Minute || m.CacheTags {
		t.Fatalf("parsed cachemesh config: %+v", m)
	}

	c.PodIP = "10.0.0.1"
	if err := c.Validate(); err != nil {
		t.Fatalf("rendered config does not validate: %v", err)
	}
}
