// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

func TestPeerIdentityHashMatchesProtocol(t *testing.T) {
	for _, count := range []int{0, 1, 2000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			peers := protoToNodeStatus(measurementTestStatus(count)).Peers
			if count > 0 {
				peers[0].Name = "peer\x00雪"
				peers[0].Tunnel.Protocol = ""
				peers[0].Tunnel.Interface = "ab"
				peers[0].Tunnel.PublicKey = "c"
			}

			digest, err := netstatus.PeerIdentityDigest(peers)
			if err != nil {
				t.Fatal(err)
			}

			got := hashPeerIdentities(peers)
			if !bytes.Equal(got[:], digest) {
				t.Fatal("cached identity encoding differs from the wire protocol")
			}

			if count > 0 {
				peers[0].Tunnel.Interface = "a"
				peers[0].Tunnel.PublicKey = "bc"

				if hashPeerIdentities(peers) == got {
					t.Fatal("identity boundaries are ambiguous")
				}
			}
		})
	}
}

func warmPeerIdentityCache(t *testing.T, cache *NodeStatusCache) *CachedNodeStatus {
	t.Helper()

	entry, ok := cache.Get("node-a")
	if !ok {
		t.Fatal("missing base")
	}

	message := measurementMessage(t, *entry.Status, entry.Revision)

	ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, message)
	if ack.Status != "ok" {
		t.Fatalf("compact update: %+v", ack)
	}

	result, ok := cache.Get("node-a")
	if !ok || result.peerIdentity == nil {
		t.Fatal("successful compact update did not memoize identity validation")
	}

	return result
}

func TestPeerIdentityCacheReuse(t *testing.T) {
	status := protoToNodeStatus(measurementTestStatus(2))
	cache := NewNodeStatusCache()
	cache.StoreFull("node-a", status, "ws")
	first := warmPeerIdentityCache(t, cache)

	before, err := json.Marshal(first.Status)
	if err != nil {
		t.Fatal(err)
	}

	// The compact copy owns its peer value fields, independently of StoreFull's
	// original caller. It still shares immutable static maps and slices.
	status.Peers[0].Name = "changed original input"

	for range 3 {
		next := warmPeerIdentityCache(t, cache)
		if next.peerIdentity != first.peerIdentity {
			t.Fatal("unchanged identities rebuilt their validation memo")
		}
	}

	_, conflict, err := cache.ApplyDelta("node-a", 0, map[string]json.RawMessage{"fetchError": []byte(`"unavailable"`)}, "push")
	if err != nil || conflict {
		t.Fatalf("non-peer delta: %t %v", conflict, err)
	}

	cache.UpdateSource("node-a", "ws")

	next := warmPeerIdentityCache(t, cache)
	if next.peerIdentity != first.peerIdentity {
		t.Fatal("non-peer update invalidated identity validation")
	}

	after, err := json.Marshal(first.Status)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("later updates mutated a retained snapshot")
	}

	allocations := testing.AllocsPerRun(10, func() {
		identity, err := validatePeerIdentity(next.Status.Peers, next.peerIdentity)
		if err != nil || identity != next.peerIdentity {
			t.Fatalf("reuse: %v", err)
		}
	})
	if allocations != 0 {
		t.Fatalf("identity reuse allocated %g objects", allocations)
	}
}

func TestPeerIdentityCacheInvalidatesReplacementPaths(t *testing.T) {
	changes := map[string]func(*statusproto.NodeStatusFull){
		"identical": func(_ *statusproto.NodeStatusFull) {},
		"metadata":  func(s *statusproto.NodeStatusFull) { s.Peers[0].SiteName = "another-site" },
		"renamed":   func(s *statusproto.NodeStatusFull) { s.Peers[0].Name = "another-peer" },
		"added":     func(s *statusproto.NodeStatusFull) { s.Peers = measurementTestStatus(3).Peers },
		"removed":   func(s *statusproto.NodeStatusFull) { s.Peers = s.Peers[:1] },
		"reordered": func(s *statusproto.NodeStatusFull) {
			s.Peers[0], s.Peers[1] = s.Peers[1], s.Peers[0]
		},
		"duplicate": func(s *statusproto.NodeStatusFull) { s.Peers[1] = s.Peers[0] },
		"unnamed":   func(s *statusproto.NodeStatusFull) { s.Peers[0].Name = "" },
		"nil":       func(s *statusproto.NodeStatusFull) { s.Peers = nil },
		"empty":     func(s *statusproto.NodeStatusFull) { s.Peers = []*statusproto.PeerStatus{} },
	}
	for _, path := range []string{"StoreFull", "protobuf full", "protobuf delta", "JSON delta"} {
		for name, change := range changes {
			t.Run(path+"/"+name, func(t *testing.T) {
				cache := NewNodeStatusCache()
				cache.StoreFull("node-a", protoToNodeStatus(measurementTestStatus(2)), "ws")
				first := warmPeerIdentityCache(t, cache)
				full := measurementTestStatus(2)
				change(full)

				status := protoToNodeStatus(full)
				if full.Peers != nil && len(full.Peers) == 0 {
					status.Peers = []WireGuardPeerStatus{}
				}

				var conflict bool

				var err error

				switch path {
				case "StoreFull":
					cache.StoreFull("node-a", status, "push")
				case "protobuf full":
					ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, &statusproto.NodeStatusMessage{
						Type: "node_status_full", NodeName: "node-a", Status: full,
					})
					if ack.Status != "ok" {
						t.Fatal(ack)
					}
				case "protobuf delta":
					delta := protoToParsedDelta(&statusproto.NodeStatusDelta{UpdatedFields: []string{"peers"}, Peers: full.Peers})
					_, conflict, err = cache.ApplyParsedDelta("node-a", first.Revision, delta, "ws")
				case "JSON delta":
					var raw []byte

					raw, err = json.Marshal(status.Peers)
					if err == nil {
						_, conflict, err = cache.ApplyDelta("node-a", first.Revision, map[string]json.RawMessage{"peers": raw}, "push")
					}
				}

				if err != nil || conflict {
					t.Fatalf("replacement changed legacy behavior: %t %v", conflict, err)
				}

				replaced := cache.entries["node-a"]
				if replaced.peerIdentity != nil {
					t.Fatal("peer replacement retained the old identity memo")
				}

				if name == "duplicate" || name == "unnamed" {
					message := measurementMessage(t, *first.Status, replaced.Revision)

					ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, message)
					if ack.Status != "resync_required" || cache.entries["node-a"] != replaced {
						t.Fatal("invalid replacement accepted compact measurements or changed the cache")
					}

					return
				}

				if name != "identical" && name != "metadata" {
					message := measurementMessage(t, *first.Status, replaced.Revision)

					ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, message)
					if ack.Status != "resync_required" || cache.entries["node-a"] != replaced {
						t.Fatal("replacement accepted old-topology measurements")
					}
				}

				next := warmPeerIdentityCache(t, cache)
				if next.peerIdentity == first.peerIdentity {
					t.Fatal("replacement did not independently validate its identities")
				}
			})
		}
	}
}

func TestPeerIdentityCacheDetectsBorrowedIdentityChanges(t *testing.T) {
	changes := map[string]func([]WireGuardPeerStatus){
		"name":      func(p []WireGuardPeerStatus) { p[0].Name = "another-peer" },
		"protocol":  func(p []WireGuardPeerStatus) { p[0].Tunnel.Protocol = "another-protocol" },
		"interface": func(p []WireGuardPeerStatus) { p[0].Tunnel.Interface = "another-interface" },
		"key":       func(p []WireGuardPeerStatus) { p[0].Tunnel.PublicKey = "another-key" },
		"reorder":   func(p []WireGuardPeerStatus) { p[0], p[1] = p[1], p[0] },
		"duplicate": func(p []WireGuardPeerStatus) { p[1] = p[0] },
		"unnamed":   func(p []WireGuardPeerStatus) { p[0].Name = "" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cache := NewNodeStatusCache()
			cache.StoreFull("node-a", protoToNodeStatus(measurementTestStatus(2)), "ws")
			first := warmPeerIdentityCache(t, cache)
			message := measurementMessage(t, *first.Status, first.Revision)
			digest := *first.peerIdentity
			change(first.Status.Peers)

			before, err := json.Marshal(first.Status)
			if err != nil {
				t.Fatal(err)
			}

			ack := applyMeasurementMessage(t, &healthState{statusCache: cache}, message)

			after, marshalErr := json.Marshal(cache.entries["node-a"].Status)
			if ack.Status != "resync_required" || marshalErr != nil || !bytes.Equal(before, after) {
				t.Fatal("borrowed identity mutation used a stale memo or partially updated the cache")
			}

			if *first.peerIdentity != digest || cache.entries["node-a"].Revision != first.Revision {
				t.Fatal("rejected update mutated the old memo or revision")
			}

			if name != "duplicate" && name != "unnamed" {
				next := warmPeerIdentityCache(t, cache)
				if next.peerIdentity == first.peerIdentity {
					t.Fatal("changed borrowed identity did not force revalidation")
				}
			}
		})
	}
}

func TestPeerMeasurementsCachedValidationMatchesDirect(t *testing.T) {
	changes := map[string]func([]WireGuardPeerStatus, *statusproto.PeerMeasurements){
		"valid":     func(_ []WireGuardPeerStatus, _ *statusproto.PeerMeasurements) {},
		"count":     func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.PeerCount++ },
		"rx":        func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.RxBytes = nil },
		"tx":        func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.TxBytes = nil },
		"handshake": func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.LastHandshakeUnixNs = nil },
		"uptime":    func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.Uptime = nil },
		"rtt":       func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.Rtt = nil },
		"digest":    func(_ []WireGuardPeerStatus, m *statusproto.PeerMeasurements) { m.IdentityDigest[0] ^= 1 },
		"duplicate": func(p []WireGuardPeerStatus, _ *statusproto.PeerMeasurements) { p[1] = p[0] },
		"unnamed":   func(p []WireGuardPeerStatus, _ *statusproto.PeerMeasurements) { p[0].Name = "" },
		"health":    func(p []WireGuardPeerStatus, _ *statusproto.PeerMeasurements) { p[0].HealthCheck = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			status := protoToNodeStatus(measurementTestStatus(2))
			message := measurementMessage(t, status, 1)
			m := message.Delta.PeerMeasurements

			identity, err := validatePeerIdentity(status.Peers, nil)
			if err != nil {
				t.Fatal(err)
			}

			change(status.Peers, m)
			direct, directErr := applyPeerMeasurements(status.Peers, m)

			cached, _, cachedErr := applyPeerMeasurementsWithIdentity(status.Peers, m, identity)
			if fmt.Sprint(directErr) != fmt.Sprint(cachedErr) || !reflect.DeepEqual(direct, cached) {
				t.Fatalf("direct and memoized validation disagree: %v / %v", directErr, cachedErr)
			}

			if name != "valid" && directErr == nil {
				t.Fatal("invalid measurements were accepted")
			}
		})
	}
}

func TestPeerIdentityCacheRevisionAndRemoval(t *testing.T) {
	cache := NewNodeStatusCache()
	cache.StoreFull("node-a", protoToNodeStatus(measurementTestStatus(2)), "ws")
	first := warmPeerIdentityCache(t, cache)
	fetchError := "must not apply"
	delta := parsedDelta{peerMeasurements: &statusproto.PeerMeasurements{}, fetchError: &fetchError}

	for _, revision := range []uint64{0, first.Revision - 1, first.Revision + 1} {
		_, conflict, err := cache.ApplyParsedDelta("node-a", revision, delta, "ws")
		if !conflict || err != nil {
			t.Fatalf("revision %d did not precede malformed identity/column validation: %t %v", revision, conflict, err)
		}
	}

	if current := cache.entries["node-a"]; current.Revision != first.Revision || current.peerIdentity != first.peerIdentity || current.Status.FetchError != "" {
		t.Fatal("revision conflict changed the cache")
	}

	for _, remove := range []func(){func() { cache.Delete("node-a") }, func() { cache.CleanupStaleEntries(nil) }} {
		remove()

		_, conflict, err := cache.ApplyParsedDelta("node-a", first.Revision, delta, "ws")
		if !conflict || err != nil || cache.Len() != 0 {
			t.Fatal("missing entry did not request resync before validation")
		}

		cache.StoreFull("node-a", protoToNodeStatus(measurementTestStatus(2)), "push")

		if cache.entries["node-a"].peerIdentity != nil {
			t.Fatal("recreated cache entry inherited an identity memo")
		}

		warmPeerIdentityCache(t, cache)
	}
}

func TestPeerIdentityCacheConcurrentUpdates(t *testing.T) {
	cache := NewNodeStatusCache()
	cache.StoreFull("node-a", protoToNodeStatus(measurementTestStatus(2000)), "ws")
	first := warmPeerIdentityCache(t, cache)
	delta := protoToParsedDelta(measurementMessage(t, *first.Status, first.Revision).Delta)
	start := make(chan struct{})
	results := make(chan bool, 8)

	var workers sync.WaitGroup

	for range cap(results) {
		workers.Go(func() {
			<-start

			_, conflict, err := cache.ApplyParsedDelta("node-a", first.Revision, delta, "ws")
			if err != nil {
				t.Errorf("concurrent update: %v", err)
			}

			results <- !conflict && err == nil
		})
	}

	close(start)
	workers.Wait()
	close(results)

	applied := 0

	for success := range results {
		if success {
			applied++
		}
	}

	if current := cache.entries["node-a"]; applied != 1 || current.Revision != first.Revision+1 || current.peerIdentity != first.peerIdentity {
		t.Fatalf("concurrent update lost revision or identity invariants: successes=%d", applied)
	}
}

func TestPeerIdentityCacheRejectsCommitAfterRecreation(t *testing.T) {
	cache := NewNodeStatusCache()
	cache.StoreFull("node-a", protoToNodeStatus(measurementTestStatus(2)), "ws")
	warmPeerIdentityCache(t, cache)
	previous := cache.entries["node-a"]
	merged := *previous.Status
	merged.FetchError = "must not apply"

	cache.Delete("node-a")

	replacement := protoToNodeStatus(measurementTestStatus(1))
	for range previous.Revision {
		cache.StoreFull("node-a", replacement, "push")
	}

	current := cache.entries["node-a"]
	if current.Revision != previous.Revision {
		t.Fatal("test requires a recreated entry with the same revision")
	}

	called := false

	cache.SetOnChange(func(_ string, _ *NodeStatusResponse) { called = true })

	revision, conflict, err := cache.commitParsedDelta("node-a", previous, &merged, previous.peerIdentity, "ws")
	if err != nil || !conflict || revision != current.Revision || called {
		t.Fatalf("stale commit after recreation: revision=%d conflict=%t err=%v callback=%t", revision, conflict, err, called)
	}

	if cache.entries["node-a"] != current || current.peerIdentity != nil {
		t.Fatal("stale commit overwrote the recreated entry or restored its old memo")
	}
}

func TestPeerIdentityValidationRejectsDuplicatesAfterMemo(t *testing.T) {
	peers := protoToNodeStatus(measurementTestStatus(2)).Peers

	identity, err := validatePeerIdentity(peers, nil)
	if err != nil {
		t.Fatal(err)
	}

	peers[1] = peers[0]

	_, err = validatePeerIdentity(peers, identity)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("memo skipped duplicate validation: %v", err)
	}
}

func BenchmarkCompactIdentityCache2000(b *testing.B) {
	status := protoToNodeStatus(measurementTestStatus(2000))

	measurements, err := netstatus.PeerMeasurementsToProto(status.Peers)
	if err != nil {
		b.Fatal(err)
	}

	cache := NewNodeStatusCache()
	revision := cache.StoreFull("node-a", status, "ws")
	delta := parsedDelta{peerMeasurements: measurements}

	revision, conflict, err := cache.ApplyParsedDelta("node-a", revision, delta, "ws")
	if err != nil || conflict {
		b.Fatalf("warm cache: conflict=%t err=%v", conflict, err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		next, conflict, err := cache.ApplyParsedDelta("node-a", revision, delta, "ws")
		if err != nil || conflict {
			b.Fatalf("apply: conflict=%t err=%v", conflict, err)
		}

		revision = next
	}
}
