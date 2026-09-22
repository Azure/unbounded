// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"slices"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestOverviewDiagnosticMessages(t *testing.T) {
	for _, tc := range []struct {
		name       string
		overview   v1alpha1.NodeStatusOverview
		providerID string
		want       []string
	}{
		{
			name:     "route mismatches",
			overview: v1alpha1.NodeStatusOverview{RouteMismatchCount: 2},
			want:     []string{"2 route next-hop mismatches (expected vs present)"},
		},
		{
			name:     "unhealthy peer links",
			overview: v1alpha1.NodeStatusOverview{UnhealthyPeerLinks: 3},
			want:     []string{"3 peer links are unhealthy"},
		},
		{
			name:       "Azure IPIP",
			overview:   v1alpha1.NodeStatusOverview{UsesIPIP: true},
			providerID: "azure:///subscriptions/example",
			want:       []string{"IPIP tunnel protocol is not supported on Azure (IP protocol 4 is blocked by the platform)"},
		},
		{
			name:       "non-Azure IPIP",
			overview:   v1alpha1.NodeStatusOverview{UsesIPIP: true},
			providerID: "aws:///instance/example",
		},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OverviewDiagnosticMessages(tc.overview, tc.providerID); !slices.Equal(got, tc.want) {
				t.Fatalf("messages=%q, want %q", got, tc.want)
			}
		})
	}
}

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
