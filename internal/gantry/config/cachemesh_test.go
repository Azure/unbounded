// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func cachemeshBase() *Config {
	c := NewDefault()
	c.UpstreamRegistries = []UpstreamRegistry{{Name: "r", Endpoint: "https://r"}}
	c.ChairNamespace = "gantry"
	c.PodIP = "10.0.0.1"
	c.Cachemesh.Enabled = true
	c.Cachemesh.PeerKeyFile = "/etc/gantry/cachemesh/peer-key"

	return c
}

func TestCachemeshDefaultsDisabled(t *testing.T) {
	c := NewDefault()

	m := c.Cachemesh
	if m.Enabled {
		t.Fatal("cachemesh enabled by default")
	}

	if m.ChunkSize != 64<<20 || m.ContentRoot != "/var/lib/containerd/io.containerd.content.v1.content" ||
		m.InflightBytes != 2<<30 || m.MaxNeighbors != 32 || m.MaxConcurrentChunkFills != 64 || m.CacheTags || !m.VerifyDigest {
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

func TestCachemeshValidateEnabled(t *testing.T) {
	if err := cachemeshBase().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	cases := map[string]func(c *Config){
		"cachemesh.namespace":                  func(c *Config) { c.ChairNamespace = "" },
		"pod_ip":                               func(c *Config) { c.PodIP = "" },
		"cachemesh.peer_key_file":              func(c *Config) { c.Cachemesh.PeerKeyFile = "" },
		"cachemesh.peer_key_file relative":     func(c *Config) { c.Cachemesh.PeerKeyFile = "key" },
		"cachemesh.chunk_size":                 func(c *Config) { c.Cachemesh.ChunkSize = 3 << 20 },
		"cachemesh.expected_size":              func(c *Config) { c.Cachemesh.ExpectedSize = -1 },
		"cachemesh.content_root":               func(c *Config) { c.Cachemesh.ContentRoot = "rel/dir" },
		"cachemesh.inflight_bytes":             func(c *Config) { c.Cachemesh.InflightBytes = -1 },
		"cachemesh.max_neighbors":              func(c *Config) { c.Cachemesh.MaxNeighbors = 0 },
		"cachemesh.max_concurrent_chunk_fills": func(c *Config) { c.Cachemesh.MaxConcurrentChunkFills = 0 },
	}
	for name, mutate := range cases {
		want, _, _ := strings.Cut(name, " ")
		t.Run(name, func(t *testing.T) {
			c := cachemeshBase()
			mutate(c)

			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want error mentioning %q, got %v", want, err)
			}
		})
	}
}

// An empty content_root serves peers from memory only.
func TestCachemeshContentRootOptional(t *testing.T) {
	c := cachemeshBase()
	c.Cachemesh.ContentRoot = ""

	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
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
  peer_key_file: /keys/a
  max_neighbors: 48
  cache_tags: true
`
	if err := c.LoadYAML(strings.NewReader(yaml)); err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}

	if !c.Cachemesh.Enabled || c.Cachemesh.ChunkSize != 16<<20 || c.Cachemesh.MaxNeighbors != 48 || !c.Cachemesh.CacheTags ||
		c.Cachemesh.PeerKeyFile != "/keys/a" {
		t.Fatalf("YAML not applied: %+v", c.Cachemesh)
	}

	// Defaults for keys absent from the YAML block survive.
	if c.Cachemesh.InflightBytes != DefaultCachemeshInflightBytes || !c.Cachemesh.VerifyDigest {
		t.Fatalf("defaults lost: %+v", c.Cachemesh)
	}

	env := map[string]string{
		"GANTRY_CACHEMESH_ENABLED":                    "false",
		"GANTRY_CACHEMESH_NAMESPACE":                  "mesh",
		"GANTRY_CACHEMESH_PEER_KEY_FILE":              "/keys/b",
		"GANTRY_CACHEMESH_CHUNK_SIZE":                 "33554432",
		"GANTRY_CACHEMESH_EXPECTED_SIZE":              "150",
		"GANTRY_CACHEMESH_MAX_NEIGHBORS":              "64",
		"GANTRY_CACHEMESH_CACHE_TAGS":                 "true",
		"GANTRY_CACHEMESH_VERIFY_DIGEST":              "false",
		"GANTRY_CACHEMESH_CONTENT_ROOT":               "/data/content",
		"GANTRY_CACHEMESH_INFLIGHT_BYTES":             "4096",
		"GANTRY_CACHEMESH_MAX_CONCURRENT_CHUNK_FILLS": "16",
	}

	if err := c.LoadEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}

	want := CachemeshConfig{
		Namespace: "mesh", PeerKeyFile: "/keys/b", ChunkSize: 32 << 20, ExpectedSize: 150, MaxNeighbors: 64, CacheTags: true,
		ContentRoot: "/data/content", InflightBytes: 4096, MaxConcurrentChunkFills: 16,
	}
	if c.Cachemesh != want {
		t.Fatalf("env not applied: %+v", c.Cachemesh)
	}

	bad := map[string]string{"GANTRY_CACHEMESH_MAX_NEIGHBORS": "many"}

	if err := c.LoadEnv(func(k string) string { return bad[k] }); err == nil || !strings.Contains(err.Error(), "CACHEMESH_MAX_NEIGHBORS") {
		t.Fatalf("want parse error, got %v", err)
	}
}

func TestCachemeshUnknownYAMLKeyRejected(t *testing.T) {
	c := NewDefault()
	if err := c.LoadYAML(strings.NewReader("cachemesh:\n  enabld: true\n")); err == nil {
		t.Fatal("want unknown-field error")
	}
}
