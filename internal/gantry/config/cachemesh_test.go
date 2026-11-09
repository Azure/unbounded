// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

func cachemeshBase() *Config {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.ChairNamespace = "gantry"
	c.PodIP = "10.0.0.1"
	c.Cachemesh.Enabled = true

	return c
}

func TestCachemeshDefaultsDisabled(t *testing.T) {
	c := NewDefault()

	m := c.Cachemesh
	if m.Enabled {
		t.Fatal("cachemesh enabled by default")
	}

	if m.ChunkSize != 64<<20 || m.MetadataTTL != 5*time.Minute || m.LeaseName != "gantry-cachemesh" ||
		m.ChunkCacheBytes != 4<<30 || m.MaxNeighbors != 32 || !m.CacheTags || !m.VerifyDigest || m.Lookahead != 8 {
		t.Fatalf("unexpected defaults: %+v", m)
	}

	// Disabled cachemesh never adds validation errors, even with an
	// otherwise invalid block.
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.Cachemesh.ChunkSize = 3

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with disabled cachemesh: %v", err)
	}
}

// An explicit zero max_neighbors selects the cachemesh library default.
func TestCachemeshMaxNeighborsZeroAllowed(t *testing.T) {
	c := cachemeshBase()
	if err := c.LoadYAML(strings.NewReader("cachemesh:\n  enabled: true\n  max_neighbors: 0\n")); err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}

	if c.Cachemesh.MaxNeighbors != 0 {
		t.Fatalf("MaxNeighbors = %d, want 0", c.Cachemesh.MaxNeighbors)
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestCachemeshValidateEnabled(t *testing.T) {
	if err := cachemeshBase().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cases := map[string]func(c *Config){
		"cachemesh.namespace":                  func(c *Config) { c.ChairNamespace = "" },
		"pod_ip":                               func(c *Config) { c.PodIP = "" },
		"cachemesh.lease_name":                 func(c *Config) { c.Cachemesh.LeaseName = "" },
		"cachemesh.chunk_size":                 func(c *Config) { c.Cachemesh.ChunkSize = 3 << 20 },
		"cachemesh.metadata_ttl":               func(c *Config) { c.Cachemesh.MetadataTTL = time.Minute },
		"cachemesh.chunk_cache_bytes":          func(c *Config) { c.Cachemesh.ChunkCacheBytes = c.Cachemesh.ChunkSize },
		"cachemesh.lookahead":                  func(c *Config) { c.Cachemesh.Lookahead = 0 },
		"cachemesh.max_neighbors":              func(c *Config) { c.Cachemesh.MaxNeighbors = -1 },
		"cachemesh.partition_bits":             func(c *Config) { c.Cachemesh.PartitionBits = 30 },
		"cachemesh.max_concurrent_chunk_fills": func(c *Config) { c.Cachemesh.MaxConcurrentChunkFills = 0 },
		"cachemesh.chunk_attempts":             func(c *Config) { c.Cachemesh.ChunkAttempts = 0 },
		"cachemesh.chunk_timeout":              func(c *Config) { c.Cachemesh.ChunkTimeout = 0 },
	}
	for want, mutate := range cases {
		t.Run(want, func(t *testing.T) {
			c := cachemeshBase()
			mutate(c)

			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want error mentioning %q, got %v", want, err)
			}
		})
	}
}

func TestCachemeshNamespaceOverride(t *testing.T) {
	c := cachemeshBase()
	c.ChairNamespace = ""
	c.Cachemesh.Namespace = "mesh"

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if got := c.CachemeshNamespace(); got != "mesh" {
		t.Fatalf("CachemeshNamespace = %q", got)
	}
}

func TestCachemeshYAMLAndEnv(t *testing.T) {
	c := NewDefault()

	yaml := `
cachemesh:
  enabled: true
  chunk_size: 16777216
  max_neighbors: 32
  metadata_ttl: 10m
  cache_tags: false
`
	if err := c.LoadYAML(strings.NewReader(yaml)); err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}

	if !c.Cachemesh.Enabled || c.Cachemesh.ChunkSize != 16<<20 || c.Cachemesh.MaxNeighbors != 32 ||
		c.Cachemesh.MetadataTTL != 10*time.Minute || c.Cachemesh.CacheTags {
		t.Fatalf("YAML not applied: %+v", c.Cachemesh)
	}

	// Defaults for keys absent from the YAML block survive.
	if c.Cachemesh.LeaseName != DefaultCachemeshLeaseName || c.Cachemesh.Lookahead != 8 {
		t.Fatalf("defaults lost: %+v", c.Cachemesh)
	}

	env := map[string]string{
		"GANTRY_CACHEMESH_ENABLED":           "false",
		"GANTRY_CACHEMESH_CHUNK_SIZE":        "33554432",
		"GANTRY_CACHEMESH_MAX_NEIGHBORS":     "48",
		"GANTRY_CACHEMESH_CHUNK_CACHE_BYTES": "1073741824",
		"GANTRY_CACHEMESH_LOOKAHEAD":         "16",
		"GANTRY_CACHEMESH_HEDGE":             "false",
		"GANTRY_CACHEMESH_CHUNK_TIMEOUT":     "1m",
	}

	if err := c.LoadEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}

	m := c.Cachemesh
	if m.Enabled || m.ChunkSize != 32<<20 || m.MaxNeighbors != 48 || m.ChunkCacheBytes != 1<<30 ||
		m.Lookahead != 16 || m.Hedge || m.ChunkTimeout != time.Minute {
		t.Fatalf("env not applied: %+v", m)
	}

	bad := map[string]string{"GANTRY_CACHEMESH_LOOKAHEAD": "many"}

	if err := c.LoadEnv(func(k string) string { return bad[k] }); err == nil || !strings.Contains(err.Error(), "CACHEMESH_LOOKAHEAD") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestCachemeshUnknownYAMLKeyRejected(t *testing.T) {
	c := NewDefault()
	if err := c.LoadYAML(strings.NewReader("cachemesh:\n  enabld: true\n")); err == nil {
		t.Fatal("want unknown-field error")
	}
}
