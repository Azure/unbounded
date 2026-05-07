// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// defaultWarningTimeout is how long a warning persists after the last
// significant counter increment before it is cleared automatically.
const defaultWarningTimeout = 5 * time.Minute

// defaultRateThreshold is the minimum per-interval counter increment
// that is considered significant. Increments below this threshold are
// treated as background noise and will not refresh the warning timer.
const defaultRateThreshold uint64 = 5

// linkStatsMonitor periodically collects network interface statistics across
// all network namespaces and detects incrementing error/drop counters.
type linkStatsMonitor struct {
	interval       time.Duration
	warningTimeout time.Duration
	rateThreshold  uint64
	// nowFunc is used to obtain the current time; overridable in tests.
	nowFunc func() time.Time

	mu          sync.Mutex
	previous    map[string]linkCounters // key: "netns/ifname"
	lastWarning map[string]warningEntry // key: "netns/ifname"
	warnings    []string                // current active warnings
}

// warningEntry tracks the most recent warning text and the time it was
// last observed for a given interface.
type warningEntry struct {
	message  string
	lastSeen time.Time
}

// linkCounters tracks the error-related counters from stats64.
type linkCounters struct {
	RxErrors      uint64
	RxDropped     uint64
	RxOverErrors  uint64
	TxErrors      uint64
	TxDropped     uint64
	TxCarrierErrs uint64
	TxCollisions  uint64
}

// ipLinkEntry represents a single interface from `ip -j -s -d link show`.
type ipLinkEntry struct {
	IfName  string `json:"ifname"`
	Stats64 struct {
		Rx struct {
			Errors     uint64 `json:"errors"`
			Dropped    uint64 `json:"dropped"`
			OverErrors uint64 `json:"over_errors"`
		} `json:"rx"`
		Tx struct {
			Errors        uint64 `json:"errors"`
			Dropped       uint64 `json:"dropped"`
			CarrierErrors uint64 `json:"carrier_errors"`
			Collisions    uint64 `json:"collisions"`
		} `json:"tx"`
	} `json:"stats64"`
}

// ipNetnsEntry represents a network namespace from `ip -j netns list`.
type ipNetnsEntry struct {
	Name string `json:"name"`
}

// newLinkStatsMonitor creates a monitor that checks link stats at the given interval.
func newLinkStatsMonitor(interval time.Duration) *linkStatsMonitor {
	return &linkStatsMonitor{
		interval:       interval,
		warningTimeout: defaultWarningTimeout,
		rateThreshold:  defaultRateThreshold,
		nowFunc:        time.Now,
		previous:       make(map[string]linkCounters),
		lastWarning:    make(map[string]warningEntry),
	}
}

// Start runs the monitor loop until the context is cancelled.
func (m *linkStatsMonitor) Start(ctx context.Context) {
	klog.V(2).Infof("Link stats monitor started (interval %s)", m.interval)
	// Collect baseline immediately so the first check has a reference.
	m.collect()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(2).Info("Link stats monitor stopped")
			return
		case <-ticker.C:
			m.collect()
		}
	}
}

// GetWarnings returns the current set of active warnings.
func (m *linkStatsMonitor) GetWarnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, len(m.warnings))
	copy(out, m.warnings)

	return out
}

// collect reads link stats from all namespaces and detects increments.
func (m *linkStatsMonitor) collect() {
	current := make(map[string]linkCounters)

	// Root network namespace
	entries := runIPLinkStats("")
	for _, e := range entries {
		key := "/" + e.IfName
		current[key] = countersFromEntry(e)
	}

	// Named network namespaces
	namespaces := listNetworkNamespaces()
	for _, ns := range namespaces {
		entries = runIPLinkStats(ns)
		for _, e := range entries {
			key := ns + "/" + e.IfName
			current[key] = countersFromEntry(e)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.nowFunc()

	// Update warning entries for interfaces with significant increments.
	for key, cur := range current {
		prev, ok := m.previous[key]
		if !ok {
			continue
		}

		if deltas := detectIncrements(prev, cur, m.rateThreshold); len(deltas) > 0 {
			m.lastWarning[key] = warningEntry{
				message:  fmt.Sprintf("interface %s: %s", key, strings.Join(deltas, ", ")),
				lastSeen: now,
			}
		}
	}

	// Remove entries for interfaces that no longer exist.
	for key := range m.lastWarning {
		if _, ok := current[key]; !ok {
			delete(m.lastWarning, key)
		}
	}

	// Build warnings from entries that have not expired.
	var warnings []string

	for key, entry := range m.lastWarning {
		if now.Sub(entry.lastSeen) >= m.warningTimeout {
			delete(m.lastWarning, key)
			continue
		}

		warnings = append(warnings, entry.message)
	}

	sort.Strings(warnings)

	if len(warnings) > 0 {
		klog.V(2).Infof("Link stats: detected %d interface(s) with incrementing error counters", len(warnings))

		for _, w := range warnings {
			klog.V(2).Infof("  %s", w)
		}
	}

	m.previous = current
	m.warnings = warnings
}

// countersFromEntry extracts the tracked counters from an ipLinkEntry.
func countersFromEntry(e ipLinkEntry) linkCounters {
	return linkCounters{
		RxErrors:      e.Stats64.Rx.Errors,
		RxDropped:     e.Stats64.Rx.Dropped,
		RxOverErrors:  e.Stats64.Rx.OverErrors,
		TxErrors:      e.Stats64.Tx.Errors,
		TxDropped:     e.Stats64.Tx.Dropped,
		TxCarrierErrs: e.Stats64.Tx.CarrierErrors,
		TxCollisions:  e.Stats64.Tx.Collisions,
	}
}

// detectIncrements compares previous and current counters and returns
// descriptions of any that increased by at least minDelta.
func detectIncrements(prev, cur linkCounters, minDelta uint64) []string {
	var deltas []string
	if d := cur.RxErrors - prev.RxErrors; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("rx_errors +%d", d))
	}

	if d := cur.RxDropped - prev.RxDropped; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("rx_dropped +%d", d))
	}

	if d := cur.RxOverErrors - prev.RxOverErrors; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("rx_over_errors +%d", d))
	}

	if d := cur.TxErrors - prev.TxErrors; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("tx_errors +%d", d))
	}

	if d := cur.TxDropped - prev.TxDropped; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("tx_dropped +%d", d))
	}

	if d := cur.TxCarrierErrs - prev.TxCarrierErrs; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("tx_carrier_errors +%d", d))
	}

	if d := cur.TxCollisions - prev.TxCollisions; d >= minDelta {
		deltas = append(deltas, fmt.Sprintf("tx_collisions +%d", d))
	}

	return deltas
}

// runIPLinkStats executes `ip -s -d -j link show` in the given network
// namespace (empty string for the root namespace).
func runIPLinkStats(netns string) []ipLinkEntry {
	var cmd *exec.Cmd
	if netns == "" {
		cmd = exec.Command("ip", "-s", "-d", "-j", "link", "show")
	} else {
		cmd = exec.Command("ip", "-n", netns, "-s", "-d", "-j", "link", "show")
	}

	out, err := cmd.Output()
	if err != nil {
		klog.V(4).Infof("Failed to run ip link stats (netns=%q): %v", netns, err)
		return nil
	}

	var entries []ipLinkEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		klog.V(4).Infof("Failed to parse ip link stats JSON (netns=%q): %v", netns, err)
		return nil
	}

	return entries
}

// listNetworkNamespaces returns the names of all network namespaces.
func listNetworkNamespaces() []string {
	out, err := exec.Command("ip", "-j", "netns", "list").Output()
	if err != nil {
		klog.V(4).Infof("Failed to list network namespaces: %v", err)
		return nil
	}
	// ip -j netns list returns [] or null when there are no namespaces
	if len(out) == 0 || strings.TrimSpace(string(out)) == "null" {
		return nil
	}

	var entries []ipNetnsEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		klog.V(4).Infof("Failed to parse network namespace JSON: %v", err)
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name != "" {
			names = append(names, e.Name)
		}
	}

	return names
}
