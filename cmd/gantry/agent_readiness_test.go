// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import "testing"

// Readiness gate order is contract, not style. When several conditions are
// unsatisfied at once the first gate supplies the reported reason, so the
// order decides which cause an operator is shown. This re-expresses the
// contract the membership-era tests carried: an agent that has not yet found
// a peer must say so, rather than blaming a downstream symptom.
func TestReadinessGateOrderReportsRootCause(t *testing.T) {
	// Every gate after the first is also unsatisfied, which is the normal
	// state early in a rollout.
	gates := []readinessGate{
		{reason: "cache scan not complete", ready: func() bool { return true }},
		{reason: "lease rendezvous has no connected DHT peer", ready: func() bool { return false }},
		{reason: "libp2p has no dialable advertised address", ready: func() bool { return false }},
		{reason: "containerd content store unavailable", ready: func() bool { return false }},
	}

	reason, ok := firstUnreadyGate(gates)
	if ok {
		t.Fatal("firstUnreadyGate reported ready with unsatisfied gates")
	}

	if reason != "lease rendezvous has no connected DHT peer" {
		t.Errorf("reason = %q, want the earliest unsatisfied gate", reason)
	}
}

func TestReadinessAllGatesSatisfied(t *testing.T) {
	gates := []readinessGate{
		{reason: "a", ready: func() bool { return true }},
		{reason: "b", ready: func() bool { return true }},
	}

	if reason, ok := firstUnreadyGate(gates); !ok {
		t.Errorf("firstUnreadyGate = %q, false; want ready", reason)
	}
}

// A gate after the first unsatisfied one must not be consulted: gates may be
// expensive (the containerd ping opens a client) and must not run once the
// probe has already decided.
func TestReadinessStopsAtFirstUnreadyGate(t *testing.T) {
	evaluated := false
	gates := []readinessGate{
		{reason: "first", ready: func() bool { return false }},
		{reason: "second", ready: func() bool { evaluated = true; return true }},
	}

	if _, ok := firstUnreadyGate(gates); ok {
		t.Fatal("want not ready")
	}

	if evaluated {
		t.Error("a later gate was evaluated after the probe already had its answer")
	}
}

func TestTransferAddrFamilyMismatch(t *testing.T) {
	tests := []struct {
		name           string
		transferListen string
		podIP          string
		want           bool
	}{
		{"v4 wildcard with v4 pod", "0.0.0.0:5001", "10.42.0.7", false},
		{"v4 wildcard with v6 pod", "0.0.0.0:5001", "fd00::1234", true},
		{"v6 wildcard with v6 pod", "[::]:5001", "fd00::1234", false},
		{"v6 wildcard with v4 pod", "[::]:5001", "10.42.0.7", true},
		{"empty host with v4 pod", ":5001", "10.42.0.7", false},
		{"empty host with v6 pod", ":5001", "fd00::1234", false},
		{"explicit v4 bind", "10.42.0.7:5001", "10.42.0.7", false},
		{"explicit v6 bind", "[fd00::1234]:5001", "fd00::1234", false},
		{"empty pod IP", "0.0.0.0:5001", "", false},
		{"unparseable listen", "notahostport", "10.42.0.7", false},
		{"unparseable pod IP", "0.0.0.0:5001", "not-an-ip", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transferAddrFamilyMismatch(tt.transferListen, tt.podIP); got != tt.want {
				t.Errorf("transferAddrFamilyMismatch(%q, %q) = %v; want %v", tt.transferListen, tt.podIP, got, tt.want)
			}
		})
	}
}
