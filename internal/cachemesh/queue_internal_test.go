// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cachemesh

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// TestCoalesceNeighbors checks that merged neighbor events leave a watcher
// with the same set as delivering every event, that removals always name a
// peer the watcher has, and that the queue stays bounded.
func TestCoalesceNeighbors(t *testing.T) {
	for seed := range uint64(200) {
		r := rand.New(rand.NewPCG(seed, 1))
		truth := map[uint32]PeerRef{} // slot -> current peer
		seen := map[uint32]PeerRef{}  // the watcher's set

		deliver := func(q []event) {
			for _, ev := range q {
				if ev.kind != evNeighbors {
					continue
				}

				for _, p := range ev.removed {
					if seen[uint32(p.Slot)] != p {
						t.Fatalf("seed %d: removed unknown %v", seed, p)
					}

					delete(seen, uint32(p.Slot))
				}

				for _, p := range ev.added {
					if _, ok := seen[uint32(p.Slot)]; ok {
						t.Fatalf("seed %d: added twice %v", seed, p)
					}

					seen[uint32(p.Slot)] = p
				}
			}
		}

		var q []event

		for step := range 2000 {
			switch k := r.IntN(20); {
			case k == 0:
				q = coalesce(q, event{kind: evLeadership})
			case k < 4:
				q = coalesce(q, event{kind: evView, version: Version{}})
			default:
				var ev event

				used := map[uint32]bool{} // one change per slot per event, as onNeighbors

				for range 1 + r.IntN(3) {
					s := uint32(r.IntN(8))
					if used[s] {
						continue
					}

					used[s] = true
					if old, ok := truth[s]; ok && r.IntN(2) == 0 {
						delete(truth, s)

						ev.removed = append(ev.removed, old)

						continue
					}

					p := PeerRef{Slot: Slot(s), ID: fmt.Sprintf("m%d-%d", s, r.IntN(2)), Addr: "a"}
					if old, ok := truth[s]; ok {
						if old == p {
							continue
						}

						ev.removed = append(ev.removed, old)
					}

					truth[s] = p
					ev.added = append(ev.added, p)
				}

				q = coalesce(q, ev)
			}

			lead := 0

			for _, ev := range q {
				if ev.kind == evLeadership {
					lead++
				}
			}

			if len(q) > 2*(lead+1)+lead {
				t.Fatalf("seed %d: queue grew to %d with %d leadership events", seed, len(q), lead)
			}

			if r.IntN(50) == 0 || step == 1999 {
				deliver(q)
				q = nil
			}
		}

		if len(seen) != len(truth) {
			t.Fatalf("seed %d: watcher has %d peers, want %d", seed, len(seen), len(truth))
		}

		for s, p := range truth {
			if seen[s] != p {
				t.Fatalf("seed %d: slot %d: watcher has %v, want %v", seed, s, seen[s], p)
			}
		}
	}
}
