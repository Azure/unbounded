// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	testingclock "k8s.io/utils/clock/testing"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func newTestNodeDetailCache(t *testing.T) (*nodeDetailCache, *testingclock.FakeClock) {
	t.Helper()

	cache, err := newNodeDetailCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	cache.clock = fakeClock

	return cache, fakeClock
}

func storeTestNodeDetails(t *testing.T, cache *nodeDetailCache, requestID string) nodeDetailSnapshot {
	t.Helper()

	snapshot, err := cache.Store("node", requestID, cache.clock.Now().Add(-time.Hour), &NodeStatusResponse{})
	if err != nil {
		t.Fatal(err)
	}

	return snapshot
}

func assertNodeDetailEntries(t *testing.T, cache *nodeDetailCache, want int) {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()

	if got := len(cache.entries); got != want {
		t.Fatalf("cache owns %d entries, want %d", got, want)
	}
}

func startNodeDetailLoop(t *testing.T, cache *nodeDetailCache) (context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- cache.Run(ctx)
	}()

	t.Cleanup(cancel)
	synctest.Wait()

	return cancel, done
}

func TestNodeDetailCacheValidation(t *testing.T) {
	for _, ttl := range []time.Duration{-time.Second, 0} {
		if cache, err := newNodeDetailCache(ttl); err == nil || cache != nil {
			t.Fatalf("TTL %v: got cache %v, error %v", ttl, cache, err)
		}
	}

	if _, err := newNodeDetailCache(time.Nanosecond); err != nil {
		t.Fatalf("positive TTL rejected: %v", err)
	}

	cache, fakeClock := newTestNodeDetailCache(t)
	initial := storeTestNodeDetails(t, cache, "original")

	fakeClock.Step(time.Second)

	if _, err := cache.Store("", "invalid", fakeClock.Now(), &NodeStatusResponse{}); err == nil {
		t.Fatal("empty node name accepted")
	}

	if _, err := cache.Store("node", "invalid", fakeClock.Now(), nil); err == nil {
		t.Fatal("nil details accepted")
	}

	if got, ok := cache.Get("node"); !ok || got != initial {
		t.Fatal("invalid Store replaced or refreshed existing details")
	}

	if got, ok := cache.Get("missing"); ok || got != (nodeDetailSnapshot{}) {
		t.Fatal("missing entry did not return an empty snapshot")
	}

	assertNodeDetailEntries(t, cache, 1)
}

func TestNodeDetailCacheDeadlineAndReadNonrefresh(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			cache, fakeClock := newTestNodeDetailCache(t)
			initial := storeTestNodeDetails(t, cache, "request")

			if initial.NodeName != "node" || initial.RequestID != "request" ||
				!initial.ReceivedAt.Equal(fakeClock.Now()) ||
				!initial.CollectedAt.Equal(fakeClock.Now().Add(-time.Hour)) ||
				!initial.ExpiresAt.Equal(fakeClock.Now().Add(cache.ttl)) {
				t.Fatalf("incorrect receipt metadata: %+v", initial)
			}

			fakeClock.Step(cache.ttl / 2)

			for range 3 {
				got, ok := cache.Get("node")
				if !ok || got != initial {
					t.Fatal("read changed the snapshot")
				}

				got.RequestID = "local-copy-only"
			}

			fakeClock.Step(cache.ttl/2 + offset)

			got, ok := cache.Get("node")
			if offset < 0 {
				if !ok || got != initial {
					t.Fatal("details expired before the deadline")
				}

				assertNodeDetailEntries(t, cache, 1)
			} else {
				if ok || got != (nodeDetailSnapshot{}) {
					t.Fatal("expired details returned")
				}

				assertNodeDetailEntries(t, cache, 0)
			}
		})
	}
}

func TestNodeDetailCacheReplacement(t *testing.T) {
	cache, fakeClock := newTestNodeDetailCache(t)
	initial := storeTestNodeDetails(t, cache, "first")
	fakeClock.Step(cache.ttl / 2)
	replacement := storeTestNodeDetails(t, cache, "second")

	if replacement.Status == initial.Status || replacement.ExpiresAt != initial.ExpiresAt.Add(cache.ttl/2) {
		t.Fatal("replacement did not renew details and TTL")
	}

	fakeClock.Step(cache.ttl / 2)

	if removed := cache.Expire(); removed != 0 {
		t.Fatalf("old deadline removed %d replacements", removed)
	}

	if got, ok := cache.Get("node"); !ok || got != replacement {
		t.Fatal("replacement missing at the old deadline")
	}

	fakeClock.Step(cache.ttl / 2)

	if removed := cache.Expire(); removed != 1 {
		t.Fatalf("removed %d entries at replacement deadline, want 1", removed)
	}

	if removed := cache.Expire(); removed != 0 {
		t.Fatalf("repeated expiry removed %d entries", removed)
	}

	assertNodeDetailEntries(t, cache, 0)
}

func TestNodeDetailCacheReleasesHeavyReferences(t *testing.T) {
	for _, operation := range []string{"replace", "delete", "clear", "expire", "get-expired"} {
		t.Run(operation, func(t *testing.T) {
			cache, fakeClock := newTestNodeDetailCache(t)
			status := &NodeStatusResponse{
				NodeInfo: NodeInfo{K8sLabels: map[string]string{"label": "original"}},
				Peers:    make([]statusv1alpha1.PeerStatus, 1024),
				RoutingTable: RoutingTableInfo{Routes: []statusv1alpha1.RouteEntry{{
					NextHops: make([]statusv1alpha1.NextHop, 1024),
				}}},
				BpfEntries: make([]BpfEntry, 1024),
			}

			snapshot, err := cache.Store("node", "heavy", fakeClock.Now(), status)
			if err != nil {
				t.Fatal(err)
			}

			if snapshot.Status == status || &snapshot.Status.Peers[0] != &status.Peers[0] ||
				&snapshot.Status.RoutingTable.Routes[0] != &status.RoutingTable.Routes[0] ||
				&snapshot.Status.BpfEntries[0] != &status.BpfEntries[0] {
				t.Fatal("Store must copy the top-level value but share nested details")
			}

			switch operation {
			case "replace":
				replacement := storeTestNodeDetails(t, cache, "light")
				cache.mu.Lock()
				stored := cache.entries["node"]
				cache.mu.Unlock()

				if stored != replacement || stored.Status == snapshot.Status {
					t.Fatal("map still owns the heavy snapshot")
				}
			case "delete":
				cache.Delete("missing")
				assertNodeDetailEntries(t, cache, 1)
				cache.Delete("node")
				cache.Delete("node")
			case "clear":
				if _, err := cache.Store("other", "", fakeClock.Now(), status); err != nil {
					t.Fatal(err)
				}

				cache.Clear()
				cache.Clear()
			case "expire":
				fakeClock.Step(cache.ttl)
				cache.Expire()
			case "get-expired":
				fakeClock.Step(cache.ttl)
				cache.Get("node")
			}

			if operation != "replace" {
				assertNodeDetailEntries(t, cache, 0)
			}

			if status.NodeInfo.K8sLabels["label"] != "original" || len(status.Peers) != 1024 ||
				len(status.RoutingTable.Routes[0].NextHops) != 1024 || len(status.BpfEntries) != 1024 ||
				len(snapshot.Status.Peers) != 1024 || snapshot.Status.NodeInfo.K8sLabels["label"] != "original" {
				t.Fatal("removing ownership mutated a shared payload")
			}
		})
	}
}

func TestNodeDetailCacheRunExpiryAndReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, fakeClock := newTestNodeDetailCache(t)
		cancel, done := startNodeDetailLoop(t, cache)
		storeTestNodeDetails(t, cache, "first")
		synctest.Wait()
		fakeClock.Step(cache.ttl / 2)
		storeTestNodeDetails(t, cache, "replacement")
		synctest.Wait()
		fakeClock.Step(cache.ttl / 2)
		synctest.Wait()

		// Inspect the map, not Get: readers must not be required for cleanup.
		assertNodeDetailEntries(t, cache, 1)
		fakeClock.Step(cache.ttl / 2)
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 0)
		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		if fakeClock.HasWaiters() {
			t.Fatal("expiry loop left an active timer")
		}
	})
}

func TestNodeDetailCacheRunClearCancelRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, fakeClock := newTestNodeDetailCache(t)
		storeTestNodeDetails(t, cache, "before-run")
		cancel, done := startNodeDetailLoop(t, cache)

		if err := cache.Run(t.Context()); err == nil {
			t.Fatal("concurrent expiry loop accepted")
		}

		cache.Clear()
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 0)

		if fakeClock.HasWaiters() {
			t.Fatal("Clear left an active timer")
		}

		storeTestNodeDetails(t, cache, "after-clear")
		synctest.Wait()
		fakeClock.Step(cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 0)
		storeTestNodeDetails(t, cache, "before-cancel")
		synctest.Wait()
		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		assertNodeDetailEntries(t, cache, 0)

		if fakeClock.HasWaiters() {
			t.Fatal("cancellation left an active timer")
		}

		storeTestNodeDetails(t, cache, "restart")
		cancel, done = startNodeDetailLoop(t, cache)
		fakeClock.Step(cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 0)
		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestNodeDetailCacheRunMultipleDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, fakeClock := newTestNodeDetailCache(t)
		storeTestNodeDetails(t, cache, "already-expired")
		fakeClock.Step(cache.ttl)
		cancel, done := startNodeDetailLoop(t, cache)
		assertNodeDetailEntries(t, cache, 0)

		storeTestNodeDetails(t, cache, "earliest")
		synctest.Wait()
		fakeClock.Step(cache.ttl / 2)

		if _, err := cache.Store("later", "", fakeClock.Now(), &NodeStatusResponse{}); err != nil {
			t.Fatal(err)
		}

		synctest.Wait()
		fakeClock.Step(cache.ttl / 2)
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 1)

		cache.mu.Lock()
		_, earliestRetained := cache.entries["node"]
		_, laterRetained := cache.entries["later"]
		cache.mu.Unlock()

		if earliestRetained || !laterRetained {
			t.Fatal("loop did not expire only the earliest deadline")
		}

		fakeClock.Step(cache.ttl / 2)
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 0)
		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestNodeDetailCacheRunAlreadyCanceled(t *testing.T) {
	cache, _ := newTestNodeDetailCache(t)
	storeTestNodeDetails(t, cache, "request")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := cache.Run(ctx); err != nil {
		t.Fatal(err)
	}

	assertNodeDetailEntries(t, cache, 0)
}

func TestNodeDetailCacheConcurrentAccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, fakeClock := newTestNodeDetailCache(t)
		cancel, done := startNodeDetailLoop(t, cache)

		var workers sync.WaitGroup

		for worker := range 8 {
			workers.Go(func() {
				for iteration := range 100 {
					name := fmt.Sprintf("node-%d", iteration%4)
					request := fmt.Sprintf("%d-%d", worker, iteration)

					if _, err := cache.Store(name, request, fakeClock.Now(), &NodeStatusResponse{}); err != nil {
						t.Error(err)

						return
					}

					cache.Get(name)
					fakeClock.Step(time.Second)
					cache.Expire()
					cache.Delete(name)

					if iteration%10 == 0 {
						cache.Clear()
					}
				}
			})
		}

		workers.Wait()
		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}

		assertNodeDetailEntries(t, cache, 0)

		if fakeClock.HasWaiters() {
			t.Fatal("concurrent operations left an active timer")
		}
	})
}

func TestNodeDetailCacheRunRealClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, err := newNodeDetailCache(time.Minute)
		if err != nil {
			t.Fatal(err)
		}

		storeTestNodeDetails(t, cache, "request")
		cancel, done := startNodeDetailLoop(t, cache)

		// synctest advances virtual standard-library time without a real sleep.
		time.Sleep(cache.ttl)
		synctest.Wait()
		assertNodeDetailEntries(t, cache, 0)
		cancel()

		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}
