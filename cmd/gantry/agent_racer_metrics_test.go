// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/metrics"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

func racerMetricCounters(t *testing.T, reg *metrics.Registry) map[string]float64 {
	t.Helper()

	families, err := reg.PrometheusRegistry().Gather()
	if err != nil {
		t.Fatal(err)
	}

	counters := make(map[string]float64)

	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Counter == nil {
				continue
			}

			key := family.GetName()
			for _, label := range metric.Label {
				key += "/" + label.GetName() + "=" + label.GetValue()
			}

			counters[key] = metric.GetCounter().GetValue()
		}
	}

	return counters
}

func TestRacerStreamMetricsForwardingOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
		err     error
		outcome string
	}{
		{name: "full response", outcome: "completed"},
		{name: "range response", partial: true, outcome: "partial"},
		{name: "truncated full response", err: io.ErrUnexpectedEOF, outcome: "aborted"},
		{name: "truncated range response", partial: true, err: io.ErrUnexpectedEOF, outcome: "aborted"},
		{name: "canceled response", err: context.Canceled, outcome: "aborted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := metrics.New()
			record := newRacerStreamMetrics(reg)

			// Compatibility counters must be exported at zero before traffic.
			for _, name := range []string{"gantry_racer_tee_calls_total", "gantry_racer_tee_bytes_total"} {
				if value, ok := racerMetricCounters(t, reg)[name]; !ok || value != 0 {
					t.Fatalf("%s = %v, present=%v; want exported zero", name, value, ok)
				}
			}

			record(sdk.TransferStats{SpliceCalls: 2, SpliceBytes: 4096, BufferedBytes: 128}, tc.partial, tc.err)
			record(sdk.TransferStats{SpliceCalls: 1, SpliceBytes: 2048, BufferedBytes: 64}, tc.partial, tc.err)

			want := map[string]float64{
				"gantry_racer_splice_calls_total":                 3,
				"gantry_racer_splice_bytes_total":                 6144,
				"gantry_racer_buffered_bytes_total":               192,
				"gantry_racer_tee_calls_total":                    0,
				"gantry_racer_tee_bytes_total":                    0,
				"gantry_racer_stream_total/outcome=" + tc.outcome: 2,
			}

			got := racerMetricCounters(t, reg)
			if len(got) != len(want) {
				t.Fatalf("unexpected metric series: %v, want %v", got, want)
			}

			for name, value := range want {
				if actual, ok := got[name]; !ok || actual != value {
					t.Errorf("%s = %v, present=%v; want %v", name, actual, ok, value)
				}
			}
		})
	}
}

func TestRacerStreamMetricsCommitObservationIsSeparate(t *testing.T) {
	reg := metrics.New()
	record := newRacerStreamMetrics(reg)
	p9 := newPhase9Metrics(reg)
	store := &fakeInventorySource{}
	tracker := newStreamCommitTracker(store, nil,
		func(n int) { p9.containerdCommitObserved.Add(float64(n)) },
		nil,
		func(n int) { p9.commitMissingAfterStream.Add(float64(n)) })

	d := trackerDigestOf([]byte("forwarded response"))

	record(sdk.TransferStats{}, false, nil)
	tracker.RecordCompleted(d)

	assertCounts := func(completed, observed, missing float64) {
		t.Helper()

		got := racerMetricCounters(t, reg)
		for name, want := range map[string]float64{
			"gantry_racer_stream_total/outcome=completed":         completed,
			"gantry_containerd_commit_observed_total":             observed,
			"gantry_containerd_commit_missing_after_stream_total": missing,
		} {
			if actual, ok := got[name]; !ok || actual != want {
				t.Errorf("%s = %v, present=%v; want %v", name, actual, ok, want)
			}
		}

		for _, outcome := range []string{"verified", "digest_mismatch", "aborted"} {
			if _, ok := got["gantry_racer_stream_total/outcome="+outcome]; ok {
				t.Errorf("commit observation created stream outcome %q", outcome)
			}
		}
	}
	assertCounts(1, 0, 0)
	store.SetCurrent(d)
	tracker.probe(t.Context())
	assertCounts(1, 1, 0)

	store.SetCurrent()

	tracker.verifyWindow = -time.Second

	record(sdk.TransferStats{}, false, nil)
	tracker.RecordCompleted(d)
	tracker.probe(t.Context())
	assertCounts(2, 1, 1)
}
