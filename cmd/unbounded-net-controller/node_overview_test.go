// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestNodeOverviewCacheReplacesOnlyRoutineState(t *testing.T) {
	cache := NewNodeStatusCache()
	cache.StoreFull("node", NodeStatusResponse{Peers: []WireGuardPeerStatus{{Name: "peer"}}}, "push")

	var notified statusv1alpha1.NodeStatusOverview

	cache.SetOnOverviewChange(func(name string, overview statusv1alpha1.NodeStatusOverview) {
		if name != "node" || cache.Len() != 1 {
			t.Error("notification has wrong identity or ran under the cache lock")
		}

		notified = overview
	})
	overview := statusv1alpha1.NodeStatusOverview{
		PeerCount: 20, HealthyPeers: 17, RouteCount: 30, RouteMismatch: true,
		NodeErrors: []NodeError{{Type: "cni", Message: "bootstrap blocked"}},
	}

	revision, err := cache.StoreOverview("node", overview, "ws")
	if err != nil || revision != 2 {
		t.Fatalf("store: revision=%d error=%v", revision, err)
	}

	cached, ok := cache.Get("node")
	if !ok || cached.Overview == nil || cached.Overview.PeerCount != 20 || cached.peerIdentity != nil {
		t.Fatalf("unexpected overview wire state: %+v", cached)
	}

	if cached.Status.Peers != nil || cached.Status.RoutingTable.Routes != nil || cached.Status.BpfEntries != nil {
		t.Fatal("summary retained diagnostic arrays")
	}

	cached.Overview.PeerCount = 99

	unchanged, _ := cache.Get("node")
	if unchanged.Overview.PeerCount != 20 {
		t.Fatal("changing the returned overview mutated the cache")
	}

	if notified.NodeInfo.Name != "node" || notified.StatusSource != "ws" || len(cached.Status.NodeErrors) != 1 {
		t.Fatal("notification or metadata lost identity, source, or errors")
	}

	rev, resync, err := cache.ApplyDelta("node", revision, map[string]json.RawMessage{}, "push")
	if err != nil || !resync || rev != revision {
		t.Fatalf("legacy delta must not apply to a summary: %d %v %v", rev, resync, err)
	}

	snapshot := cache.GetAll()
	cache.UpdateSource("node", "apiserver-ws")

	if snapshot["node"].Source != "ws" || notified.StatusSource != "apiserver-ws" {
		t.Fatal("source change mutated an older snapshot or lost its notification")
	}

	if next := cache.StoreFull("node", NodeStatusResponse{Peers: []WireGuardPeerStatus{{Name: "legacy"}}}, "push"); next != 3 {
		t.Fatalf("legacy full resync revision=%d", next)
	}

	legacy, _ := cache.Get("node")
	if legacy.Overview != nil || len(legacy.Status.Peers) != 1 {
		t.Fatal("explicit legacy full resync did not replace summary state")
	}
}

func TestNodeOverviewCacheRejectsInvalidFacts(t *testing.T) {
	for _, overview := range []statusv1alpha1.NodeStatusOverview{
		{NodeInfo: NodeInfo{Name: "different-node"}},
		{PeerCount: -1},
		{HealthyPeers: -1},
		{PeerCount: 1, HealthyPeers: 2},
		{RouteCount: -1},
		{RouteMismatchCount: -1},
		{UnhealthyPeerLinks: -1},
		{RouteMismatchCount: 1},
	} {
		cache := NewNodeStatusCache()
		if _, err := cache.StoreOverview("node", overview, "ws"); err == nil || cache.Len() != 0 {
			t.Fatalf("invalid summary accepted: %+v", overview)
		}
	}

	if _, err := NewNodeStatusCache().StoreOverview("", statusv1alpha1.NodeStatusOverview{}, ""); err == nil {
		t.Fatal("empty node identity accepted")
	}
}

func TestClusterOverviewPreservesCountsAndEnrichment(t *testing.T) {
	c := NewClusterStatusCache(&healthState{})
	c.status = &ClusterStatusResponse{
		Nodes: []*NodeStatusResponse{{NodeInfo: NodeInfo{Name: "node", K8sReady: "Ready", ProviderID: "provider"}}},
	}
	c.nodeIndex["node"] = 0
	overview := statusv1alpha1.NodeStatusOverview{
		NodeInfo:     NodeInfo{Name: "node", SiteName: "site", WireGuard: &WireGuardStatusInfo{Interface: "wg0"}},
		StatusSource: "ws", PeerCount: 20, HealthyPeers: 17, RouteCount: 30, RouteMismatch: true,
		RouteMismatchCount: 2, UnhealthyPeerLinks: 3,
	}
	c.PatchOverview("node", overview)
	snapshot := c.Get()

	row := buildClusterSummary(snapshot).NodeSummaries[0]
	if row.PeerCount != 20 || row.HealthyPeers != 17 || row.RouteCount != 30 || !row.RouteMismatch ||
		row.K8sReady != "Ready" || row.CniStatus != "Route mismatch" || row.SiteName != "site" {
		t.Fatalf("summary lost observed facts or enriched fields: %+v", row)
	}

	if snapshot.Nodes[0].NodeInfo.ProviderID != "provider" {
		t.Fatal("controller enrichment was lost")
	}

	problems := collectClusterProblems(snapshot)
	if len(problems) != 1 || len(problems[0].Errors) != 2 {
		t.Fatalf("summary health/mismatch problems were hidden: %+v", problems)
	}

	overview.PeerCount = 25
	overview.NodeErrors = []NodeError{{Type: "cni", Message: "blocked"}}
	c.PatchOverview("node", overview)

	if !reflect.DeepEqual(buildClusterSummary(snapshot).NodeSummaries[0], row) {
		t.Fatal("patching changed a previously returned snapshot")
	}

	nextRow := buildClusterSummary(c.Get()).NodeSummaries[0]
	if nextRow.PeerCount != 25 || nextRow.FirstError != "blocked" || nextRow.CniTone != "danger" {
		t.Fatalf("summary update lost errors or counts: %+v", nextRow)
	}

	c.PatchNode("node", NodeStatusResponse{NodeInfo: overview.NodeInfo, Peers: []WireGuardPeerStatus{{}}})

	if legacy := buildClusterSummary(c.Get()).NodeSummaries[0]; legacy.PeerCount != 1 {
		t.Fatal("legacy update retained stale explicit summary counts")
	}
}

func TestClusterOverviewWireIgnoresDiagnosticArrays(t *testing.T) {
	node := &NodeStatusResponse{NodeInfo: NodeInfo{Name: "node"}}
	status := &ClusterStatusResponse{
		Nodes: []*NodeStatusResponse{node},
		NodeOverviews: map[string]*statusv1alpha1.NodeStatusOverview{
			"node": {PeerCount: 5, HealthyPeers: 4, RouteCount: 9},
		},
	}

	before, err := json.Marshal(buildClusterSummary(status))
	if err != nil {
		t.Fatal(err)
	}

	node.Peers = make([]WireGuardPeerStatus, 10000)
	node.RoutingTable.Routes = make([]RouteEntry, 10000)
	node.BpfEntries = make([]BpfEntry, 10000)

	after, err := json.Marshal(buildClusterSummary(status))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(before, after) {
		t.Fatal("overview wire size or facts depend on diagnostic arrays")
	}

	for _, field := range []string{`"peers":`, `"routingTable":`, `"bpfEntries":`, `"NodeOverviews":`} {
		if strings.Contains(string(after), field) {
			t.Fatalf("overview exposed %s", field)
		}
	}
}

func TestClusterOverviewConcurrentSnapshots(t *testing.T) {
	c := NewClusterStatusCache(&healthState{})
	c.status = &ClusterStatusResponse{}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				c.PatchOverview("node", statusv1alpha1.NodeStatusOverview{NodeInfo: NodeInfo{Name: "node"}, Timestamp: time.Now()})
				buildClusterSummary(c.Get())
			}
		})
	}

	wg.Wait()
}
