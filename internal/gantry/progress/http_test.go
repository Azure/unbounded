// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	cerrdefs "github.com/containerd/errdefs"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Azure/unbounded/internal/gantry/containerdstore"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/rangecache"
)

type partialStore struct {
	content.Store
	statuses []content.Status
	err      error
}

func (s partialStore) ListStatuses(context.Context, ...string) ([]content.Status, error) {
	return s.statuses, s.err
}

func (partialStore) ReaderAt(context.Context, ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, cerrdefs.ErrNotFound
}

func TestEndpointCombinesIngestAndUniqueStreamingRanges(t *testing.T) {
	d := digest.MustParse(godigest.FromString("layer").String())

	cache, err := rangecache.New(t.TempDir()+"/ranges", 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cache.Close() })

	for range 2 {
		if _, err := cache.Put(d, 100, 5, bytes.NewReader(make([]byte, 15))); err != nil {
			t.Fatal(err)
		}
	}

	store := containerdstore.New(partialStore{statuses: []content.Status{{Expected: godigest.Digest(d.String()), Offset: 10, Total: 100}}})
	handler := NewHandler(store, cache, metrics.New().PrometheusRegistry(), "")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/progress?digest="+d.String()+","+d.String(), nil))

	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || len(result.Blobs) != 1 || result.Blobs[0].Available != 20 || result.Blobs[0].Complete {
		t.Fatalf("range/ingest overlap or repeated requests double counted: %d %s", response.Code, response.Body)
	}
}

func TestEndpointRejectsUnboundedQueriesAndSurfacesStoreFailures(t *testing.T) {
	d := godigest.FromString("layer").String()
	handler := NewHandler(containerdstore.New(partialStore{err: errors.New("unavailable")}), nil, metrics.New().PrometheusRegistry(), "")

	for _, tc := range []struct {
		method, query string
		status        int
	}{
		{http.MethodGet, "invalid", http.StatusBadRequest},
		{http.MethodGet, strings.Repeat(d+",", MaxLayers), http.StatusBadRequest},
		{http.MethodPost, d, http.StatusMethodNotAllowed},
		{http.MethodGet, d, http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(tc.method, "/v1/progress?digest="+tc.query, nil))

		if response.Code != tc.status {
			t.Errorf("%s query returned %d, want %d", tc.method, response.Code, tc.status)
		}
	}
}

func TestActivityOnlyReportsLiveSourceMetricsWithoutStoreOrDoubleCounting(t *testing.T) {
	reg := prometheus.NewRegistry()
	live := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gantry_transfer_bytes_total", Help: "test"}, []string{"source", "kind"})
	active := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gantry_active_transfers", Help: "test"}, []string{"source", "kind"})
	old := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gantry_origin_bytes_total", Help: "test"}, []string{"kind"})
	served := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gantry_peer_serve_bytes_total", Help: "test"}, []string{"kind"})
	reg.MustRegister(live, active, old, served)
	live.WithLabelValues("origin", "layer").Add(123)
	live.WithLabelValues("peer", "layer").Add(456)
	live.WithLabelValues("origin", "manifest").Add(9999)
	active.WithLabelValues("origin", "layer").Set(2)
	active.WithLabelValues("peer", "layer").Set(3)
	old.WithLabelValues("layer").Add(9999)
	served.WithLabelValues("layer").Add(456)

	response := httptest.NewRecorder()
	NewHandler(nil, nil, reg, "").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/progress", nil))

	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || len(result.Blobs) != 0 || result.Traffic.OriginBytes != 123 || result.Traffic.PeerBytes != 456 ||
		result.Activity == nil || result.Activity.OriginActive != 2 || result.Activity.PeerActive != 3 {
		t.Fatalf("activity-only response duplicated legacy/peer-send counters or accessed containerd: %d %s", response.Code, response.Body)
	}
}

func TestAbsentActivityGaugesAreUnavailableNotZero(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gantry_origin_bytes_total", Help: "test"}, []string{"kind"})
	reg.MustRegister(counter)
	counter.WithLabelValues("layer").Add(42)

	response := httptest.NewRecorder()
	NewHandler(nil, nil, reg, "").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/progress", nil))

	var result Response
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusOK || result.Activity != nil || result.Traffic.OriginBytes != 42 {
		t.Fatalf("legacy activity was fabricated: %s", response.Body)
	}
}

func TestFilesystemUsageDoesNotRequireContentAndReportsReadFailures(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{root, root + "/missing"} {
		response := httptest.NewRecorder()
		NewHandler(nil, nil, prometheus.NewRegistry(), path).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/progress", nil))

		var result Response
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}

		if response.Code != http.StatusOK {
			t.Fatalf("filesystem observation blocked other telemetry: %d %s", response.Code, response.Body)
		}

		if path == root {
			if result.Storage == nil || result.Storage.Path != root || result.Storage.Total == 0 ||
				result.Storage.Used > result.Storage.Total || result.Storage.Available > result.Storage.Total-result.Storage.Used ||
				result.StorageError != "" {
				t.Fatalf("invalid filesystem observation: %s", response.Body)
			}
		} else if result.Storage != nil || !strings.Contains(result.StorageError, "filesystem usage unavailable") {
			t.Fatalf("filesystem failure became a fabricated zero: %s", response.Body)
		}
	}
}
