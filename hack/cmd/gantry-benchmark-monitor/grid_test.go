// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseAndAggregateProgressGrid(t *testing.T) {
	phaseStart := time.Unix(1_000, 0).UTC()
	image := "registry.example/pull@sha256:image"
	raw := fmt.Sprintf(`{"status":"success","data":{"result":[
		{"metric":{"__name__":"gantry_layer_download_completed_timestamp_seconds","node":"node-a","image_digest":"sha256:image","layer_digest":"sha256:aaa","layer_index":"0"},"value":[%d,"%d"]},
		{"metric":{"__name__":"gantry_layer_download_completed_timestamp_seconds","node":"node-b","image_digest":"sha256:image","layer_digest":"sha256:aaa","layer_index":"0"},"value":[%d,"0"]},
		{"metric":{"__name__":"gantry_layer_download_completed_timestamp_seconds","node":"node-a","image_digest":"sha256:image","layer_digest":"sha256:bbb","layer_index":"1"},"value":[%d,"%d"]},
		{"metric":{"__name__":"gantry_benchmark_image_unpack_started_timestamp_seconds","node":"node-a","image":"%s"},"value":[%d,"%d"]},
		{"metric":{"__name__":"gantry_benchmark_layer_unpacked_timestamp_seconds","node":"node-a","image":"%s","layer_digest":"sha256:aaa"},"value":[%d,"%d"]},
		{"metric":{"__name__":"gantry_benchmark_image_unpacked_timestamp_seconds","node":"node-b","image":"%s"},"value":[%d,"%d"]}
	]}}`,
		phaseStart.Unix()+100, phaseStart.Unix()+30,
		phaseStart.Unix()+100,
		phaseStart.Unix()+100, phaseStart.Unix()+90,
		image, phaseStart.Unix()+100, phaseStart.Unix()+5,
		image, phaseStart.Unix()+100, phaseStart.Unix()+80,
		image, phaseStart.Unix()+100, phaseStart.Unix()+95,
	)

	response, err := parseInstantResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parseInstantResponse: %v", err)
	}

	grid := aggregateProgressGrid(response, []string{"node-b", "node-a", "node-c"}, image)
	if len(grid.Layers) != 2 || grid.Layers[0].Digest != "sha256:aaa" || grid.Layers[1].Digest != "sha256:bbb" {
		t.Fatalf("layers = %#v", grid.Layers)
	}

	if got := phaseMinuteCell(grid.Downloaded["node-a"][0], phaseStart); got != '0' {
		t.Fatalf("layer 0 cell = %q, want 0", got)
	}

	if got := phaseMinuteCell(grid.Downloaded["node-a"][1], phaseStart); got != '1' {
		t.Fatalf("layer 1 cell = %q, want 1", got)
	}

	if got := unpackCell(grid, "node-a"); got != '5' {
		t.Fatalf("node-a unpack cell = %q, want 5", got)
	}

	if got := unpackCell(grid, "node-b"); got != '#' {
		t.Fatalf("node-b unpack cell = %q, want #", got)
	}

	if got := unpackCell(grid, "node-c"); got != '.' {
		t.Fatalf("node-c unpack cell = %q, want .", got)
	}
}

func TestRenderProgressGridsPagesNodes(t *testing.T) {
	phaseStart := time.Unix(1_000, 0).UTC()
	grid := progressGrid{
		Image:       "registry.example/pull@sha256:image",
		ImageDigest: "sha256:image",
		Nodes:       []string{"node-001", "node-002", "node-003"},
		Layers:      []progressLayer{{Index: 0, Digest: "sha256:aaaaaaaa"}},
		Downloaded: map[string]map[int]time.Time{
			"node-001": {0: phaseStart.Add(2 * time.Minute)},
		},
		Unpacked:   map[string]map[string]time.Time{},
		ImageStart: map[string]time.Time{},
		ImageDone:  map[string]time.Time{},
	}

	var builder strings.Builder
	renderProgressGrids(&builder, monitorSnapshot{
		PhaseStart:   phaseStart,
		Progress:     grid,
		NodePage:     1,
		NodesPerPage: 2,
	})

	output := builder.String()
	for _, want := range []string{
		"=== Layer downloads x nodes ===",
		"page 1/2; nodes 3; showing node-001 .. node-002",
		"L00 aaaaaaaa 2.",
		"=== Image unpack x nodes ===",
		"image     ..",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("rendered grid missing %q:\n%s", want, output)
		}
	}
}

func TestPageNodesClampsPage(t *testing.T) {
	nodes, page, pages := pageNodes([]string{"a", "b", "c"}, 99, 2)
	if page != 2 || pages != 2 || len(nodes) != 1 || nodes[0] != "c" {
		t.Fatalf("pageNodes = %v, page %d/%d", nodes, page, pages)
	}
}
