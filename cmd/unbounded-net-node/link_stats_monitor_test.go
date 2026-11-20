// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"
)

func TestWarningAppearsOnIncrement(t *testing.T) {
	m := newLinkStatsMonitor(30 * time.Second)

	// Baseline: no previous, so no warnings.
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 100}})

	if len(m.GetWarnings()) != 0 {
		t.Fatal("expected no warnings on first collection")
	}

	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	got := m.GetWarnings()
	if len(got) != 1 || got[0] != "interface /wg51820: tx_errors +100" {
		t.Fatalf("expected one tx_errors warning, got %v", got)
	}
}

// TestWarningClearsWhenCounterGoesFlat reproduces the incident where
// "interface /wg51821: tx_errors +12" kept being reported every interval
// after the tunnel recovered although the raw counter was flat.
func TestWarningClearsWhenCounterGoesFlat(t *testing.T) {
	m := newLinkStatsMonitor(30 * time.Second)

	m.update(map[string]linkCounters{"/wg51821": {TxErrors: 100, TxDropped: 7}})

	// Counter increases once.
	m.update(map[string]linkCounters{"/wg51821": {TxErrors: 112, TxDropped: 7}})

	if got := m.GetWarnings(); len(got) != 1 || got[0] != "interface /wg51821: tx_errors +12" {
		t.Fatalf("expected one tx_errors +12 warning, got %v", got)
	}

	// Counters stay flat for several intervals: nothing is reported.
	for i := range 5 {
		m.update(map[string]linkCounters{"/wg51821": {TxErrors: 112, TxDropped: 7}})

		if got := m.GetWarnings(); len(got) != 0 {
			t.Fatalf("interval %d: expected no warnings for flat counters, got %v", i, got)
		}
	}
}

func TestWarningReportsEachIntervalDelta(t *testing.T) {
	m := newLinkStatsMonitor(30 * time.Second)

	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 100}})
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 200}})
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 230}})

	if got := m.GetWarnings(); len(got) != 1 || got[0] != "interface /wg51820: tx_errors +30" {
		t.Fatalf("expected the delta of the latest interval only, got %v", got)
	}
}

func TestWarningIgnoresIncrementBelowThreshold(t *testing.T) {
	m := newLinkStatsMonitor(30 * time.Second)

	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 100}})
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 100 + defaultRateThreshold - 1}})

	if got := m.GetWarnings(); len(got) != 0 {
		t.Fatalf("expected no warnings below threshold, got %v", got)
	}
}

func TestWarningNotReportedOnCounterReset(t *testing.T) {
	m := newLinkStatsMonitor(30 * time.Second)

	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 1000}})

	// Interface recreated: counters restart from zero.
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 3}})

	if got := m.GetWarnings(); len(got) != 0 {
		t.Fatalf("expected counter reset to be ignored, got %v", got)
	}

	// The reset value becomes the new baseline.
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 20}})

	if got := m.GetWarnings(); len(got) != 1 || got[0] != "interface /wg51820: tx_errors +17" {
		t.Fatalf("expected delta from reset baseline, got %v", got)
	}
}

func TestWarningRemovedWhenInterfaceDisappears(t *testing.T) {
	m := newLinkStatsMonitor(30 * time.Second)

	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 100}})
	m.update(map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	if len(m.GetWarnings()) != 1 {
		t.Fatal("expected warning")
	}

	// Interface disappears.
	m.update(map[string]linkCounters{})

	if len(m.GetWarnings()) != 0 {
		t.Fatal("expected warning to be removed when interface disappears")
	}
}

func TestDetectIncrements(t *testing.T) {
	prev := linkCounters{TxErrors: 10, RxErrors: 5}
	cur := linkCounters{TxErrors: 20, RxErrors: 5}

	deltas := detectIncrements(prev, cur, 1)
	if len(deltas) != 1 {
		t.Fatalf("expected 1 delta, got %d: %v", len(deltas), deltas)
	}

	if deltas[0] != "tx_errors +10" {
		t.Fatalf("unexpected delta: %s", deltas[0])
	}
}

func TestDetectIncrementsThreshold(t *testing.T) {
	prev := linkCounters{TxErrors: 10, RxErrors: 5, TxDropped: 100}
	cur := linkCounters{TxErrors: 14, RxErrors: 5, TxDropped: 110}
	// With threshold=5, TxErrors +4 is below threshold, TxDropped +10 is at threshold.
	deltas := detectIncrements(prev, cur, 5)
	if len(deltas) != 1 {
		t.Fatalf("expected 1 delta (tx_dropped only), got %d: %v", len(deltas), deltas)
	}

	if deltas[0] != "tx_dropped +10" {
		t.Fatalf("unexpected delta: %s", deltas[0])
	}
}

func TestDetectIncrementsFlatAndReset(t *testing.T) {
	prev := linkCounters{TxErrors: 10, RxDropped: 50}
	cur := linkCounters{TxErrors: 10, RxDropped: 2}

	if deltas := detectIncrements(prev, cur, 0); len(deltas) != 0 {
		t.Fatalf("expected no deltas for flat or reset counters, got %v", deltas)
	}
}
