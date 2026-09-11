// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
)

// rewriteWildcardMultiaddr substitutes a Pod IP into wildcard listen
// addresses so the agent publishes dialable p2p multiaddrs in its
// self-announce annotation. Without the substitution, peers receive
// /ip4/0.0.0.0/tcp/4001 and silently fail to connect, deadlocking
// libp2p bootstrap on a cold cluster.
func TestRewriteWildcardMultiaddr(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		podIP string
		want  string
	}{
		{
			name:  "ipv4 wildcard with pod IP",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "10.42.0.7",
			want:  "/ip4/10.42.0.7/tcp/4001",
		},
		{
			name:  "ipv4 wildcard without pod IP returns empty (skip)",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "",
			want:  "",
		},
		{
			name:  "ipv6 wildcard with v6 pod IP rewrites to /ip6/",
			in:    "/ip6/::/tcp/4001",
			podIP: "fd00:10:244::7",
			want:  "/ip6/fd00:10:244::7/tcp/4001",
		},
		{
			name:  "ipv4 wildcard with v6 pod IP skips (no v6 listener)",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "fd00:10:244::7",
			want:  "",
		},
		{
			name:  "ipv6 wildcard with v4 pod IP skips (no v4 listener under /ip6/)",
			in:    "/ip6/::/tcp/4001",
			podIP: "10.42.0.7",
			want:  "",
		},
		{
			name:  "wildcard with unparseable pod IP returns empty",
			in:    "/ip4/0.0.0.0/tcp/4001",
			podIP: "not-an-ip",
			want:  "",
		},
		{
			name:  "concrete ipv4 passes through",
			in:    "/ip4/10.42.0.7/tcp/4001",
			podIP: "10.42.0.7",
			want:  "/ip4/10.42.0.7/tcp/4001",
		},
		{
			name:  "concrete ipv4 loopback returns empty",
			in:    "/ip4/127.0.0.1/tcp/4001",
			podIP: "10.42.0.7",
			want:  "",
		},
		{
			name:  "concrete ipv6 passes through",
			in:    "/ip6/2001:db8::1/tcp/4001",
			podIP: "10.42.0.7",
			want:  "/ip6/2001:db8::1/tcp/4001",
		},
		{
			name:  "concrete ipv6 loopback returns empty",
			in:    "/ip6/::1/tcp/4001",
			podIP: "fd00:10:244::7",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteWildcardMultiaddr(tc.in, tc.podIP)
			if got != tc.want {
				t.Errorf("rewriteWildcardMultiaddr(%q, %q) = %q, want %q",
					tc.in, tc.podIP, got, tc.want)
			}
		})
	}
}

// advertisedTransferAddr leaves empty when the listen host is a
// wildcard and no Pod IP is available, so members.Snapshot falls back
// to composing podIP:port from the pod's status.podIP. A non-empty
// 0.0.0.0:port published in the annotation would override that
// fallback and produce an unreachable advertised address.
//
// It also leaves empty for the cross-family case (e.g. v4 wildcard
// with a v6 Pod IP) so that an undialable annotation isn't published.
// In that case the readiness probe (see TestTransferAddrFamilyMismatch)
// is responsible for failing the rollout so the broken pod never goes
// Ready and never appears in peers' Snapshot views.
func TestAdvertisedTransferAddr(t *testing.T) {
	cases := []struct {
		name           string
		transferListen string
		podIP          string
		want           string
	}{
		{
			name:           "ipv4 wildcard + ipv4 pod IP composes",
			transferListen: "0.0.0.0:5001",
			podIP:          "10.42.0.7",
			want:           "10.42.0.7:5001",
		},
		{
			name:           "ipv4 wildcard + empty pod IP returns empty",
			transferListen: "0.0.0.0:5001",
			podIP:          "",
			want:           "",
		},
		{
			name:           "ipv4 wildcard + ipv6 pod IP returns empty (cross-family)",
			transferListen: "0.0.0.0:5001",
			podIP:          "fd00::1234",
			want:           "",
		},
		{
			name:           "ipv6 wildcard + ipv6 pod IP composes with brackets",
			transferListen: "[::]:5001",
			podIP:          "fd00::1234",
			want:           "[fd00::1234]:5001",
		},
		{
			name:           "ipv6 wildcard + ipv4 pod IP returns empty (cross-family)",
			transferListen: "[::]:5001",
			podIP:          "10.42.0.7",
			want:           "",
		},
		{
			name:           "ipv6 wildcard + empty pod IP returns empty",
			transferListen: "[::]:5001",
			podIP:          "",
			want:           "",
		},
		{
			name:           "explicit bind passes through",
			transferListen: "10.42.0.7:5001",
			podIP:          "10.42.0.7",
			want:           "10.42.0.7:5001",
		},
		{
			name:           "empty host (dual-stack) + ipv4 pod IP composes",
			transferListen: ":5001",
			podIP:          "10.42.0.7",
			want:           "10.42.0.7:5001",
		},
		{
			name:           "empty host (dual-stack) + ipv6 pod IP composes",
			transferListen: ":5001",
			podIP:          "fd00::1234",
			want:           "[fd00::1234]:5001",
		},
		{
			name:           "unparseable listen passes through verbatim",
			transferListen: "notahostport",
			podIP:          "10.42.0.7",
			want:           "notahostport",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := advertisedTransferAddr(tc.transferListen, tc.podIP)
			if got != tc.want {
				t.Errorf("advertisedTransferAddr(%q, %q) = %q, want %q",
					tc.transferListen, tc.podIP, got, tc.want)
			}
		})
	}
}

// transferAddrFamilyMismatch must return true exactly for the
// configurations where advertisedTransferAddr drops to "" because of
// a cross-family wildcard listener (not because of a missing Pod IP
// or a non-K8s deploy). The readiness probe uses this to fail
// /readyz with a targeted message when the transfer endpoint is
// misconfigured for the pod's IP family.
func TestTransferAddrFamilyMismatch(t *testing.T) {
	cases := []struct {
		name           string
		transferListen string
		podIP          string
		want           bool
	}{
		{"v4 wildcard + v4 pod -> OK", "0.0.0.0:5001", "10.42.0.7", false},
		{"v4 wildcard + v6 pod -> MISMATCH", "0.0.0.0:5001", "fd00::1234", true},
		{"v6 wildcard + v6 pod -> OK", "[::]:5001", "fd00::1234", false},
		{"v6 wildcard + v4 pod -> MISMATCH", "[::]:5001", "10.42.0.7", true},
		{"empty host (dual-stack) + v4 pod -> OK", ":5001", "10.42.0.7", false},
		{"empty host (dual-stack) + v6 pod -> OK", ":5001", "fd00::1234", false},
		{"explicit v4 bind -> not a wildcard -> OK", "10.42.0.7:5001", "10.42.0.7", false},
		{"explicit v6 bind -> not a wildcard -> OK", "[fd00::1234]:5001", "fd00::1234", false},
		{"empty pod IP (non-K8s) -> never a mismatch", "0.0.0.0:5001", "", false},
		{"unparseable listen -> never a mismatch (annotation passes verbatim)", "notahostport", "10.42.0.7", false},
		{"unparseable pod IP -> never a mismatch (treat as opaque)", "0.0.0.0:5001", "not-an-ip", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := transferAddrFamilyMismatch(tc.transferListen, tc.podIP); got != tc.want {
				t.Errorf("transferAddrFamilyMismatch(%q, %q) = %v; want %v",
					tc.transferListen, tc.podIP, got, tc.want)
			}
		})
	}
}

// bootstrapConvergenceTarget gates "bootstrap converged; ceasing
// periodic dials" on RoutingTableSize ≥ target. A fixed target of 5
// loops forever on small clusters (2-3 nodes) because the routing
// table can never grow that big; for single-node deploys we return
// 0 so the loop exits immediately on the first pass (no peers to
// dial; routing-table will stay empty by definition).
func TestBootstrapConvergenceTarget(t *testing.T) {
	cases := []struct {
		name         string
		snapshotSize int // includes self
		maxSize      int
		want         int
	}{
		{name: "single-node cluster targets 0", snapshotSize: 1, maxSize: 5, want: 0},
		{name: "empty snapshot defensively returns 0", snapshotSize: 0, maxSize: 5, want: 0},
		{name: "2-node cluster targets 1 peer", snapshotSize: 2, maxSize: 5, want: 1},
		{name: "3-node cluster targets 2 peers", snapshotSize: 3, maxSize: 5, want: 2},
		{name: "5-node cluster targets 4 peers", snapshotSize: 5, maxSize: 5, want: 4},
		{name: "6-node cluster caps at max=5", snapshotSize: 6, maxSize: 5, want: 5},
		{name: "100-node cluster caps at max=5", snapshotSize: 100, maxSize: 5, want: 5},
		{name: "custom max=3 caps at 3", snapshotSize: 10, maxSize: 3, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bootstrapConvergenceTarget(tc.snapshotSize, tc.maxSize); got != tc.want {
				t.Errorf("bootstrapConvergenceTarget(%d, %d) = %d, want %d",
					tc.snapshotSize, tc.maxSize, got, tc.want)
			}
		})
	}
}

// TestRoutingTableTarget guards eighth-review #3: the kad-dht
// routing-table target is the count of *other* peers we expect to
// learn about - snapshotSize-1 - NOT the raw snapshot size. Passing
// snapshotSize as target meant a fully-converged N-node cluster
// could only ever reach (N-1)/N of the target, pegging the DHT
// health score at < 1.0 (0.5 in a 2-node deploy, 0.66 in a 3-node
// deploy, 0.75 in a 4-node deploy …) and tripping degraded-cluster
// alerts on healthy clusters.
//
// Single-node carve-out: snapshot ≤ 1 -> 0. The lone-agent case
// must not produce a positive target (the routing table has nothing
// to learn) and the bootstrap loop already encodes that contract via
// bootstrapConvergenceTarget; routingTableTarget agrees.
func TestRoutingTableTarget(t *testing.T) {
	cases := []struct {
		name         string
		snapshotSize int
		maxSize      int
		want         int
	}{
		{"empty snapshot returns 0", 0, 256, 0},
		{"single-self snapshot returns 0 (lone-agent carve-out)", 1, 256, 0},
		{"2-node cluster expects 1 peer in routing table", 2, 256, 1},
		{"3-node cluster expects 2 peers in routing table", 3, 256, 2},
		{"10-node cluster expects 9 peers in routing table", 10, 256, 9},
		{"snapshot exactly at cap returns cap (snapshot-1 = max)", 257, 256, 256},
		{"snapshot above cap clamps to cap", 1000, 256, 256},
		{"small max applies (snapshot-1 > max)", 100, 4, 4},
		{"small max with snapshot just over: snapshot-1 ≤ max returns snapshot-1", 5, 4, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routingTableTarget(tc.snapshotSize, tc.maxSize); got != tc.want {
				t.Errorf("routingTableTarget(snapshotSize=%d, maxSize=%d) = %d; want %d",
					tc.snapshotSize, tc.maxSize, got, tc.want)
			}
		})
	}
}
