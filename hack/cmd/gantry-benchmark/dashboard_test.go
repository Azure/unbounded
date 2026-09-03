// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderDashboardNamespaces(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("findRepoRoot: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(repoRoot, "hack", "gantry-benchmark", "grafana-dashboard.json"))
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}

	rendered, err := renderDashboard(raw, "custom-benchmark", "custom-gantry")
	if err != nil {
		t.Fatalf("renderDashboard: %v", err)
	}

	var dashboard struct {
		Templating struct {
			List []struct {
				Name    string `json:"name"`
				Current struct {
					Value string `json:"value"`
				} `json:"current"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(rendered, &dashboard); err != nil {
		t.Fatalf("decode rendered dashboard: %v", err)
	}

	values := make(map[string]string)
	for _, variable := range dashboard.Templating.List {
		values[variable.Name] = variable.Current.Value
	}

	if values["namespace"] != "custom-benchmark" || values["gantry_namespace"] != "custom-gantry" {
		t.Fatalf("rendered namespace variables = %v", values)
	}

	for _, metric := range []string{
		"gantry_origin_bytes_total",
		"gantry_peer_fetch_bytes_total",
		"gantry_peer_serve_bytes_total",
		"gantry_mirror_bytes_served_total",
	} {
		if !strings.Contains(string(rendered), metric) {
			t.Fatalf("rendered dashboard is missing byte metric %q", metric)
		}
	}
}
