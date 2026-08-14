// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/snapshotter"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
)

// scrape renders the registry the way a Prometheus scrape would.
func scrape(t *testing.T, mux *http.ServeMux) string {
	t.Helper()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}

	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return string(body)
}

// testMux builds the observability routes over a fresh registry and a detached
// catalog, which is the state a node is in before racer-ctrl publishes a
// device.
func testMux(t *testing.T, cfg *Config) (*http.ServeMux, *daemonMetrics) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := metrics.New()
	cat := &holder{log: log}

	daemon := newDaemonMetrics(reg, cat)

	queue, err := ingest.NewQueue(ingest.QueueOptions{Ingester: &ingest.Ingester{}})
	if err != nil {
		t.Fatalf("queue: %v", err)
	}

	daemon.trackQueue(reg, queue)

	health := func(context.Context) error { return nil }

	return metricsMux(cfg, reg, health, log), daemon
}

// The daemon's own metrics have to be on the endpoint the DaemonSet exposes.
// Before this the handler served the default registry, so a scrape returned
// Go runtime counters and nothing about snapshots.
func TestMetricsExposesTheDaemonsMetrics(t *testing.T) {
	mux, _ := testMux(t, &Config{})
	body := scrape(t, mux)

	want := []string{
		"gantry_snapshotter_adopt_total",
		"gantry_snapshotter_ingest_total",
		"gantry_snapshotter_ingest_bytes_total",
		"gantry_snapshotter_segment_rolls_total",
		"gantry_snapshotter_catalog_records",
		"gantry_snapshotter_catalog_hole_seconds",
		"gantry_snapshotter_ingest_pending",
	}

	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("scrape is missing %s", name)
		}
	}
}

// A rate over a series that does not exist yet is not a number an alert can
// use, so every label value has to be present on a node that has done nothing.
func TestMetricsPreCreatesEveryOutcomeLabel(t *testing.T) {
	mux, _ := testMux(t, &Config{})
	body := scrape(t, mux)

	want := []string{
		`gantry_snapshotter_adopt_total{outcome="hit"} 0`,
		`gantry_snapshotter_adopt_total{outcome="miss"} 0`,
		`gantry_snapshotter_adopt_total{outcome="exists"} 0`,
		`gantry_snapshotter_adopt_total{outcome="failed"} 0`,
		`gantry_snapshotter_ingest_total{outcome="ingested"} 0`,
		`gantry_snapshotter_ingest_total{outcome="present"} 0`,
		`gantry_snapshotter_ingest_total{outcome="linked"} 0`,
		`gantry_snapshotter_ingest_total{outcome="failed"} 0`,
	}

	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("scrape is missing %q", line)
		}
	}
}

func TestMetricsCountsAdoptOutcomes(t *testing.T) {
	mux, daemon := testMux(t, &Config{})

	daemon.observeAdopt(snapshotter.AdoptHit)
	daemon.observeAdopt(snapshotter.AdoptHit)
	daemon.observeAdopt(snapshotter.AdoptMiss)
	daemon.observeAdopt(snapshotter.AdoptFailed)

	body := scrape(t, mux)

	want := []string{
		`gantry_snapshotter_adopt_total{outcome="hit"} 2`,
		`gantry_snapshotter_adopt_total{outcome="miss"} 1`,
		`gantry_snapshotter_adopt_total{outcome="failed"} 1`,
		`gantry_snapshotter_adopt_total{outcome="exists"} 0`,
	}

	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("scrape is missing %q", line)
		}
	}
}

// Only an ingest that actually wrote moved bytes into a segment. Counting the
// other outcomes would make every node look like it filled the cluster.
func TestMetricsCountsIngestBytesOnlyWhenBytesWereWritten(t *testing.T) {
	mux, daemon := testMux(t, &Config{})

	ingested := ingest.Result{Outcome: ingest.OutcomeIngested}
	ingested.Blob.Address.ByteLength = 4096

	linked := ingest.Result{Outcome: ingest.OutcomeLinked}
	linked.Blob.Address.ByteLength = 1 << 20

	daemon.observeIngest(ingest.Request{}, ingested, nil)
	daemon.observeIngest(ingest.Request{}, linked, nil)

	body := scrape(t, mux)

	if !strings.Contains(body, "gantry_snapshotter_ingest_bytes_total 4096") {
		t.Errorf("bytes counter did not record only the ingested blob:\n%s", body)
	}

	if !strings.Contains(body, `gantry_snapshotter_ingest_total{outcome="linked"} 1`) {
		t.Errorf("linked outcome was not counted:\n%s", body)
	}
}

// A failed ingest has no ingest.Outcome, because it did nothing. It still has
// to be counted: a node whose ingests all fail publishes nothing, and no other
// metric here would show that.
func TestMetricsCountsFailedIngest(t *testing.T) {
	mux, daemon := testMux(t, &Config{})

	daemon.observeIngest(ingest.Request{}, ingest.Result{}, io.ErrUnexpectedEOF)

	body := scrape(t, mux)

	if !strings.Contains(body, `gantry_snapshotter_ingest_total{outcome="failed"} 1`) {
		t.Errorf("failed ingest was not counted:\n%s", body)
	}

	if !strings.Contains(body, "gantry_snapshotter_ingest_bytes_total 0") {
		t.Errorf("failed ingest recorded bytes:\n%s", body)
	}
}

func TestMetricsCountsSegmentRolls(t *testing.T) {
	mux, daemon := testMux(t, &Config{})

	daemon.observeRoll(catalog.Roll{Sealed: 1, Opened: 2})

	if body := scrape(t, mux); !strings.Contains(body, "gantry_snapshotter_segment_rolls_total 1") {
		t.Errorf("roll was not counted:\n%s", body)
	}
}

// The holder routes rollovers to whatever observer is attached, on top of the
// log line it already emits. Without this the counter above never moves in a
// running daemon.
func TestHolderReportsRollsToTheObserver(t *testing.T) {
	var got []catalog.Roll

	h := &holder{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		roll: func(r catalog.Roll) { got = append(got, r) },
	}

	h.logRoll(catalog.Roll{Sealed: 3, Opened: 4})

	if len(got) != 1 || got[0].Sealed != 3 || got[0].Opened != 4 {
		t.Fatalf("observer saw %v, want one roll 3 -> 4", got)
	}
}

// A holder with no catalog attached is the state every node boots in. Reading
// metrics off it must not panic.
func TestHolderHoleWithoutACatalog(t *testing.T) {
	h := &holder{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	slot, age := h.Hole()
	if slot != 0 || age != 0 {
		t.Fatalf("Hole() = %d, %s, want 0, 0", slot, age)
	}
}

// pprof hands out heap contents and execution traces, and this listener is on
// the pod network because the kubelet probe has to reach it. Off unless asked
// for.
func TestPprofIsOffByDefault(t *testing.T) {
	mux, _ := testMux(t, &Config{})

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol", "/debug/pprof/trace"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

func TestPprofIsServedWhenEnabled(t *testing.T) {
	mux, _ := testMux(t, &Config{EnablePprof: true})

	// Index and symbol only; profile and trace both sleep for their
	// sampling window.
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/symbol"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
}

// The liveness probe shares the listener with pprof, so gating pprof must not
// have moved it.
func TestHealthzIsServedInBothModes(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		mux, _ := testMux(t, &Config{EnablePprof: enabled})

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

		if rec.Code != http.StatusOK {
			t.Errorf("pprof=%v: /healthz status = %d, want 200", enabled, rec.Code)
		}
	}
}

func TestChainObserverRunsEveryObserver(t *testing.T) {
	var calls []string

	fn := chainObserver(
		func(ingest.Request, ingest.Result, error) { calls = append(calls, "first") },
		func(ingest.Request, ingest.Result, error) { calls = append(calls, "second") },
	)

	fn(ingest.Request{}, ingest.Result{}, nil)

	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("calls = %v, want first then second", calls)
	}
}
