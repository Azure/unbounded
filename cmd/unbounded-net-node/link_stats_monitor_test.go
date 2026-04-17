// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"
	"time"
)

// testMonitor creates a linkStatsMonitor with a controllable clock and
// pre-populated previous state so tests can call collectDirect without
// shelling out to ip(8).
func testMonitor(now time.Time) *linkStatsMonitor {
	m := newLinkStatsMonitor(30 * time.Second)
	m.nowFunc = func() time.Time { return now }

	return m
}

// collectDirect injects counters directly and runs the warning-expiry logic
// that normally lives in collect(), avoiding the need for real network
// namespaces.
func collectDirect(m *linkStatsMonitor, current map[string]linkCounters) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.nowFunc()

	for key, cur := range current {
		prev, ok := m.previous[key]
		if !ok {
			continue
		}

		if deltas := detectIncrements(prev, cur, m.rateThreshold); len(deltas) > 0 {
			m.lastWarning[key] = warningEntry{
				message:  "interface " + key + ": incremented",
				lastSeen: now,
			}
		}
	}

	for key := range m.lastWarning {
		if _, ok := current[key]; !ok {
			delete(m.lastWarning, key)
		}
	}

	var warnings []string

	for key, entry := range m.lastWarning {
		if now.Sub(entry.lastSeen) >= m.warningTimeout {
			delete(m.lastWarning, key)
			continue
		}

		warnings = append(warnings, entry.message)
	}

	m.previous = current
	m.warnings = warnings
}

func TestWarningAppearsOnIncrement(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(now)

	// Baseline: no previous, so no warnings.
	baseline := map[string]linkCounters{
		"/wg51820": {TxErrors: 100},
	}
	collectDirect(m, baseline)

	if len(m.GetWarnings()) != 0 {
		t.Fatal("expected no warnings on first collection")
	}

	// Increment tx_errors.
	incremented := map[string]linkCounters{
		"/wg51820": {TxErrors: 200},
	}
	collectDirect(m, incremented)

	if len(m.GetWarnings()) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(m.GetWarnings()))
	}
}

func TestWarningPersistsWhenCounterStopsIncrementing(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(now)

	baseline := map[string]linkCounters{"/wg51820": {TxErrors: 100}}
	collectDirect(m, baseline)

	// Increment once.
	now = now.Add(30 * time.Second)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	if len(m.GetWarnings()) != 1 {
		t.Fatal("expected warning after increment")
	}

	// Counters stay flat for 2 minutes -- warning should persist.
	now = now.Add(2 * time.Minute)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	if len(m.GetWarnings()) != 1 {
		t.Fatal("expected warning to persist within timeout")
	}
}

func TestWarningClearsAfterTimeout(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(now)

	baseline := map[string]linkCounters{"/wg51820": {TxErrors: 100}}
	collectDirect(m, baseline)

	// Increment once.
	now = now.Add(30 * time.Second)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	if len(m.GetWarnings()) != 1 {
		t.Fatal("expected warning after increment")
	}

	// Advance past the 5-minute timeout with no new increments.
	now = now.Add(5 * time.Minute)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	if len(m.GetWarnings()) != 0 {
		t.Fatal("expected warning to be cleared after 5-minute timeout")
	}
}

func TestWarningRefreshedByNewIncrement(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(now)

	baseline := map[string]linkCounters{"/wg51820": {TxErrors: 100}}
	collectDirect(m, baseline)

	// First increment.
	now = now.Add(30 * time.Second)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	// 4 minutes later, another increment refreshes the timer.
	now = now.Add(4 * time.Minute)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 300}})

	// 3 more minutes (7.5 min since first increment, 3 min since last).
	now = now.Add(3 * time.Minute)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 300}})

	if len(m.GetWarnings()) != 1 {
		t.Fatal("expected warning to still be active -- timer was refreshed")
	}

	// 5 more minutes since last increment -- should clear.
	now = now.Add(2*time.Minute + 1*time.Second)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 300}})

	if len(m.GetWarnings()) != 0 {
		t.Fatal("expected warning to clear after timeout from last increment")
	}
}

func TestWarningRemovedWhenInterfaceDisappears(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := testMonitor(now)

	baseline := map[string]linkCounters{"/wg51820": {TxErrors: 100}}
	collectDirect(m, baseline)

	now = now.Add(30 * time.Second)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{"/wg51820": {TxErrors: 200}})

	if len(m.GetWarnings()) != 1 {
		t.Fatal("expected warning")
	}

	// Interface disappears.
	now = now.Add(30 * time.Second)
	m.nowFunc = func() time.Time { return now }
	collectDirect(m, map[string]linkCounters{})

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
