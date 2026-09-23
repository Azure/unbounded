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

	if c.ContentBackend != "direct" || c.RacerCacheName != "gantry" {
		t.Fatal("wrong defaults")
	}

	if err := c.LoadYAML(strings.NewReader("content_backend: racer\nracer_cache_name: yaml-cache\n")); err != nil {
		t.Fatal(err)
	}

	if c.ContentBackend != "racer" || c.RacerCacheName != "yaml-cache" {
		t.Fatal("YAML not applied")
	}

	if err := c.LoadEnv(func(k string) string {
		return map[string]string{"GANTRY_CONTENT_BACKEND": "direct", "GANTRY_RACER_CACHE_NAME": "env-cache"}[k]
	}); err != nil {
		t.Fatal(err)
	}

	if c.ContentBackend != "direct" || c.RacerCacheName != "env-cache" {
		t.Fatal("environment not applied")
	}

	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c.BindFlags(flags)

	if err := flags.Parse([]string{"--content-backend=racer", "--racer-cache-name=flag-cache"}); err != nil {
		t.Fatal(err)
	}

	c.UpstreamRegistries = []UpstreamRegistry{{Name: "registry.example", Endpoint: "https://registry.example"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	if c.ContentBackend != "racer" || c.RacerCacheName != "flag-cache" {
		t.Fatal("flags not applied")
	}

	c.ContentBackend = "unknown"
	if err := c.Validate(); err == nil {
		t.Fatal("accepted unknown backend")
	}

	c.ContentBackend = "racer"
	for _, name := range []string{"", "../other", "a.b", strings.Repeat("a", 64)} {
		c.RacerCacheName = name
		if err := c.Validate(); err == nil {
			t.Fatal("accepted invalid cache name", name)
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
