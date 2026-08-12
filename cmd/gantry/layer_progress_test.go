// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/metrics"
)

func TestLayerProgressTrackerBoundsSeriesToCurrentManifest(t *testing.T) {
	registry := metrics.New()
	phase := newPhase2Metrics(registry)
	now := time.Unix(123, 500_000_000)
	tracker := newLayerProgressTracker(phase.layerCompletedAt, "node-a", func() time.Time { return now })

	manifestA := digest.MustParse("sha256:" + strings.Repeat("a", 64))
	configA := digest.MustParse("sha256:" + strings.Repeat("b", 64))
	layerA0 := digest.MustParse("sha256:" + strings.Repeat("c", 64))
	layerA1 := digest.MustParse("sha256:" + strings.Repeat("d", 64))

	tracker.completed(layerA0)

	tracker.observeManifest(manifestA, []manifest.TypedChild{
		{Digest: configA, Kind: ifaces.KindConfig},
		{Digest: layerA0, Kind: ifaces.KindBlob},
		{Digest: layerA1, Kind: ifaces.KindBlob},
	})
	tracker.completed(layerA1)

	now = time.Unix(200, 0)

	tracker.completed(layerA1)

	series := layerProgressSeries(t, registry)
	if len(series) != 2 {
		t.Fatalf("series after first manifest = %d, want 2", len(series))
	}

	if got := series[layerA0.String()]; got != 123.5 {
		t.Fatalf("early completed layer value = %v, want 123.5", got)
	}

	if got := series[layerA1.String()]; got != 123.5 {
		t.Fatalf("completed layer value = %v, want 123.5", got)
	}

	manifestB := digest.MustParse("sha256:" + strings.Repeat("e", 64))
	layerB0 := digest.MustParse("sha256:" + strings.Repeat("f", 64))
	tracker.observeManifest(manifestB, []manifest.TypedChild{{Digest: layerB0, Kind: ifaces.KindBlob}})

	series = layerProgressSeries(t, registry)
	if len(series) != 1 {
		t.Fatalf("series after manifest replacement = %d, want 1", len(series))
	}

	if _, ok := series[layerB0.String()]; !ok {
		t.Fatalf("current layer %s missing from series %v", layerB0, series)
	}
}

func layerProgressSeries(t *testing.T, registry *metrics.Registry) map[string]float64 {
	t.Helper()

	families, err := registry.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	result := map[string]float64{}

	for _, family := range families {
		if family.GetName() != "gantry_layer_download_completed_timestamp_seconds" {
			continue
		}

		for _, metric := range family.Metric {
			digestLabel := ""

			for _, label := range metric.Label {
				if label.GetName() == "layer_digest" {
					digestLabel = label.GetValue()
				}
			}

			result[digestLabel] = metric.GetGauge().GetValue()
		}
	}

	return result
}
