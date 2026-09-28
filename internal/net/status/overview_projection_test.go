// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"reflect"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestPeerHealthyForOverview(t *testing.T) {
	now := time.Unix(1000, 0)

	for _, tc := range []struct {
		name    string
		check   *v1alpha1.HealthCheckPeerStatus
		age     time.Duration
		missing bool
		want    bool
	}{
		{name: "unknown", missing: true},
		{name: "recent", age: time.Minute, want: true},
		{name: "boundary", age: 3 * time.Minute},
		{name: "stale", age: 4 * time.Minute},
		{name: "future preserves legacy behavior", age: -time.Minute, want: true},
		{name: "up", check: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "up"}, missing: true, want: true},
		{name: "Up", check: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "Up"}, missing: true, want: true},
		{name: "UP is not up", check: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "UP"}},
		{name: "enabled down overrides handshake", check: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "down"}},
		{name: "disabled uses handshake", check: &v1alpha1.HealthCheckPeerStatus{Status: "down"}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := v1alpha1.PeerStatus{HealthCheck: tc.check}
			if !tc.missing {
				peer.Tunnel.LastHandshake = now.Add(-tc.age)
			}

			if got := PeerHealthyForOverview(&peer, now); got != tc.want {
				t.Fatalf("healthy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRouteMismatchForOverview(t *testing.T) {
	yes, no := true, false
	for _, expected := range []*bool{nil, &no, &yes} {
		for _, present := range []*bool{nil, &no, &yes} {
			route := v1alpha1.RouteEntry{
				NextHops: []v1alpha1.NextHop{{}, {Expected: expected, Present: present}},
			}

			want := (expected != nil && *expected) != (present != nil && *present)
			if got := RouteMismatchForOverview(route); got != want {
				t.Fatalf("expected=%v present=%v mismatch=%v, want %v", expected, present, got, want)
			}
		}
	}

	if RouteMismatchForOverview(v1alpha1.RouteEntry{}) {
		t.Fatal("an empty route has no mismatched hops")
	}
}

func TestOverviewProjectionPreservesFactsAndMetadata(t *testing.T) {
	now := time.Unix(1000, 0)
	yes := true
	full := v1alpha1.NodeStatusResponse{
		Timestamp: now,
		NodeInfo: v1alpha1.NodeInfo{
			Name: "node", SiteName: "site", IsGateway: true, K8sReady: "NotReady",
			WireGuard: &v1alpha1.WireGuardStatusInfo{Interface: "wg0"},
		},
		HealthCheck: &v1alpha1.HealthCheckStatus{Healthy: false, Summary: "blocked"},
		NodeErrors:  []v1alpha1.NodeError{{Type: "cni", Message: "bootstrap blocked"}},
		FetchError:  "stale", LastPushTime: &now, StatusSource: "stale-cache",
		NodePodInfo: &v1alpha1.NodePodInfo{PodName: "pod"},
		Peers: []v1alpha1.PeerStatus{
			{HealthCheck: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "up"}},
			{HealthCheck: &v1alpha1.HealthCheckPeerStatus{Enabled: true, Status: "down"}},
		},
		RoutingTable: v1alpha1.RoutingTableInfo{Routes: []v1alpha1.RouteEntry{
			{}, {NextHops: []v1alpha1.NextHop{{Expected: &yes}}},
		}},
		BpfEntries: []v1alpha1.BpfEntry{{CIDR: "10.0.0.0/24"}},
	}

	overview := OverviewFromStatus(&full, now)
	if overview.PeerCount != 2 || overview.HealthyPeers != 1 || overview.RouteCount != 2 || !overview.RouteMismatch {
		t.Fatalf("observed facts changed: %+v", overview)
	}

	metadata := OverviewMetadata(overview)
	want := full
	want.Peers = nil
	want.RoutingTable = v1alpha1.RoutingTableInfo{}

	want.BpfEntries = nil
	if !reflect.DeepEqual(metadata, want) {
		t.Fatalf("metadata changed: got %+v, want %+v", metadata, want)
	}

	if len(full.Peers) != 2 || len(full.RoutingTable.Routes) != 2 || len(full.BpfEntries) != 1 {
		t.Fatal("projection mutated the original snapshot")
	}
}
