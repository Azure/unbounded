// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

// defaultRateThreshold is the minimum per-interval counter increment
// that is considered significant. Increments below this threshold are
// treated as background noise and do not raise a warning.
const defaultRateThreshold uint64 = 5

// linkStatsMonitor periodically collects network interface statistics across
// all network namespaces and detects incrementing error/drop counters.
//
// Warnings describe only the increments observed in the most recent interval
// (current - previous). A counter that stays flat produces no warning, so a
// warning clears on the first quiet interval after recovery and the cleared
// state is pushed to the controller with the next status update.
type linkStatsMonitor struct {
	interval      time.Duration
	rateThreshold uint64

	mu       sync.Mutex
	previous map[string]linkCounters // key: "netns/ifname"
	warnings []string                // warnings from the most recent interval
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
		interval:      interval,
		rateThreshold: defaultRateThreshold,
		previous:      make(map[string]linkCounters),
	}
}

// Start runs the monitor loop until the context is canceled.
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

	m.update(current)
}

// update compares current counters with the previous sample, replaces the
// active warnings with the increments seen in this interval and makes current
// the baseline for the next interval.
func (m *linkStatsMonitor) update(current map[string]linkCounters) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var warnings []string

	for key, cur := range current {
		prev, ok := m.previous[key]
		if !ok {
			continue
		}

		if deltas := detectIncrements(prev, cur, m.rateThreshold); len(deltas) > 0 {
			warnings = append(warnings, fmt.Sprintf("interface %s: %s", key, strings.Join(deltas, ", ")))
		}
	}

	sort.Strings(warnings)

	if len(warnings) > 0 {
		klog.V(2).Infof("Link stats: detected %d interface(s) with incrementing error counters", len(warnings))

		for _, w := range warnings {
			klog.V(2).Infof("  %s", w)
		}
	} else if len(m.warnings) > 0 {
		klog.V(2).Info("Link stats: error counters are no longer incrementing, clearing warnings")
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
// descriptions of any that increased by at least minDelta. A counter that
// went backwards (for example, because the interface was recreated) is
// treated as reset and not reported.
func detectIncrements(prev, cur linkCounters, minDelta uint64) []string {
	fields := []struct {
		name      string
		prev, cur uint64
	}{
		{"rx_errors", prev.RxErrors, cur.RxErrors},
		{"rx_dropped", prev.RxDropped, cur.RxDropped},
		{"rx_over_errors", prev.RxOverErrors, cur.RxOverErrors},
		{"tx_errors", prev.TxErrors, cur.TxErrors},
		{"tx_dropped", prev.TxDropped, cur.TxDropped},
		{"tx_carrier_errors", prev.TxCarrierErrs, cur.TxCarrierErrs},
		{"tx_collisions", prev.TxCollisions, cur.TxCollisions},
	}

	var deltas []string

	for _, f := range fields {
		if f.cur <= f.prev {
			continue
		}

		if d := f.cur - f.prev; d >= minDelta {
			deltas = append(deltas, fmt.Sprintf("%s +%d", f.name, d))
		}
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
