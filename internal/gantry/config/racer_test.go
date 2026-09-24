// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"strings"
	"testing"
	"time"
)

func TestRacerConfiguration(t *testing.T) {
	c := NewDefault()
	if c.RacerMetadataTimeout != 3*time.Second || c.RacerMaxConcurrentTransfers != 64 {
		t.Fatal("wrong transfer defaults")
	}

	if c.ContentBackend != "direct" || c.RacerCacheUID != "" {
		t.Fatal("wrong defaults")
	}

	if err := c.LoadYAML(strings.NewReader("content_backend: racer\nracer_cache_uid: yaml-cache\n")); err != nil {
		t.Fatal(err)
	}

	if c.ContentBackend != "racer" || c.RacerCacheUID != "yaml-cache" {
		t.Fatal("YAML not applied")
	}

	if err := c.LoadEnv(func(k string) string {
		return map[string]string{"GANTRY_CONTENT_BACKEND": "direct", "GANTRY_RACER_CACHE_UID": "env-cache"}[k]
	}); err != nil {
		t.Fatal(err)
	}

	if c.ContentBackend != "direct" || c.RacerCacheUID != "env-cache" {
		t.Fatal("environment not applied")
	}

	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c.BindFlags(flags)

	if err := flags.Parse([]string{"--content-backend=racer", "--racer-cache-uid=flag-cache"}); err != nil {
		t.Fatal(err)
	}

	c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: "https://registry.example"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	if c.ContentBackend != "racer" || c.RacerCacheUID != "flag-cache" {
		t.Fatal("flags not applied")
	}

	c.ContentBackend = "unknown"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted unknown backend")
	}

	for _, uid := range []string{"", "../other", "a.b", "UPPER", "-edge", "edge-", "a/b", "a;id", strings.Repeat("a", 64)} {
		c.ContentBackend = "racer"

		c.RacerCacheUID = uid
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "racer_cache_uid") {
			t.Fatalf("invalid cache UID %q: %v", uid, err)
		}

		c.ContentBackend = "direct"
		if err := c.Validate(); err != nil {
			t.Fatalf("direct fallback must ignore cache UID %q: %v", uid, err)
		}
	}

	for _, uid := range []string{"a", "0", "synthetic-uid", "feee1f75-a659-46cd-a2d9-e1e7ee2c5318", strings.Repeat("a", 63)} {
		c.ContentBackend = "racer"

		c.RacerCacheUID = uid
		if err := c.Validate(); err != nil {
			t.Fatalf("valid cache UID %q: %v", uid, err)
		}
	}
}

func TestRacerTransferBudgetConfiguration(t *testing.T) {
	c := NewDefault()
	if err := c.LoadYAML(strings.NewReader("racer_metadata_timeout: 2s\nracer_max_concurrent_transfers: 12\n")); err != nil {
		t.Fatal(err)
	}

	if c.RacerMetadataTimeout != 2*time.Second || c.RacerMaxConcurrentTransfers != 12 {
		t.Fatal("YAML budgets")
	}

	if err := c.LoadEnv(func(k string) string {
		return map[string]string{"GANTRY_RACER_METADATA_TIMEOUT": "4s", "GANTRY_RACER_MAX_CONCURRENT_TRANSFERS": "24"}[k]
	}); err != nil {
		t.Fatal(err)
	}

	if c.RacerMetadataTimeout != 4*time.Second || c.RacerMaxConcurrentTransfers != 24 {
		t.Fatal("environment budgets")
	}

	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c.BindFlags(flags)

	if err := flags.Parse([]string{"--racer-metadata-timeout=5s", "--racer-max-concurrent-transfers=32", "--peer-fetch-timeout=10m"}); err != nil {
		t.Fatal(err)
	}

	if c.RacerMetadataTimeout != 5*time.Second || c.RacerMaxConcurrentTransfers != 32 || c.PeerFetchTimeout != 10*time.Minute {
		t.Fatal("flag budgets")
	}

	c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: "https://registry.example"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	c.RacerMetadataTimeout = 0
	if err := c.Validate(); err == nil {
		t.Fatal("unbounded metadata accepted")
	}

	c.RacerMetadataTimeout = time.Second

	c.RacerMaxConcurrentTransfers = 0
	if err := c.Validate(); err == nil {
		t.Fatal("unbounded admission accepted")
	}
}

func TestGeneratedBackendFlagsOverrideLegacyConfiguration(t *testing.T) {
	for _, backend := range []string{"direct", "racer"} {
		t.Run(backend, func(t *testing.T) {
			c := NewDefault()
			if err := c.LoadYAML(strings.NewReader("content_backend: obsolete\nracer_cache_uid: ../stale\nupstream_registries:\n  - name: private.example\n    endpoint: https://private.example\n")); err != nil {
				t.Fatal(err)
			}

			if err := c.LoadEnv(func(key string) string {
				return map[string]string{"GANTRY_CONTENT_BACKEND": "obsolete-env", "GANTRY_RACER_CACHE_UID": "../stale-env"}[key]
			}); err != nil {
				t.Fatal(err)
			}

			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			c.BindFlags(flags)

			args := []string{"--content-backend=" + backend}
			if backend == "racer" {
				args = append(args, "--racer-cache-uid=selected")
			}

			if err := flags.Parse(args); err != nil {
				t.Fatal(err)
			}

			if err := c.Validate(); err != nil {
				t.Fatalf("generated flags did not override stale config: %v", err)
			}

			if c.ContentBackend != backend || (backend == "racer" && c.RacerCacheUID != "selected") || len(c.UpstreamRegistries) != 1 || c.UpstreamRegistries[0].Name != "private.example" {
				t.Fatalf("incorrect merged config: %#v", c)
			}
		})
	}
}

func TestBackendFlagsDoNotHideMalformedYAML(t *testing.T) {
	for _, payload := range []string{"[not: yaml", "unknown_field: value", "racer_cache_uid: [invalid, type]"} {
		c := NewDefault()
		if err := c.LoadYAML(strings.NewReader(payload)); err == nil {
			t.Fatalf("malformed configuration must remain an error: %q", payload)
		}
	}
}
