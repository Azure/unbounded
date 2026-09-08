// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"net/http"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestClassifyPeerFetchError_Busy429(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		wantOutcome peerFetchOutcomeKind
		wantLabel   string
	}{
		{"too_many_requests", http.StatusTooManyRequests, peerFetchOutcomeBusy, "busy"},
		{"service_unavailable", http.StatusServiceUnavailable, peerFetchOutcomePeerServerError, "server_error"},
		{"forbidden", http.StatusForbidden, peerFetchOutcomeAuthOrConfigError, "auth_or_config"},
		{"teapot", http.StatusTeapot, peerFetchOutcomeProtocolError, "protocol_error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, label := classifyPeerFetchError(&ifaces.ErrPeerHTTPStatus{PeerAddr: "peer:5001", StatusCode: tc.status})
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %v, want %v", outcome, tc.wantOutcome)
			}

			if label != tc.wantLabel {
				t.Errorf("label = %q, want %q", label, tc.wantLabel)
			}
		})
	}
}

func TestPeerAttemptSummary_BusyIsNotAllStale(t *testing.T) {
	// A round in which every attempt returned busy (429) must NOT be treated
	// as all-stale-or-filtered: the peers are alive and will serve once their
	// load drops, so we must not escalate as if they were dead.
	busy := updatePeerSummary(peerAttemptSummary{attempted: 1}, peerFetchOutcomeBusy)
	if busy.busy != 1 {
		t.Fatalf("busy count = %d, want 1", busy.busy)
	}

	if busy.allStaleOrFiltered() {
		t.Error("busy-only round must not be all-stale-or-filtered")
	}

	// A stale-only round, by contrast, IS all-stale-or-filtered.
	stale := updatePeerSummary(peerAttemptSummary{attempted: 1}, peerFetchOutcomeStaleProvider)
	if !stale.allStaleOrFiltered() {
		t.Error("stale-only round should be all-stale-or-filtered")
	}
}

func TestJitteredBackoff_WithinBounds(t *testing.T) {
	base := time.Second
	lo := base - base/4
	hi := base + base/4

	for i := 0; i < 2000; i++ {
		got := jitteredBackoff(base)
		if got < lo || got > hi {
			t.Fatalf("jitteredBackoff(%v) = %v, want within [%v, %v]", base, got, lo, hi)
		}
	}

	if got := jitteredBackoff(0); got != 0 {
		t.Errorf("jitteredBackoff(0) = %v, want 0", got)
	}
}
