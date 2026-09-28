// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type metricsRacerClient struct{ startupRacerClient }

func (*metricsRacerClient) Stats() racersdk.Stats {
	return racersdk.Stats{
		QueueDepth: 10, BulkQueueDepth: 2, MetadataQueueDepth: 3, SmallObjectQueueDepth: 5,
		ActiveBulk: 7, ActiveMetadata: 11, ActiveSmallObjects: 13,
		Connections: 37, IdleConnections: 6,
		Dials: 41, ConnectionReuses: 43, ConnectionRotations: 19, Retries: 2,
		QueueWaits: 3, QueueWaitNanoseconds: 1500000000, BytesRead: 17,
	}
}

func TestRacerProductionMetrics(t *testing.T) {
	reg := metrics.New()
	m := newRacerMetrics(reg, &metricsRacerClient{})
	m.mirrorResponse(mirror.RacerHTTPObservation{Method: http.MethodGet, Status: 206, Bytes: 11, Duration: time.Second, Aborted: true})
	m.originRequest(http.MethodHead, 200)
	m.originRequest(http.MethodGet, 206)
	m.originBytes("blob", 17)

	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "206", "aborted")); got != 1 {
		t.Fatalf("aborted responses=%v", got)
	}

	if got := testutil.ToFloat64(m.bytes.WithLabelValues("GET")); got != 11 {
		t.Fatalf("partial bytes=%v", got)
	}

	if got := testutil.ToFloat64(m.originRequests.WithLabelValues("HEAD", "200")); got != 1 {
		t.Fatalf("HEAD requests=%v", got)
	}

	if got := testutil.ToFloat64(m.originRequests.WithLabelValues("GET", "206")); got != 1 {
		t.Fatalf("GET requests=%v", got)
	}

	if got := testutil.ToFloat64(m.originBodyBytes.WithLabelValues("blob")); got != 17 {
		t.Fatalf("origin bytes=%v", got)
	}

	if err := testutil.GatherAndCompare(reg.PrometheusRegistry(), strings.NewReader(`
# HELP gantry_racer_sdk_queue_depth Calls waiting for SDK admission.
# TYPE gantry_racer_sdk_queue_depth gauge
gantry_racer_sdk_queue_depth 10
# HELP gantry_racer_sdk_bulk_queue_depth Calls waiting for SDK bulk admission.
# TYPE gantry_racer_sdk_bulk_queue_depth gauge
gantry_racer_sdk_bulk_queue_depth 2
# HELP gantry_racer_sdk_metadata_queue_depth Calls waiting for SDK metadata admission.
# TYPE gantry_racer_sdk_metadata_queue_depth gauge
gantry_racer_sdk_metadata_queue_depth 3
# HELP gantry_racer_sdk_small_object_queue_depth Calls waiting for SDK small-object admission.
# TYPE gantry_racer_sdk_small_object_queue_depth gauge
gantry_racer_sdk_small_object_queue_depth 5
# HELP gantry_racer_sdk_active_bulk Occupied bulk admission slots.
# TYPE gantry_racer_sdk_active_bulk gauge
gantry_racer_sdk_active_bulk 7
# HELP gantry_racer_sdk_active_metadata Occupied metadata admission slots.
# TYPE gantry_racer_sdk_active_metadata gauge
gantry_racer_sdk_active_metadata 11
# HELP gantry_racer_sdk_active_small_objects Occupied small-object admission slots.
# TYPE gantry_racer_sdk_active_small_objects gauge
gantry_racer_sdk_active_small_objects 13
# HELP gantry_racer_sdk_connections Open SDK connections across all three pools.
# TYPE gantry_racer_sdk_connections gauge
gantry_racer_sdk_connections 37
# HELP gantry_racer_sdk_idle_connections Reusable SDK connections across all three pools.
# TYPE gantry_racer_sdk_idle_connections gauge
gantry_racer_sdk_idle_connections 6
# HELP gantry_racer_sdk_dials_total SDK dial attempts, including failures.
# TYPE gantry_racer_sdk_dials_total counter
gantry_racer_sdk_dials_total 41
# HELP gantry_racer_sdk_connection_reuses_total SDK idle connection leases.
# TYPE gantry_racer_sdk_connection_reuses_total counter
gantry_racer_sdk_connection_reuses_total 43
# HELP gantry_racer_sdk_connection_rotations_total SDK reusable connections retired at their jittered maximum age across all three pools, excluding failures, aborts, and stale retries.
# TYPE gantry_racer_sdk_connection_rotations_total counter
gantry_racer_sdk_connection_rotations_total 19
# HELP gantry_racer_sdk_retries_total SDK stale pooled connection retries.
# TYPE gantry_racer_sdk_retries_total counter
gantry_racer_sdk_retries_total 2
# HELP gantry_racer_sdk_queue_wait_seconds_total Completed SDK admission wait time, including failures.
# TYPE gantry_racer_sdk_queue_wait_seconds_total counter
gantry_racer_sdk_queue_wait_seconds_total 1.5
# HELP gantry_racer_sdk_bytes_read_total SDK body bytes consumed, including partial transfers.
# TYPE gantry_racer_sdk_bytes_read_total counter
gantry_racer_sdk_bytes_read_total 17
`), "gantry_racer_sdk_queue_depth", "gantry_racer_sdk_bulk_queue_depth", "gantry_racer_sdk_metadata_queue_depth", "gantry_racer_sdk_small_object_queue_depth",
		"gantry_racer_sdk_active_bulk", "gantry_racer_sdk_active_metadata", "gantry_racer_sdk_active_small_objects",
		"gantry_racer_sdk_connections", "gantry_racer_sdk_idle_connections",
		"gantry_racer_sdk_dials_total", "gantry_racer_sdk_connection_reuses_total", "gantry_racer_sdk_connection_rotations_total", "gantry_racer_sdk_retries_total",
		"gantry_racer_sdk_queue_wait_seconds_total", "gantry_racer_sdk_bytes_read_total"); err != nil {
		t.Fatal(err)
	}
}

func TestRacerSDKRotationMetricsSnapshot(t *testing.T) {
	reg := metrics.New()
	snapshot := racersdk.Stats{}
	samples := 0

	reg.PrometheusRegistry().MustRegister(newRacerSDKCollector(func() racersdk.Stats {
		samples++
		return snapshot
	}))

	for i, test := range []struct {
		rotations uint64
		want      string
	}{
		{rotations: 0, want: "0"},
		{rotations: 7, want: "7"},
		{rotations: 7, want: "7"},
		{rotations: 9, want: "9"},
	} {
		snapshot.ConnectionRotations = test.rotations
		if err := testutil.GatherAndCompare(reg.PrometheusRegistry(), strings.NewReader(`
# HELP gantry_racer_sdk_connection_rotations_total SDK reusable connections retired at their jittered maximum age across all three pools, excluding failures, aborts, and stale retries.
# TYPE gantry_racer_sdk_connection_rotations_total counter
gantry_racer_sdk_connection_rotations_total `+test.want+"\n"), "gantry_racer_sdk_connection_rotations_total"); err != nil {
			t.Fatal(err)
		}

		if samples != i+1 {
			t.Fatalf("Stats samples=%d, want one per scrape (%d)", samples, i+1)
		}
	}
}

func TestRacerSDKConfigWiring(t *testing.T) {
	c := config.NewDefault()
	c.RacerMaxConnections = 7
	c.RacerMetadataConnections = 2
	c.RacerMetadataQueuedRequests = 5
	c.RacerSmallObjectConnections = 6
	c.RacerSmallObjectQueuedRequests = 17
	c.RacerMaxQueuedRequests = 11
	c.RacerQueueTimeout = time.Second
	c.RacerResponseHeaderTimeout = 2 * time.Second
	c.RacerOriginMaxConnections = 13
	c.RacerOriginConcurrentRequests = 3
	c.RacerOriginConcurrentHeadRequests = 8
	c.RacerOriginRequestTimeout = 4 * time.Second
	cache := racerTestCache(t)

	client := racerClientConfig(c, cache)
	if client.Cache != cache || client.MaxConnections != 7 || client.MetadataConnections != 2 || client.MaxQueuedRequests != 11 || client.QueueTimeout != time.Second || client.ResponseHeaderTimeout != 2*time.Second {
		t.Fatalf("client config=%+v", client)
	}

	if client.MetadataQueuedRequests != 5 || client.SmallObjectConnections != 6 || client.SmallObjectQueuedRequests != 17 {
		t.Fatalf("reserved client pools=%+v", client)
	}

	origin := racerOriginConfig(c, cache)
	if origin.Cache != cache || origin.MaxConnections != 13 || origin.MaxConcurrentRequests != 3 || origin.MaxConcurrentHeadRequests != 8 || origin.RequestTimeout != 4*time.Second {
		t.Fatalf("origin config=%+v", origin)
	}
}

func TestRacerSDKReservedPoolDefaults(t *testing.T) {
	for _, test := range []struct {
		name                                                    string
		config                                                  *config.Config
		metadataQueue, smallConnections, smallQueue, originHead int
	}{
		{name: "defaults", config: config.NewDefault(), metadataQueue: 16, smallConnections: 4, smallQueue: 128, originHead: 4},
		{name: "zero selects SDK defaults", config: &config.Config{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cache := racerTestCache(t)
			client := racerClientConfig(test.config, cache)

			origin := racerOriginConfig(test.config, cache)
			if client.MetadataQueuedRequests != test.metadataQueue || client.SmallObjectConnections != test.smallConnections || client.SmallObjectQueuedRequests != test.smallQueue || origin.MaxConcurrentHeadRequests != test.originHead {
				t.Fatalf("client=%+v origin=%+v", client, origin)
			}

			sdk, err := racersdk.NewClient(client)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := sdk.Close(); err != nil {
					t.Error(err)
				}
			})
		})
	}
}

func TestRacerSDKIdlePoolMetrics(t *testing.T) {
	reg := metrics.New()
	reg.PrometheusRegistry().MustRegister(newRacerSDKCollector(func() racersdk.Stats { return racersdk.Stats{} }))

	families, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	if len(families) != 18 {
		t.Fatalf("SDK metrics=%d, want all 18 even when idle", len(families))
	}

	for _, family := range families {
		if len(family.Metric) != 1 {
			t.Fatalf("%s: expected one fixed series", family.GetName())
		}

		metric := family.Metric[0]
		if len(metric.Label) != 0 || metric.GetGauge().GetValue() != 0 || metric.GetCounter().GetValue() != 0 {
			t.Fatalf("%s: idle metric must be zero without request labels: %v", family.GetName(), metric)
		}
	}
}
