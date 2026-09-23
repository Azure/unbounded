// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"flag"
	"strings"
	"testing"
)

func TestRacerConfiguration(t *testing.T) {
	c := NewDefault()
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
