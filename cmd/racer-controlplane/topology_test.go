// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	pb "github.com/Azure/unbounded/api/racer"
)

// Topology fixtures, graph invariants and snapshot bounds.

func testGeneration(p uint32, n int) *generation {
	names := make([]string, n)
	g := &generation{Format: generationFormat, Universe: "default", Revision: 1, Nodes: map[string]member{}, Volume: &volumeSpec{ID: "cache-uid", Name: "volume", Slots: p, CacheSocket: "/dev/racer/volume/cache", OriginSocket: "/dev/racer/volume/origin", Algorithm: 2, Attempts: 3}}

	for i := range names {
		names[i] = fmt.Sprintf("node-%06d", i)
		g.Nodes[names[i]] = member{ID: identity("node", names[i]), IP: fmt.Sprintf("10.%d.%d.%d", i>>16, (i>>8)&255, i&255), PodUID: "pod-" + names[i]}
	}

	var err error

	g.Owners, err = place(p, names, nil)
	if err != nil {
		panic(err)
	}

	return g
}

func TestPeerTLSIdentityAndEndpoint(t *testing.T) {
	for _, ip := range []string{"10.1.2.3", "2001:db8::1"} {
		t.Run(ip, func(t *testing.T) {
			g := testGeneration(8, 2)
			remote := g.Nodes["node-000001"]
			remote.IP = ip
			g.Nodes["node-000001"] = remote

			index, err := indexGeneration(g)
			if err != nil {
				t.Fatal(err)
			}

			snapshot := index.snapshot(g.Nodes["node-000000"].ID)
			if len(snapshot.Peers) != 1 {
				t.Fatalf("peers: %v", snapshot.Peers)
			}

			peer := snapshot.Peers[0]
			if peer.PodUid != remote.PodUID || peer.HttpAddress != net.JoinHostPort(ip, "9443") {
				t.Fatalf("peer TLS identity/address: %v", peer)
			}

			for _, volume := range snapshot.Volumes {
				if volume.CacheSocket != "/dev/racer/volume/cache" {
					t.Fatalf("client listener: %s", volume.CacheSocket)
				}

				if len(volume.PeerEndpoints.Peers) != 1 || volume.PeerEndpoints.Peers[0].HttpAddress != peer.HttpAddress {
					t.Fatalf("peer endpoint: %v", volume.PeerEndpoints)
				}
			}
		})
	}
}

// Placement diversity, deterministic churn and minimum pair movement.

// Independent ring oracle: every primary's default three attempts must include
// a different physical owner. Three DISTINCT owners cannot be guaranteed for
// nonmultiples of three (e.g. P=8,N=3). Also check adjacent diversity, with the
// unavoidable single seam for odd two-owner rings.
func checkPlacement(t *testing.T, p uint32, names, owners []string) {
	t.Helper()

	counts := map[string]int{}
	repeated := 0

	for i, owner := range owners {
		counts[owner]++
		if len(names) > 1 && owner == owners[(i+1)%len(owners)] {
			repeated++
		}

		if len(names) > 1 && owner == owners[(i+1)%len(owners)] && owner == owners[(i+2)%len(owners)] {
			t.Fatalf("P=%d N=%d primary=%d: all three candidates on %s", p, len(names), i, owner)
		}
	}

	allowed := 0
	if len(names) == 2 && p%2 == 1 {
		allowed = 1
	}

	if len(names) > 1 && repeated != allowed {
		t.Fatalf("P=%d N=%d repeated adjacency %d want %d: %v", p, len(names), repeated, allowed, owners)
	}

	if len(owners) != int(p) || len(counts) != len(names) {
		t.Fatal("missing ownership")
	}

	for _, name := range names {
		if counts[name] < int(p)/len(names) || counts[name] > (int(p)+len(names)-1)/len(names) {
			t.Fatal("unbalanced", counts)
		}
	}
}

func TestB02EveryPrimaryWindow(t *testing.T) {
	for _, p := range []uint32{2, 3, 7, 8, 9, 17, 131072, 131073, defaultSlots} {
		for _, n := range []int{2, 3, 5, 7, 17} {
			if n > int(p) {
				continue
			}

			t.Run(fmt.Sprintf("%d/%d", p, n), func(t *testing.T) {
				names := make([]string, n)
				for i := range names {
					names[i] = fmt.Sprintf("n%03d", i)
				}
				// Historical on-disk layout, independently constructed contiguous blocks.
				var blocks []string

				for i, name := range names {
					q := int(p) / n
					if i < int(p)%n {
						q++
					}

					for j := 0; j < q; j++ {
						blocks = append(blocks, name)
					}
				}

				for _, prior := range [][]string{nil, blocks} {
					owners, err := place(p, names, prior)
					if err != nil {
						t.Fatal(err)
					}

					checkPlacement(t, p, names, owners)
					data, _ := json.Marshal(owners)

					var reload []string
					if err := json.Unmarshal(data, &reload); err != nil {
						t.Fatal(err)
					}

					reversed := append([]string(nil), names...)
					for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
						reversed[i], reversed[j] = reversed[j], reversed[i]
					}

					again, _ := place(p, reversed, reload)
					if !reflect.DeepEqual(owners, again) {
						t.Fatal("restart/list order caused churn")
					}

					retry, _ := place(p, reversed, prior)
					if !reflect.DeepEqual(owners, retry) {
						t.Fatal("retry differs")
					}
				}
			})
		}
	}
}

func TestB02MembershipHistory(t *testing.T) {
	for _, p := range []uint32{8, 17, defaultSlots} {
		var prior []string

		for _, n := range []int{1, 2, 3, 5, 7, 4, 2, 1, 3, 2} {
			names := make([]string, n)
			for i := range names {
				names[i] = fmt.Sprintf("n%d", i)
			}

			owners, err := place(p, names, prior)
			if err != nil {
				t.Fatal(err)
			}

			checkPlacement(t, p, names, owners)

			again, _ := place(p, names, owners)
			if !reflect.DeepEqual(owners, again) {
				t.Fatal("repeated transition churn")
			}

			prior = owners
		}
	}
}

func TestB02ExhaustiveBalancedMigration(t *testing.T) {
	for p := 2; p <= 10; p++ {
		for n := 2; n <= 3 && n <= p; n++ {
			names := []string{"a", "b", "c"}[:n]

			quota := make([]int, n)
			for i := range quota {
				quota[i] = p / n
				if i < p%n {
					quota[i]++
				}
			}

			owners := make([]string, p)

			var visit func(int)

			visit = func(s int) {
				if s == p {
					next, err := place(uint32(p), names, owners)
					if err != nil {
						t.Fatalf("%v: %v", owners, err)
					}

					checkPlacement(t, uint32(p), names, next)

					if n == 2 {
						best := p
						for shift := 0; shift < p; shift++ {
							moved := 0

							for i := range owners {
								if owners[i] != names[((i+shift)%p)%2] {
									moved++
								}
							}

							best = min(best, moved)
						}

						moved := 0

						for i := range owners {
							if owners[i] != next[i] {
								moved++
							}
						}

						if moved != best {
							t.Fatalf("not minimum pair movement: %v -> %v", owners, next)
						}
					}

					return
				}

				for i := range names {
					if quota[i] > 0 {
						quota[i]--
						owners[s] = names[i]
						visit(s + 1)

						quota[i]++
					}
				}
			}
			visit(0)
		}
	}
}

func TestB02SeededChurnReplay(t *testing.T) {
	for _, p := range []uint32{8, 31, 1024, defaultSlots} {
		run := func() []string {
			rng := rand.New(rand.NewSource(502))

			var prior []string

			for step := 0; step < 100; step++ {
				n := 1 + rng.Intn(min(int(p), 19))
				permutation := rng.Perm(32)

				names := make([]string, n)
				for i := range names {
					names[i] = fmt.Sprintf("n%02d", permutation[i])
				}

				owners, err := place(p, names, prior)
				if err != nil {
					t.Fatal(err)
				}

				checkPlacement(t, p, names, owners)

				stable, _ := place(p, names, owners)
				if !reflect.DeepEqual(stable, owners) {
					t.Fatal("unchanged membership churn")
				}

				prior = owners
			}

			return prior
		}
		if !reflect.DeepEqual(run(), run()) {
			t.Fatal("seeded history replay diverged")
		}
	}
}

// Persisted layout migration and configuration admission.

func TestB02PersistedPlacementRebalancesOnce(t *testing.T) {
	for _, p := range []uint32{262144} {
		t.Run(fmt.Sprint(p), func(t *testing.T) {
			ctx := context.Background()
			n, a, s := fixtures()
			m, b := n.DeepCopy(), a.DeepCopy()
			m.Name = "other"
			m.UID = "other-uid"
			b.Name = "other"
			b.UID = "other-pod"
			b.Spec.NodeName = m.Name
			b.Status.PodIP = "10.1.1.2"

			g, _, err := buildCacheFixture("default", nil, []corev1.Node{*n, *m}, []corev1.Pod{*a, *b}, s)
			if err != nil {
				t.Fatal(err)
			}

			g.Revision = 41
			for i := range g.Owners {
				if i < int(p)/2 {
					g.Owners[i] = n.Name
				} else {
					g.Owners[i] = m.Name
				}
			}

			old := append([]string(nil), g.Owners...)
			c := fakeKube(n, m, a, b, s)

			r := newTestReconciler(c)
			if err := r.store.commit(ctx, g, nil); err != nil {
				t.Fatal(err)
			}

			index, err := indexGeneration(g)
			if err != nil {
				t.Fatal(err)
			}

			if err := r.server.install(index); err != nil {
				t.Fatal(err)
			}

			rollout, err := r.server.rolloutFor(ctx, index)
			if err != nil {
				t.Fatal(err)
			}

			if err := r.server.persistPhase(ctx, "default", rollout, 4); err != nil {
				t.Fatal(err)
			}

			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "default"}}

			for retry := 0; retry < 4; retry++ {
				if retry%2 == 0 {
					r = newTestReconciler(c)
				}

				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}

				next := r.loaded["default"]
				if next.Revision != 42 {
					t.Fatalf("migration/restart revision %d", next.Revision)
				}

				checkPlacement(t, p, []string{n.Name, m.Name}, next.Owners)

				moved := 0

				for i := range old {
					if old[i] != next.Owners[i] {
						moved++
					}
				}

				if moved != int(p)/2 {
					t.Fatalf("migration not minimum: %d", moved)
				}

				idx, _ := indexGeneration(next)
				for _, node := range next.Nodes {
					snap := idx.snapshot(node.ID)
					if snap.Revision != 42 || snap.Volumes[0].Topology.Epoch != 42 {
						t.Fatal("epoch did not fence migration")
					}
				}

				reload, _, err := r.store.load(ctx, "default")
				if err != nil || !reflect.DeepEqual(next, reload) {
					t.Fatal("durable migration differs", err)
				}
			}
		})
	}
}

func TestB02CapacityAndAdmission(t *testing.T) {
	for _, tc := range []struct {
		p     uint32
		names []string
	}{{0, []string{"a"}}, {262145, []string{"a"}}, {1, []string{"a", "b"}}, {8, []string{"a", "a"}}, {8, []string{""}}, {8, nil}} {
		if _, err := place(tc.p, tc.names, nil); err == nil {
			t.Fatalf("invalid capacity accepted: %+v", tc)
		}
	}

	for _, n := range []int{1, 2} {
		g := testGeneration(defaultSlots, n)
		for i := 0; i < 4; i++ {
			v := *g.Volume
			v.ID = fmt.Sprintf("v%d", i)
			v.CacheSocket = fmt.Sprintf("/dev/racer/v%d/cache", i)
			v.OriginSocket = fmt.Sprintf("/dev/racer/v%d/origin", i)
			g.Additional = append(g.Additional, volumeState{&v, g.Owners})
		}

		idx, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		if err := idx.admit(); (err != nil) != (n == 1) {
			t.Fatalf("five default volumes N=%d: %v", n, err)
		}
	}
	// Wire bound still rejects the audited dispersed 32-volume/1000-node case.
	g := testGeneration(defaultSlots, 1000)
	for i := 0; i < 31; i++ {
		v := *g.Volume
		v.ID = fmt.Sprintf("v%d", i)
		g.Additional = append(g.Additional, volumeState{&v, g.Owners})
	}

	idx, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	if idx.admit() == nil {
		t.Fatal("conservative wire admission bypassed")
	}
}

// Cross-language conformance snapshots and optional export.

// Optional export is consumed byte-for-byte by the Rust conformance/live probe.
func TestB02ProductionSnapshots(t *testing.T) {
	dir := os.Getenv("B02_EXPORT")

	var files []string

	for _, p := range []uint32{8, defaultSlots, 131073} {
		for _, n := range []int{2, 3, 7, 1000} {
			if n > int(p) {
				continue
			}

			g := testGeneration(p, n)
			g.Volume.OriginSocket = "/dev/racer/volume/origin"

			for name, node := range g.Nodes {
				node.IP = "127.0.0.2"
				if name != "node-000000" {
					node.IP = "127.0.0.3"
				}

				g.Nodes[name] = node
			}

			fresh := append([]string(nil), g.Owners...)

			for _, layout := range []string{"fresh", "historical", "migrated"} {
				if layout == "historical" {
					g.Owners = nil

					for i := 0; i < n; i++ {
						q := int(p) / n
						if i < int(p)%n {
							q++
						}

						for j := 0; j < q; j++ {
							g.Owners = append(g.Owners, fmt.Sprintf("node-%06d", i))
						}
					}
				}

				if layout == "migrated" {
					names := make([]string, n)
					for i := range names {
						names[i] = fmt.Sprintf("node-%06d", i)
					}

					var err error

					g.Owners, err = place(p, names, g.Owners)
					if err != nil {
						t.Fatal(err)
					}
				}

				start := time.Now()

				if dir != "" {
					data, _ := json.Marshal(g.Owners)

					file := fmt.Sprintf("p%d-n%d-%s-owners.json", p, n, layout)
					if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
						t.Fatal(err)
					}
				}

				idx, err := indexGeneration(g)
				if err != nil {
					t.Fatal(err)
				}

				if err = idx.admit(); err != nil {
					t.Fatal(err)
				}

				for _, i := range []int{0, n - 1} {
					name := fmt.Sprintf("node-%06d", i)
					snap := idx.snapshot(g.Nodes[name].ID)

					if root := os.Getenv("B02_SOCKET_ROOT"); root != "" {
						snap.Volumes[0].CacheSocket = filepath.Join(root, fmt.Sprintf("cache-%d", i))
						snap.Volumes[0].OriginSocket = filepath.Join(root, fmt.Sprintf("origin-%d", i))
					}

					wire, err := marshalSnapshot(snap)
					if err != nil {
						t.Fatal(err)
					}

					data, err := protojson.Marshal(&pb.Configuration{Contents: &pb.Configuration_Snapshot{Snapshot: snap}})
					if err != nil {
						t.Fatal(err)
					}

					top := snap.Volumes[0].Topology
					if len(top.Neighbors) > int(p)-len(top.LocalSlots) || len(snap.Peers) > min(n-1, 2*len(top.LocalSlots)*int(degree(p))) {
						t.Fatal("sparse/endpoint bound exceeded")
					}

					t.Logf("P=%d N=%d %s node=%d local=%d sparse=%d endpoints=%d work=%d binary=%d json=%d elapsed=%s", p, n, layout, i, len(top.LocalSlots), len(top.Neighbors), len(snap.Peers), 64*len(top.LocalSlots), len(wire), len(data), time.Since(start))

					if dir != "" {
						file := fmt.Sprintf("p%d-n%d-%s-%d.json", p, n, layout, i)

						files = append(files, file)
						if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
							t.Fatal(err)
						}

						if p == defaultSlots && n == 2 && layout == "fresh" {
							label := "a"
							if i != 0 {
								label = "b"
							}

							if err := os.WriteFile(filepath.Join(dir, label+".json"), data, 0o644); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}

			if dir != "" && p == defaultSlots && n == 2 {
				data, _ := json.Marshal(map[string]any{"universe": identity("universe", g.Universe), "nodes": map[string]member{"a": g.Nodes["node-000000"], "b": g.Nodes["node-000001"]}, "owners": fresh})
				if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	if dir != "" {
		data, _ := json.Marshal(files)
		if err := os.WriteFile(filepath.Join(dir, "files.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Directed graph equivalence, sparse representations and topology benchmarks.

func TestTopologyMatchesDirectedGraph(t *testing.T) {
	for p := uint32(1); p <= 128; p++ {
		g := testGeneration(p, int(p))

		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		d := degree(p)

		for slot, name := range g.Owners {
			neighbors, _, direct := index.connections(name)

			actual := map[uint32]string{}
			for _, n := range neighbors {
				actual[n.Slot] = n.Peer
			}

			wantDirect := map[string]bool{}

			for source := uint32(0); source < p; source++ {
				for digit := uint32(0); digit < d; digit++ {
					next := (source*d + digit) % p
					if source == uint32(slot) && next != source {
						if actual[next] != g.Nodes[g.Owners[next]].ID {
							t.Fatalf("P=%d missing edge %d->%d", p, source, next)
						}

						wantDirect[g.Owners[next]] = true
					}

					if next == uint32(slot) && next != source {
						wantDirect[g.Owners[source]] = true
					}
				}
			}

			if !reflect.DeepEqual(direct, wantDirect) {
				t.Fatalf("P=%d slot=%d reverse edges mismatch", p, slot)
			}
			// Independent breadth-first traversal verifies the three-edge bound.
			seen := map[uint32]bool{uint32(slot): true}
			frontier := []uint32{uint32(slot)}

			for depth := 0; depth < 3; depth++ {
				var next []uint32

				for _, s := range frontier {
					for a := uint32(0); a < d; a++ {
						v := (s*d + a) % p
						if !seen[v] {
							seen[v] = true
							next = append(next, v)
						}
					}
				}

				frontier = next
			}

			if len(seen) != int(p) {
				t.Fatalf("P=%d source=%d exceeds three edges", p, slot)
			}
		}
	}
}

func TestColocatedSlotsAndSparseSnapshots(t *testing.T) {
	g := testGeneration(64, 16)

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	for name, node := range g.Nodes {
		s := index.snapshot(node.ID)

		v := s.Volumes[0]
		if len(v.Topology.LocalSlots) != 4 {
			t.Fatal("unbalanced placement")
		}

		for _, edge := range v.Topology.Neighbors {
			if g.Owners[edge.Slot] == name {
				t.Fatal("local edge published as network hop")
			}
		}

		peers := map[string]bool{}
		for _, peer := range s.Peers {
			peers[peer.Id] = true
			if peer.Id == node.ID {
				t.Fatal("self peer")
			}
		}

		for _, peer := range v.Peers {
			if !peers[peer] {
				t.Fatal("outgoing peer absent from snapshot")
			}
		}

		if len(s.Peers) >= len(g.Nodes) {
			t.Fatal("full mesh")
		}
	}
}

func TestPlacementRetainsBalancedOwnership(t *testing.T) {
	names := []string{"c", "a", "b", "d"}

	old, err := place(64, names, nil)
	if err != nil {
		t.Fatal(err)
	}

	same, _ := place(64, []string{"d", "b", "c", "a"}, old)
	if !reflect.DeepEqual(old, same) {
		t.Fatal("list ordering changes placement")
	}

	joined, _ := place(64, append(names, "e"), old)
	moved := 0

	counts := map[string]int{}
	for slot, owner := range joined {
		counts[owner]++
		if owner != old[slot] {
			moved++
		}
	}

	if moved != 12 || counts["e"] != 12 {
		t.Fatalf("moved %d slots, counts %v", moved, counts)
	}

	removed, _ := place(64, []string{"a", "b", "c", "d"}, joined)
	for slot, owner := range joined {
		if owner != "e" && removed[slot] != owner {
			t.Fatal("unnecessary movement on removal")
		}
	}

	for _, tc := range []struct {
		p uint32
		n int
	}{{0, 1}, {1, 2}, {262145, 1}} {
		names := make([]string, tc.n)
		if _, err := place(tc.p, names, nil); err == nil {
			t.Fatalf("accepted invalid capacity %+v", tc)
		}
	}
}

func TestLargeGeometryCompatibility(t *testing.T) {
	// Exactly one slot per node fits today's dataplane at the requested scale.
	g := testGeneration(100000, 100000)

	index, err := indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"node-000000", "node-050000", "node-099999"} {
		s := index.snapshot(g.Nodes[name].ID)
		if len(s.Volumes[0].Peers) > 64 {
			t.Fatal("peer bound exceeded")
		}

		wire, err := marshalSnapshot(s)
		if err != nil || len(wire) > 4*1024*1024 {
			t.Fatal("snapshot exceeds dataplane receive limit")
		}
	}

	g = testGeneration(262144, 100000)

	index, err = indexGeneration(g)
	if err != nil {
		t.Fatal(err)
	}
	// A three-slot owner routes into the two-slot ownership region.
	if len(index.snapshot(g.Nodes["node-001000"].ID).Volumes[0].Peers) <= 64 {
		t.Fatal("large topology fixture must exercise more than 64 peers")
	}
}

func BenchmarkTopology100K(b *testing.B) {
	g := testGeneration(262144, 100000)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if _, err := indexGeneration(g); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSnapshot100K(b *testing.B) {
	g := testGeneration(262144, 100000)

	index, err := indexGeneration(g)
	if err != nil {
		b.Fatal(err)
	}

	id := g.Nodes["node-001000"].ID

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if _, err := marshalSnapshot(index.snapshot(id)); err != nil {
			b.Fatal(err)
		}
	}
}

func TestFixedMaximumGeometryLifecycle(t *testing.T) {
	var previous []string

	for _, n := range []int{1, 2, 17, 4096, 100000, 4913, 3, 1} {
		g := testGeneration(262144, n)

		names := make([]string, 0, n)
		for name := range g.Nodes {
			names = append(names, name)
		}

		var err error

		g.Owners, err = place(262144, names, previous)
		if err != nil {
			t.Fatal(err)
		}

		index, err := indexGeneration(g)
		if err != nil {
			t.Fatal(err)
		}

		if err = index.admit(); err != nil {
			t.Fatalf("participants=%d: %v", n, err)
		}

		for name, slots := range index.local {
			if len(slots) < 262144/n || len(slots) > (262144+n-1)/n {
				t.Fatalf("unbalanced %s: %d", name, len(slots))
			}
		}

		s := index.snapshot(g.Nodes["node-000000"].ID)

		wire, err := marshalSnapshot(s)
		if err != nil || len(wire) > 64*1024*1024 {
			t.Fatal("wire budget", err)
		}

		t.Logf("participants=%d local=%d successors=%d peers=%d binary=%d", n,
			len(s.Volumes[0].Topology.LocalSlots), len(s.Volumes[0].Topology.Neighbors),
			len(s.Peers), len(wire))

		previous = g.Owners
	}
}
