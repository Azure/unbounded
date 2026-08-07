// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultNodesPerPage = 64

type instantSeries struct {
	Metric    map[string]string
	Timestamp time.Time
	Value     float64
}

type instantResponse struct {
	Series []instantSeries
}

type progressLayer struct {
	Index  int
	Digest string
}

type progressGrid struct {
	Image       string
	ImageDigest string
	Nodes       []string
	Layers      []progressLayer
	Downloaded  map[string]map[int]time.Time
	Unpacked    map[string]map[string]time.Time
	ImageStart  map[string]time.Time
	ImageDone   map[string]time.Time
	Latest      time.Time
}

func parseInstantResponse(raw []byte) (instantResponse, error) {
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string  `json:"metric"`
				Value  [2]json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return instantResponse{}, fmt.Errorf("decode Prometheus instant response: %w", err)
	}

	if envelope.Status != "success" {
		return instantResponse{}, fmt.Errorf("prometheus instant query status is %q", envelope.Status)
	}

	response := instantResponse{Series: make([]instantSeries, 0, len(envelope.Data.Result))}
	for _, rawSeries := range envelope.Data.Result {
		var timestamp float64
		if err := json.Unmarshal(rawSeries.Value[0], &timestamp); err != nil {
			return instantResponse{}, fmt.Errorf("decode Prometheus instant timestamp: %w", err)
		}

		var text string
		if err := json.Unmarshal(rawSeries.Value[1], &text); err != nil {
			return instantResponse{}, fmt.Errorf("decode Prometheus instant value: %w", err)
		}

		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return instantResponse{}, fmt.Errorf("parse Prometheus instant value %q: %w", text, err)
		}

		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}

		seconds, fraction := math.Modf(timestamp)
		response.Series = append(response.Series, instantSeries{
			Metric:    rawSeries.Metric,
			Timestamp: time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC(),
			Value:     value,
		})
	}

	return response, nil
}

func imageDigest(reference string) string {
	_, digest, ok := strings.Cut(reference, "@")
	if ok {
		return digest
	}

	return ""
}

func imageMatches(reference, target, targetDigest string) bool {
	if reference == target {
		return true
	}

	return targetDigest != "" && imageDigest(reference) == targetDigest
}

func aggregateProgressGrid(response instantResponse, nodes []string, image string) progressGrid {
	grid := progressGrid{
		Image:       image,
		ImageDigest: imageDigest(image),
		Nodes:       append([]string(nil), nodes...),
		Downloaded:  map[string]map[int]time.Time{},
		Unpacked:    map[string]map[string]time.Time{},
		ImageStart:  map[string]time.Time{},
		ImageDone:   map[string]time.Time{},
	}
	sort.Strings(grid.Nodes)

	layers := map[int]string{}

	for _, series := range response.Series {
		if series.Timestamp.After(grid.Latest) {
			grid.Latest = series.Timestamp
		}

		name, node := series.Metric["__name__"], series.Metric["node"]

		if node == "" {
			continue
		}

		switch name {
		case "gantry_layer_download_completed_timestamp_seconds":
			if grid.ImageDigest != "" && series.Metric["image_digest"] != grid.ImageDigest {
				continue
			}

			index, err := strconv.Atoi(series.Metric["layer_index"])
			if err != nil || index < 0 {
				continue
			}

			digest := series.Metric["layer_digest"]
			if digest == "" {
				continue
			}

			layers[index] = digest

			if series.Value > 0 {
				if grid.Downloaded[node] == nil {
					grid.Downloaded[node] = map[int]time.Time{}
				}

				grid.Downloaded[node][index] = time.Unix(0, int64(series.Value*float64(time.Second))).UTC()
			}
		case "gantry_benchmark_layer_unpacked_timestamp_seconds":
			if !imageMatches(series.Metric["image"], image, grid.ImageDigest) || series.Value <= 0 {
				continue
			}

			digest := series.Metric["layer_digest"]
			if digest == "" {
				continue
			}

			if grid.Unpacked[node] == nil {
				grid.Unpacked[node] = map[string]time.Time{}
			}

			grid.Unpacked[node][digest] = time.Unix(seriesValueSeconds(series.Value), 0).UTC()
		case "gantry_benchmark_image_unpack_started_timestamp_seconds":
			if imageMatches(series.Metric["image"], image, grid.ImageDigest) && series.Value > 0 {
				grid.ImageStart[node] = time.Unix(seriesValueSeconds(series.Value), 0).UTC()
			}
		case "gantry_benchmark_image_unpacked_timestamp_seconds":
			if imageMatches(series.Metric["image"], image, grid.ImageDigest) && series.Value > 0 {
				grid.ImageDone[node] = time.Unix(seriesValueSeconds(series.Value), 0).UTC()
			}
		}
	}

	indexes := make([]int, 0, len(layers))
	for index := range layers {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	grid.Layers = make([]progressLayer, 0, len(indexes))
	for _, index := range indexes {
		grid.Layers = append(grid.Layers, progressLayer{Index: index, Digest: layers[index]})
	}

	return grid
}

func seriesValueSeconds(value float64) int64 {
	return int64(math.Floor(value))
}

func phaseMinuteCell(completedAt, phaseStart time.Time) byte {
	if completedAt.IsZero() {
		return '.'
	}

	minute := int(completedAt.Sub(phaseStart) / time.Minute)
	if minute < 0 {
		minute = 0
	}

	if minute > 35 {
		minute = 35
	}

	const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	return digits[minute]
}

func unpackCell(grid progressGrid, node string) byte {
	if !grid.ImageDone[node].IsZero() {
		return '#'
	}

	if len(grid.Layers) == 0 {
		return '.'
	}

	completed := 0

	for _, layer := range grid.Layers {
		if !grid.Unpacked[node][layer.Digest].IsZero() {
			completed++
		}
	}

	if completed == 0 {
		if !grid.ImageStart[node].IsZero() {
			return '0'
		}

		return '.'
	}

	level := int(math.Ceil(float64(completed) / float64(len(grid.Layers)) * 9))
	if level < 1 {
		level = 1
	}

	if level > 9 {
		level = 9
	}

	return byte('0' + level)
}

func pageNodes(nodes []string, page, perPage int) ([]string, int, int) {
	if perPage <= 0 {
		perPage = defaultNodesPerPage
	}

	totalPages := max(1, (len(nodes)+perPage-1)/perPage)

	if page < 1 {
		page = 1
	}

	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * perPage
	end := min(start+perPage, len(nodes))

	return nodes[start:end], page, totalPages
}

func renderProgressGrids(builder *strings.Builder, snapshot monitorSnapshot) {
	if snapshot.GridError != "" {
		fmt.Fprintf(builder, "\nprogress grids unavailable: %s\n", snapshot.GridError)
		return
	}

	grid := snapshot.Progress
	if len(grid.Nodes) == 0 || len(grid.Layers) == 0 {
		fmt.Fprintln(builder, "\nprogress grids: waiting for per-layer samples")
		return
	}

	nodes, page, pages := pageNodes(grid.Nodes, snapshot.NodePage, snapshot.NodesPerPage)

	fmt.Fprintln(builder, "\n=== Layer downloads x nodes ===")
	fmt.Fprintf(builder, "page %d/%d; nodes %d; showing %s .. %s\n", page, pages, len(grid.Nodes), nodes[0], nodes[len(nodes)-1])
	renderNodeHeader(builder, nodes)

	downloaded := 0

	for _, layer := range grid.Layers {
		fmt.Fprintf(builder, "L%02d %s ", layer.Index, shortDigest(layer.Digest))

		for _, node := range nodes {
			cell := phaseMinuteCell(grid.Downloaded[node][layer.Index], snapshot.PhaseStart)
			builder.WriteByte(cell)

			if cell != '.' {
				downloaded++
			}
		}

		builder.WriteByte('\n')
	}

	fmt.Fprintf(builder, "shown cells downloaded: %d/%d; legend: .=pending, 0-9/A-Z=completion phase minute (Z=35+)\n", downloaded, len(nodes)*len(grid.Layers))

	fmt.Fprintln(builder, "\n=== Image unpack x nodes ===")
	fmt.Fprintf(builder, "image %s\n", shortDigest(grid.ImageDigest))
	renderNodeHeader(builder, nodes)
	fmt.Fprintf(builder, "image     ")

	for _, node := range nodes {
		builder.WriteByte(unpackCell(grid, node))
	}

	builder.WriteByte('\n')
	fmt.Fprintln(builder, "legend: .=not started, 0=started, 1-9=unpacked layer decile, #=image unpacked")
}

func renderNodeHeader(builder *strings.Builder, nodes []string) {
	for position := 0; position < 3; position++ {
		if position == 0 {
			fmt.Fprint(builder, "node[-3:] ")
		} else {
			fmt.Fprint(builder, "          ")
		}

		for _, node := range nodes {
			suffix := node

			if len(suffix) > 3 {
				suffix = suffix[len(suffix)-3:]
			}

			for len(suffix) <= position {
				suffix = " " + suffix
			}

			builder.WriteByte(suffix[position])
		}

		builder.WriteByte('\n')
	}
}

func shortDigest(value string) string {
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) > 8 {
		return value[:8]
	}

	return value
}
