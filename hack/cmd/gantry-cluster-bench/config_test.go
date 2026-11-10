// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"sigs.k8s.io/yaml"
)

func TestPatchGantryConfig(t *testing.T) {
	in := []byte(`mirror_listen: "0.0.0.0:5000"
upstream_registries:
  - name: "docker.io"
    endpoint: "https://registry-1.docker.io"
  - name: "10.0.0.1:5005"
    endpoint: "https://stale"
peer_fetch_timeout: "5m"
`)

	out, err := patchGantryConfig(in, "10.0.0.1:5005", "http", []string{"peer_fetch_timeout=15m", "transfer_max_concurrent_serves=10"})
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	if doc["mirror_listen"] != "0.0.0.0:5000" || doc["peer_fetch_timeout"] != "15m" || doc["transfer_max_concurrent_serves"] != float64(10) {
		t.Fatalf("unexpected doc %v", doc)
	}

	regs := doc["upstream_registries"].([]any)
	if len(regs) != 2 {
		t.Fatalf("want 2 registries, got %v", regs)
	}

	last := regs[1].(map[string]any)
	if last["name"] != "10.0.0.1:5005" || last["endpoint"] != "http://10.0.0.1:5005" {
		t.Fatalf("origin entry %v", last)
	}

	again, err := patchGantryConfig(out, "10.0.0.1:5005", "http", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := yaml.Unmarshal(again, &doc); err != nil {
		t.Fatal(err)
	}

	if n := len(doc["upstream_registries"].([]any)); n != 2 {
		t.Fatalf("patch is not idempotent: %d registries", n)
	}
}

func TestPatchGantryConfigEmpty(t *testing.T) {
	out, err := patchGantryConfig(nil, "h:1", "https", nil)
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "upstream_registries:\n- endpoint: https://h:1\n  name: h:1\n" {
		t.Fatalf("got %q", out)
	}
}

func TestPatchGantryConfigErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg, origin, scheme string
		sets                []string
	}{
		"empty origin":    {origin: "", scheme: "http"},
		"origin url":      {origin: "http://h:1", scheme: "http"},
		"bad scheme":      {origin: "h:1", scheme: "ftp"},
		"bad yaml":        {cfg: "a: [", origin: "h:1", scheme: "http"},
		"registries map":  {cfg: "upstream_registries: {a: b}", origin: "h:1", scheme: "http"},
		"set without eq":  {origin: "h:1", scheme: "http", sets: []string{"novalue"}},
		"set without key": {origin: "h:1", scheme: "http", sets: []string{"=v"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := patchGantryConfig([]byte(tc.cfg), tc.origin, tc.scheme, tc.sets); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
