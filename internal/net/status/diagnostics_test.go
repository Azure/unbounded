// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestPeerLinkHealthyForDiagnostics(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name      string
		check     *v1alpha1.HealthCheckPeerStatus
		handshake time.Time
		healthy   bool
	}{
		{"unknown", nil, time.Time{}, false},
		{"recent", nil, now.Add(-time.Minute), true},
		{"boundary", nil, now.Add(-3 * time.Minute), false},
		{"future", nil, now.Add(time.Minute), true},
		{"normalized up", &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: " UP "}, time.Time{}, true},
		{"disabled nonempty up", &v1alpha1.HealthCheckPeerStatus{Status: " up "}, time.Time{}, true},
		{"disabled nonempty down", &v1alpha1.HealthCheckPeerStatus{Status: " DOWN "}, now, false},
		{"enabled empty", &v1alpha1.HealthCheckPeerStatus{Enabled: true}, now, false},
		{"disabled whitespace uses handshake", &v1alpha1.HealthCheckPeerStatus{Status: "  "}, now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := v1alpha1.PeerStatus{HealthCheck: tc.check, Tunnel: v1alpha1.PeerTunnelStatus{LastHandshake: tc.handshake}}
			if got := PeerLinkHealthyForDiagnostics(&peer, now); got != tc.healthy {
				t.Fatalf("healthy=%v, want %v", got, tc.healthy)
			}
		})
	}
}

func TestUnhealthyPeerLinkCountKeepsDistinctLinks(t *testing.T) {
	now := time.Unix(1000, 0)

	peers := []v1alpha1.PeerStatus{
		{Name: "same", HealthCheck: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "DOWN"}},
		{Name: "same", HealthCheck: &v1alpha1.HealthCheckPeerStatus{Status: " down "}, Tunnel: v1alpha1.PeerTunnelStatus{LastHandshake: now}},
		{Name: "same", HealthCheck: &v1alpha1.HealthCheckPeerStatus{Status: " UP "}},
	}
	if got := UnhealthyPeerLinkCount(peers, now); got != 2 {
		t.Fatalf("unhealthy links=%d, want 2", got)
	}

	if got := UnhealthyPeerLinkCount(nil, now); got != 0 {
		t.Fatalf("empty links=%d, want 0", got)
	}
}

func TestRouteMismatchCountIncludesEveryHop(t *testing.T) {
	yes, no := true, false

	routes := []v1alpha1.RouteEntry{
		{NextHops: []v1alpha1.NextHop{
			{Expected: &yes},
			{Present: &yes},
			{Expected: &yes, Present: &no},
			{Expected: &yes, Present: &yes},
			{Expected: &no},
			{},
		}},
		{NextHops: []v1alpha1.NextHop{{Expected: &no, Present: &yes}}},
	}
	if got := RouteMismatchCount(routes); got != 4 {
		t.Fatalf("mismatched hops=%d, want 4", got)
	}

	if got := RouteMismatchCount(nil); got != 0 {
		t.Fatalf("empty routes=%d, want 0", got)
	}
}
