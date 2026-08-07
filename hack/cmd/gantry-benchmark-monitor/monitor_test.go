// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func prometheusFixture(start time.Time) []byte {
	return []byte(fmt.Sprintf(`{
  "status":"success",
  "data":{"resultType":"matrix","result":[
    {"metric":{"outcome":"busy"},"values":[[%f,"100"],[%f,"110"],[%f,"130"],[%f,"135"]]},
    {"metric":{"outcome":"hit"},"values":[[%f,"20"],[%f,"22"],[%f,"25"]]},
    {"metric":{},"values":[[%f,"1000"],[%f,"60000001000"],[%f,"120000001000"],[%f,"150000001000"]]}
  ]}
}`,
		float64(start.Add(-10*time.Second).Unix()),
		float64(start.Add(10*time.Second).Unix()),
		float64(start.Add(70*time.Second).Unix()),
		float64(start.Add(130*time.Second).Unix()),
		float64(start.Add(-10*time.Second).Unix()),
		float64(start.Add(30*time.Second).Unix()),
		float64(start.Add(90*time.Second).Unix()),
		float64(start.Add(-10*time.Second).Unix()),
		float64(start.Add(59*time.Second).Unix()),
		float64(start.Add(119*time.Second).Unix()),
		float64(start.Add(130*time.Second).Unix()),
	))
}

func TestParseAndAggregateRange(t *testing.T) {
	start := time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC)

	response, err := parseRangeResponse(prometheusFixture(start))
	if err != nil {
		t.Fatalf("parseRangeResponse: %v", err)
	}

	snapshot := aggregateRange(response, start, start.Add(150*time.Second), 200e9, 1000)
	if len(snapshot.Bins) != 3 {
		t.Fatalf("bins = %d, want 3", len(snapshot.Bins))
	}

	checks := []struct {
		minute int
		busy   float64
		hit    float64
		bytes  float64
	}{
		{minute: 0, busy: 10, hit: 2, bytes: 60e9},
		{minute: 1, busy: 20, hit: 3, bytes: 60e9},
		{minute: 2, busy: 5, hit: 0, bytes: 30e9},
	}
	for _, check := range checks {
		bin := snapshot.Bins[check.minute]
		if bin.PeerOutcomes["busy"] != check.busy || bin.PeerOutcomes["hit"] != check.hit || bin.Bytes != check.bytes {
			t.Errorf("minute %d = busy %.0f hit %.0f bytes %.0f; want %.0f %.0f %.0f",
				check.minute, bin.PeerOutcomes["busy"], bin.PeerOutcomes["hit"], bin.Bytes, check.busy, check.hit, check.bytes)
		}
	}

	if snapshot.PeerTotals["busy"] != 35 || snapshot.PeerTotals["hit"] != 5 {
		t.Fatalf("peer totals = %#v", snapshot.PeerTotals)
	}

	if snapshot.TotalBytes != 150e9 {
		t.Fatalf("total bytes = %.0f, want %.0f", snapshot.TotalBytes, 150e9)
	}
}

func TestRenderSnapshotIncludesBothLiveTables(t *testing.T) {
	start := time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC)

	response, err := parseRangeResponse(prometheusFixture(start))
	if err != nil {
		t.Fatal(err)
	}

	snapshot := aggregateRange(response, start, start.Add(150*time.Second), 200e9, 1000)
	snapshot.RunID = "run-1"
	snapshot.JobName = "gantry-benchmark-gantry-cold-run-1"
	snapshot.PhaseStart = start
	snapshot.Now = start.Add(150 * time.Second)
	snapshot.RefreshInterval = time.Second
	snapshot.Job = jobStatus{Succeeded: 12, Active: 988}

	output := renderSnapshot(snapshot)
	for _, want := range []string{
		"=== Peer fetch outcomes by phase minute ===",
		"busy",
		"notfound",
		"TOTAL",
		"busy in first 6 min: 35 of 35 = 100.0%",
		"=== Layer bytes delivered by phase minute ===",
		"MB/s per node",
		"2*",
		"total 0.150 TB of 0.200 TB (75.0%)",
		"pods: 12/1000 succeeded, 988 active, 0 failed",
		"Prometheus scrape cadence: 10s",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("output is missing %q:\n%s", want, output)
		}
	}
}

func TestPrometheusExpressionScopesCurrentRevision(t *testing.T) {
	expression := prometheusExpression(monitorSession{revision: "gantry-abc123"}, "gantry-system")
	for _, want := range []string{
		`p2p_peer_fetch_total`,
		`gantry_mirror_bytes_served_total`,
		`namespace="gantry-system"`,
		`controller_revision_hash="gantry-abc123"`,
		`kind="layer"`,
		` or `,
	} {
		if !strings.Contains(expression, want) {
			t.Errorf("expression %q is missing %q", expression, want)
		}
	}
}

func TestCommaInteger(t *testing.T) {
	for value, want := range map[float64]string{
		0:        "0",
		999:      "999",
		1000:     "1,000",
		1162435:  "1,162,435",
		-1234567: "-1,234,567",
	} {
		if got := commaInteger(value); got != want {
			t.Errorf("commaInteger(%.0f) = %q, want %q", value, got, want)
		}
	}
}

func TestParseRangeResponseRejectsFailure(t *testing.T) {
	if _, err := parseRangeResponse([]byte(`{"status":"error"}`)); err == nil {
		t.Fatal("parseRangeResponse succeeded for an error response")
	}
}
