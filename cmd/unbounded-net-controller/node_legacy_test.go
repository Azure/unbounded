// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func retentionFixture(peers int) NodeStatusResponse {
	status := protoToNodeStatus(measurementTestStatus(peers))
	status.NodeInfo.Name = "node"
	status.RoutingTable.Routes = []RouteEntry{{Destination: "10.0.0.0/8", NextHops: []NextHop{{Device: "wg0"}}}}
	status.BpfEntries = []BpfEntry{{CIDR: "10.0.0.0/8", Node: "private-detail-marker"}}

	return status
}

func assertThinStatus(t *testing.T, entry *CachedNodeStatus, peerCount int) {
	t.Helper()

	if entry == nil || entry.Overview == nil || entry.Overview.PeerCount != peerCount ||
		len(entry.Status.Peers) != 0 || len(entry.Status.RoutingTable.Routes) != 0 ||
		len(entry.Status.BpfEntries) != 0 || entry.peerIdentity != nil {
		t.Fatal("routine cache retained details/memo or lost observed facts")
	}
}

func TestBoundNodeCacheRetainsOverviewOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		cache := NewNodeStatusCache()
		status := retentionFixture(1024)
		cache.StoreFull("node", status, "ws")
		cache.BindDetails(manager)
		assertThinStatus(t, cache.entries["node"], 1024)

		if _, conflict, err := cache.ApplyDelta("node", 1, map[string]json.RawMessage{"statusSource": []byte(`"ws"`)}, "ws"); err != nil || !conflict {
			t.Fatal("pre-binding payload remained usable as a wire base")
		}

		fullCallbacks := 0
		overviewCallbacks := 0

		cache.SetOnChange(func(string, *NodeStatusResponse) { fullCallbacks++ })
		cache.SetOnOverviewChange(func(_ string, overview statusv1alpha1.NodeStatusOverview) {
			overviewCallbacks++

			if overview.PeerCount != 1024 || overview.RouteCount != 1 {
				t.Error("callback lost observed counts")
			}
		})

		revision, err := cache.StoreFullChecked("node", status, "ws")
		if err != nil {
			t.Fatal(err)
		}

		assertThinStatus(t, cache.entries["node"], 1024)

		base, _, ok := manager.LegacyBase("node", revision)
		if !ok || &base.Peers[0] != &status.Peers[0] {
			t.Fatal("TTL wire base missing or deeply copied")
		}

		time.Sleep(time.Second)

		measurements, err := statuspkg.PeerMeasurementsToProto(status.Peers)
		if err != nil {
			t.Fatal(err)
		}

		revision, conflict, err := cache.ApplyParsedDelta("node", revision, parsedDelta{peerMeasurements: measurements}, "ws")
		if err != nil || conflict {
			t.Fatalf("measurement delta: conflict=%v error=%v", conflict, err)
		}

		assertThinStatus(t, cache.entries["node"], 1024)

		if _, memo, ok := manager.LegacyBase("node", revision); !ok || memo == nil {
			t.Fatal("measurement identity memo was not kept with details")
		}

		snapshot, _ := manager.cache.Get("node")

		time.Sleep(time.Second)
		cache.Get("node")
		cache.GetAll()
		cache.UpdateSource("node", "push")

		if entry, ok := cache.Get("node"); !ok || entry.Overview.StatusSource != "push" || entry.Status.StatusSource != "push" {
			t.Fatal("source update left inconsistent overview metadata")
		}

		after, _ := manager.cache.Get("node")

		if after.ExpiresAt != snapshot.ExpiresAt || fullCallbacks != 0 || overviewCallbacks != 3 {
			t.Fatal("routine read/source update refreshed TTL or sent full data")
		}

		time.Sleep(9 * time.Second)
		synctest.Wait()
		assertNodeDetailEntries(t, manager.cache, 0)
		assertThinStatus(t, cache.entries["node"], 1024)

		if _, conflict, err := cache.ApplyParsedDelta("node", revision, parsedDelta{peerMeasurements: measurements}, "ws"); err != nil || !conflict {
			t.Fatal("expired legacy base did not require resync")
		}
	})
}

func TestBoundNodeCacheSummaryDeletionAndClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		cache := NewNodeStatusCache()
		cache.BindDetails(manager)

		status := retentionFixture(3)
		cache.StoreFull("node", status, "ws")
		before, _ := manager.cache.Get("node")

		time.Sleep(time.Second)

		if _, err := cache.StoreOverview("node", statuspkg.OverviewFromStatus(&status, time.Now()), "push"); err != nil {
			t.Fatal(err)
		}

		after, _ := manager.cache.Get("node")
		if after.ExpiresAt != before.ExpiresAt {
			t.Fatal("summary publication refreshed details")
		}

		cache.CleanupStaleEntries(map[string]bool{})
		assertNodeDetailEntries(t, manager.cache, 0)
		cache.StoreFull("node", status, "ws")
		cache.Delete("node")
		assertNodeDetailEntries(t, manager.cache, 0)
		manager.Close()

		if _, err := cache.StoreFullChecked("node", status, "ws"); err == nil {
			t.Fatal("closed lifecycle silently accepted full data")
		}

		if cache.Len() != 0 {
			t.Fatal("closed lifecycle repopulated routine cache")
		}
	})
}

func TestBoundNodeCacheRejectsReplacedDeltaBase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		cache := NewNodeStatusCache()
		cache.BindDetails(manager)

		status := retentionFixture(3)
		revision := cache.StoreFull("node", status, "ws")
		previous := cache.entries["node"]
		base, _, _ := manager.LegacyBase("node", revision)
		request := manager.Request("node", true)

		if err := manager.Complete("node", request.RequestID, &status); err != nil {
			t.Fatal(err)
		}

		if _, conflict, err := cache.commitParsedDeltaBase("node", previous, &status, nil, "ws", base); err != nil || !conflict {
			t.Fatal("stale delta replaced a newer one-shot result")
		}

		if result := manager.Result("node", request.RequestID); result.State != statusv1alpha1.NodeDetailComplete {
			t.Fatal("delta invalidated requested result")
		}
	})
}

func TestBoundNodeCacheDisablesLegacyBridgeObserver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		manager := testDetailRequests(t, nodeDetailRequestHooks{})
		cache := NewNodeStatusCache()
		cache.ObserveLegacyDetails(manager)

		status := retentionFixture(3)
		revision := cache.StoreFull("node", status, "ws")
		cache.BindDetails(manager)

		if cache.legacyObserver != nil {
			t.Fatal("thin binding left the compatibility observer active")
		}

		measurements, err := statuspkg.PeerMeasurementsToProto(status.Peers)
		if err != nil {
			t.Fatal(err)
		}

		if _, conflict, err := cache.ApplyParsedDelta("node", revision, parsedDelta{peerMeasurements: measurements}, "ws"); err != nil || conflict {
			t.Fatalf("bridge transition lost its valid TTL base: conflict=%v error=%v", conflict, err)
		}

		snapshot, ok := manager.cache.Get("node")
		if !ok || len(snapshot.Status.Peers) != 3 {
			t.Fatal("compatibility observer replaced detailed data with thin metadata")
		}

		assertThinStatus(t, cache.entries["node"], 3)
	})
}

func TestNodeCacheRequiresDetailsBeforeManagerStartup(t *testing.T) {
	cache := NewNodeStatusCache()
	status := retentionFixture(3)
	revision := cache.StoreFull("node", status, "ws")
	cache.RequireDetails()
	assertThinStatus(t, cache.entries["node"], 3)

	if _, err := cache.StoreFullChecked("node", status, "ws"); err == nil {
		t.Fatal("startup retained details without a lifecycle")
	}

	if _, conflict, err := cache.ApplyParsedDelta("node", revision, parsedDelta{}, "ws"); err != nil || !conflict {
		t.Fatal("startup accepted a legacy delta without a TTL base")
	}

	if _, err := cache.StoreOverview("node", statuspkg.OverviewFromStatus(&status, time.Now()), "ws"); err != nil {
		t.Fatalf("startup should still accept overview metadata: %v", err)
	}

	assertThinStatus(t, cache.entries["node"], 3)
}
