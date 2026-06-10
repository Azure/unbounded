// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRegistry_OwnershipMap(t *testing.T) {
	r := New()
	r.NewCounter("cache", prometheus.CounterOpts{Name: "p2p_cache_hit_total", Help: "h"})
	r.NewCounter("origin", prometheus.CounterOpts{Name: "p2p_origin_pull_total", Help: "h"})

	owners := r.Owners()
	if len(owners) != 2 {
		t.Fatalf("len(owners) = %d, want 2", len(owners))
	}
	// Owners returns sorted by name.
	if owners[0].Name != "p2p_cache_hit_total" || owners[0].Subsystem != "cache" {
		t.Errorf("owners[0] = %+v", owners[0])
	}

	if owners[1].Name != "p2p_origin_pull_total" || owners[1].Subsystem != "origin" {
		t.Errorf("owners[1] = %+v", owners[1])
	}
}

func TestRegistry_Handler(t *testing.T) {
	r := New()
	c := r.NewCounter("cache", prometheus.CounterOpts{Name: "p2p_cache_hit_total", Help: "h"})
	c.Inc()

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "p2p_cache_hit_total 1") {
		t.Errorf("metrics output missing counter:\n%s", body)
	}
}

func TestRegistry_DuplicateOwnerPanics(t *testing.T) {
	r := New()
	r.NewCounter("cache", prometheus.CounterOpts{Name: "p2p_thing_total", Help: "h"})

	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate-owner registration")
		}
	}()

	r.NewCounter("origin", prometheus.CounterOpts{Name: "p2p_thing_total", Help: "h"})
}
