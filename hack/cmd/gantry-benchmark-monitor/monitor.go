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

type rangeSample struct {
	Timestamp time.Time
	Value     float64
}

type rangeSeries struct {
	Metric  map[string]string
	Samples []rangeSample
}

type rangeResponse struct {
	Series []rangeSeries
}

type minuteBin struct {
	Minute       int
	PeerOutcomes map[string]float64
	Bytes        float64
}

type monitorSnapshot struct {
	RunID           string
	JobName         string
	PhaseStart      time.Time
	Now             time.Time
	FirstSample     time.Time
	LatestSample    time.Time
	RefreshInterval time.Duration
	NodeCount       int
	ExpectedBytes   float64
	Bins            []minuteBin
	PeerTotals      map[string]float64
	TotalBytes      float64
	Job             jobStatus
	PodStates       podStateCounts
	PodStateError   string
	Progress        progressGrid
	Resources       nodeResourceGrid
	GridError       string
	NodePage        int
	NodesPerPage    int
	Color           bool
}

func parseRangeResponse(raw []byte) (rangeResponse, error) {
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string    `json:"metric"`
				Values [][2]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return rangeResponse{}, fmt.Errorf("decode Prometheus range response: %w", err)
	}

	if envelope.Status != "success" {
		return rangeResponse{}, fmt.Errorf("prometheus range query status is %q", envelope.Status)
	}

	response := rangeResponse{Series: make([]rangeSeries, 0, len(envelope.Data.Result))}
	for _, rawSeries := range envelope.Data.Result {
		series := rangeSeries{Metric: rawSeries.Metric, Samples: make([]rangeSample, 0, len(rawSeries.Values))}
		for _, pair := range rawSeries.Values {
			var timestamp float64
			if err := json.Unmarshal(pair[0], &timestamp); err != nil {
				return rangeResponse{}, fmt.Errorf("decode Prometheus sample timestamp: %w", err)
			}

			var text string
			if err := json.Unmarshal(pair[1], &text); err != nil {
				return rangeResponse{}, fmt.Errorf("decode Prometheus sample value: %w", err)
			}

			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return rangeResponse{}, fmt.Errorf("parse Prometheus sample value %q: %w", text, err)
			}

			if math.IsNaN(value) || math.IsInf(value, 0) {
				continue
			}

			seconds, fraction := math.Modf(timestamp)
			series.Samples = append(series.Samples, rangeSample{
				Timestamp: time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC(),
				Value:     value,
			})
		}

		response.Series = append(response.Series, series)
	}

	return response, nil
}

func aggregateRange(response rangeResponse, phaseStart, now time.Time, expectedBytes float64, nodeCount int) monitorSnapshot {
	elapsed := now.Sub(phaseStart)
	if elapsed < 0 {
		elapsed = 0
	}

	binCount := int(elapsed/time.Minute) + 1

	bins := make([]minuteBin, binCount)
	for index := range bins {
		bins[index] = minuteBin{Minute: index, PeerOutcomes: map[string]float64{}}
	}

	totals := map[string]float64{}
	latest := time.Time{}
	first := time.Time{}

	for _, series := range response.Series {
		sort.Slice(series.Samples, func(i, j int) bool {
			return series.Samples[i].Timestamp.Before(series.Samples[j].Timestamp)
		})

		if len(series.Samples) == 0 {
			continue
		}
		if first.IsZero() || series.Samples[0].Timestamp.Before(first) {
			first = series.Samples[0].Timestamp
		}

		outcome := series.Metric["outcome"]

		previous := series.Samples[0].Value
		if series.Samples[0].Timestamp.After(latest) {
			latest = series.Samples[0].Timestamp
		}

		for _, sample := range series.Samples[1:] {
			if sample.Timestamp.After(latest) {
				latest = sample.Timestamp
			}

			delta := sample.Value - previous

			previous = sample.Value
			if delta <= 0 || sample.Timestamp.Before(phaseStart) {
				continue
			}

			minute := int(sample.Timestamp.Sub(phaseStart) / time.Minute)
			if minute < 0 || minute >= len(bins) {
				continue
			}

			if outcome == "" {
				bins[minute].Bytes += delta
				continue
			}

			bins[minute].PeerOutcomes[outcome] += delta
			totals[outcome] += delta
		}
	}

	totalBytes := 0.0
	for _, bin := range bins {
		totalBytes += bin.Bytes
	}

	return monitorSnapshot{
		FirstSample:   first,
		LatestSample:  latest,
		NodeCount:     nodeCount,
		ExpectedBytes: expectedBytes,
		Bins:          bins,
		PeerTotals:    totals,
		TotalBytes:    totalBytes,
	}
}

func hasPartialTelemetry(snapshot monitorSnapshot) bool {
	return !snapshot.FirstSample.IsZero() && snapshot.FirstSample.After(snapshot.PhaseStart.Add(2*gridQueryInterval))
}

func commaInteger(value float64) string {
	text := strconv.FormatInt(int64(math.Round(value)), 10)

	start := 0
	if strings.HasPrefix(text, "-") {
		start = 1
	}

	for index := len(text) - 3; index > start; index -= 3 {
		text = text[:index] + "," + text[index:]
	}

	return text
}

func percentage(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}

	return numerator / denominator * 100
}

func binDuration(snapshot monitorSnapshot, minute int) float64 {
	if minute < len(snapshot.Bins)-1 {
		return 60
	}

	seconds := snapshot.Now.Sub(snapshot.PhaseStart.Add(time.Duration(minute) * time.Minute)).Seconds()
	if seconds < 1 {
		return 1
	}

	if seconds > 60 {
		return 60
	}

	return seconds
}

func renderPeerTable(builder *strings.Builder, snapshot monitorSnapshot) {
	fmt.Fprintln(builder, "=== Peer fetch outcomes by phase minute ===")
	fmt.Fprintf(builder, "%4s %12s %10s %10s %10s %12s\n", "min", "busy", "hit", "stall", "notfound", "unavailable")

	for _, bin := range snapshot.Bins {
		minute := strconv.Itoa(bin.Minute)
		if bin.Minute == len(snapshot.Bins)-1 {
			minute += "*"
		}

		fmt.Fprintf(builder, "%4s %12s %10s %10s %10s %12s\n",
			minute,
			commaInteger(bin.PeerOutcomes["busy"]),
			commaInteger(bin.PeerOutcomes["hit"]),
			commaInteger(bin.PeerOutcomes["stall"]),
			commaInteger(bin.PeerOutcomes["notfound"]),
			commaInteger(bin.PeerOutcomes["unavailable"]),
		)
	}

	fmt.Fprintf(builder, "\nTOTAL %10s %10s %10s %10s %12s\n",
		commaInteger(snapshot.PeerTotals["busy"]),
		commaInteger(snapshot.PeerTotals["hit"]),
		commaInteger(snapshot.PeerTotals["stall"]),
		commaInteger(snapshot.PeerTotals["notfound"]),
		commaInteger(snapshot.PeerTotals["unavailable"]),
	)

	firstSixBusy := 0.0
	firstSixHit := 0.0

	for _, bin := range snapshot.Bins {
		if bin.Minute >= 6 {
			break
		}

		firstSixBusy += bin.PeerOutcomes["busy"]
		firstSixHit += bin.PeerOutcomes["hit"]
	}

	if !hasPartialTelemetry(snapshot) && snapshot.PeerTotals["busy"] > 0 {
		fmt.Fprintf(builder, "\nbusy in first 6 min: %s of %s = %.1f%%\n",
			commaInteger(firstSixBusy), commaInteger(snapshot.PeerTotals["busy"]), percentage(firstSixBusy, snapshot.PeerTotals["busy"]))
	}

	if !hasPartialTelemetry(snapshot) && snapshot.PeerTotals["hit"] > 0 {
		fmt.Fprintf(builder, "hit  in first 6 min: %s of %s = %.1f%%\n",
			commaInteger(firstSixHit), commaInteger(snapshot.PeerTotals["hit"]), percentage(firstSixHit, snapshot.PeerTotals["hit"]))
	}
}

func renderByteTable(builder *strings.Builder, snapshot monitorSnapshot) {
	partial := hasPartialTelemetry(snapshot)
	fmt.Fprintln(builder, "=== Layer bytes delivered by phase minute ===")
	fmt.Fprintf(builder, "%4s %12s %16s %14s %8s\n", "min", "GB moved", "GB/s all-nodes", "MB/s per node", "coverage")

	cumulative := 0.0
	for _, bin := range snapshot.Bins {
		cumulative += bin.Bytes
		seconds := binDuration(snapshot, bin.Minute)
		allNodesGBs := bin.Bytes / seconds / 1e9

		perNodeMBs := 0.0
		if snapshot.NodeCount > 0 {
			perNodeMBs = bin.Bytes / seconds / float64(snapshot.NodeCount) / 1e6
		}

		minute := strconv.Itoa(bin.Minute)
		if bin.Minute == len(snapshot.Bins)-1 {
			minute += "*"
		}

		coverage := fmt.Sprintf("%.1f%%", percentage(cumulative, snapshot.ExpectedBytes))
		if partial {
			coverage = "partial"
		}

		fmt.Fprintf(builder, "%4s %12s %16.1f %14.1f %8s\n",
			minute,
			commaInteger(bin.Bytes/1e9),
			allNodesGBs,
			perNodeMBs,
			coverage,
		)
	}

	if partial {
		fmt.Fprintf(builder, "\ntotal %.3f TB captured since %s; full-phase percentage unavailable\n",
			snapshot.TotalBytes/1e12,
			snapshot.FirstSample.UTC().Format(time.RFC3339),
		)

		return
	}

	fmt.Fprintf(builder, "\ntotal %.3f TB of %.3f TB (%.1f%%)\n",
		snapshot.TotalBytes/1e12,
		snapshot.ExpectedBytes/1e12,
		percentage(snapshot.TotalBytes, snapshot.ExpectedBytes),
	)
}

func renderSnapshot(snapshot monitorSnapshot) string {
	var builder strings.Builder

	elapsed := snapshot.Now.Sub(snapshot.PhaseStart)
	if elapsed < 0 {
		elapsed = 0
	}

	titleStart, metaStart, statusStart, reset := "", "", "", ""
	if snapshot.Color {
		titleStart = "\033[1;36m"
		metaStart = "\033[2m"
		statusStart = "\033[1m"
		reset = "\033[0m"
	}

	fmt.Fprintf(&builder, "%sGantry benchmark live monitor%s\n", titleStart, reset)
	fmt.Fprintf(&builder, "%stime: %s\n", metaStart, snapshot.Now.UTC().Format(time.RFC3339))
	fmt.Fprintf(&builder, "run: %s\n", snapshot.RunID)
	fmt.Fprintf(&builder, "job: %s\n", snapshot.JobName)
	fmt.Fprintf(&builder, "phase started: %s (elapsed %s)%s\n", snapshot.PhaseStart.UTC().Format(time.RFC3339), elapsed.Round(time.Second), reset)
	fmt.Fprintf(&builder, "%spods: %d/%d completed | %d running | %d creating | %d image-pull | %d failed%s\n",
		statusStart,
		snapshot.PodStates.Completed,
		snapshot.NodeCount,
		snapshot.PodStates.Running,
		snapshot.PodStates.Creating,
		snapshot.PodStates.ImagePull,
		snapshot.PodStates.Failed,
		reset,
	)

	if snapshot.PodStates.Unscheduled > 0 || snapshot.PodStates.Other > 0 {
		fmt.Fprintf(&builder, "%spod detail: %d unscheduled, %d other%s\n", metaStart, snapshot.PodStates.Unscheduled, snapshot.PodStates.Other, reset)
	}

	if snapshot.PodStateError != "" {
		fmt.Fprintf(&builder, "%spod watch: %s%s\n", metaStart, snapshot.PodStateError, reset)
	}

	renderNodeResources(&builder, snapshot)
	renderProgressGrids(&builder, snapshot)

	fmt.Fprintf(&builder, "%sdisplay refresh: %s; Prometheus scrape cadence: 10s (values repeat between scrapes)\n", metaStart, snapshot.RefreshInterval)

	if !snapshot.LatestSample.IsZero() {
		fmt.Fprintf(&builder, "latest query sample: %s\n", snapshot.LatestSample.UTC().Format(time.RFC3339))
	}

	fmt.Fprintf(&builder, "*: current partial minute%s\n", reset)
	fmt.Fprintln(&builder)

	renderPeerTable(&builder, snapshot)
	fmt.Fprintln(&builder)
	renderByteTable(&builder, snapshot)

	return builder.String()
}
