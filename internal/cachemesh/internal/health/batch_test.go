// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"
)

type peerSnap struct {
	slot uint32
	key  uint64
	cell uint32
}

func snap(t *Tracker) []peerSnap {
	var out []peerSnap

	for _, p := range t.tab.Load().peers {
		out = append(out, peerSnap{p.slot, p.key, p.cc.Load().cell})
	}

	return out
}

func cellSnap(t *Tracker) []uint32 {
	var out []uint32

	for _, c := range t.tab.Load().cells {
		out = append(out, c.cell, uint32(c.n))
	}

	return out
}

// TestTrackManySyncMatchTrack checks that the batch APIs produce the same
// table as the equivalent sequence of Track and Untrack calls.
func TestTrackManySyncMatchTrack(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))

	for iter := range 300 {
		a, b := newT(Config{}), newT(Config{})

		for range 1 + rng.IntN(4) {
			refs := make([]Ref, rng.IntN(40))
			for i := range refs {
				refs[i] = Ref{Slot: rng.Uint32N(50), Key: uint64(rng.IntN(3)), Cell: rng.Uint32N(5)}
			}

			if rng.IntN(2) == 0 {
				for _, r := range refs {
					a.Track(r.Slot, r.Key, r.Cell)
				}

				b.TrackMany(refs)
			} else {
				keep := map[uint32]bool{}

				for _, r := range refs {
					a.Track(r.Slot, r.Key, r.Cell)
					keep[r.Slot] = true
				}

				for _, p := range slices.Clone(a.tab.Load().peers) {
					if !keep[p.slot] {
						a.Untrack(p.slot)
					}
				}

				b.Sync(refs)
			}

			if !slices.Equal(snap(a), snap(b)) || !slices.Equal(cellSnap(a), cellSnap(b)) {
				t.Fatalf("iter %d: track %v\nbatch %v", iter, snap(a), snap(b))
			}

			for s := range uint32(50) {
				if (a.tab.Load().lookup(s) == nil) != (b.tab.Load().lookup(s) == nil) {
					t.Fatalf("iter %d: lookup %d differs", iter, s)
				}
			}

			if a.maxEjected.Load() != b.maxEjected.Load() {
				t.Fatalf("iter %d: maxEjected", iter)
			}
		}
	}
}

func TestSyncKeepsState(t *testing.T) {
	tr := newT(Config{})

	var refs []Ref
	for i := range 20 {
		refs = append(refs, Ref{Slot: uint32(i), Key: uint64(100 + i), Cell: uint32(i / 4)})
	}

	tr.Sync(refs)
	fail(tr, 3, 5)
	tr.Tick(0)

	if s, _, _ := tr.PeerState(3); s != StateEjected {
		t.Fatalf("state %v", s)
	}

	// Same set, slot 5 with a new key, slot 19 removed.
	refs[5].Key = 999
	tr.Sync(refs[:19])

	if s, n, _ := tr.PeerState(3); s != StateEjected || n != 1 {
		t.Fatalf("sync must keep state: %v %d", s, n)
	}

	if tr.Tracked() != 19 {
		t.Fatalf("tracked %d", tr.Tracked())
	}

	// No-op Sync must not rebuild.
	before := tr.tab.Load()
	tr.Sync(refs[:19])

	if tr.tab.Load() != before {
		t.Fatal("no-op Sync rebuilt the table")
	}

	tr.Sync(nil)

	if tr.Tracked() != 0 || len(tr.cells) != 0 {
		t.Fatalf("Sync(nil): tracked %d cells %d", tr.Tracked(), len(tr.cells))
	}
}

// TestEjectLoserKeepsTiming: a tryEject that loses the race against
// another ejection must not reset the winner's ejection timing.
func TestEjectLoserKeepsTiming(t *testing.T) {
	var ejects int

	tr := newT(Config{Hooks: Hooks{OnEject: func(uint32, Reason, int, time.Duration) { ejects++ }}})
	trackN(tr, 20, 4)
	tr.Tick(0)
	fail(tr, 1, 5)
	tr.Tick(sec)

	p := tr.tab.Load().lookup(1)
	until := p.until.Load()

	if until == 0 || ejects != 1 {
		t.Fatalf("not timed: %d %d", until, ejects)
	}

	if tr.tryEject(p, bHealthy, ReasonSuccessRate) {
		t.Fatal("second ejection must fail")
	}

	tr.Tick(2 * sec)

	if p.until.Load() != until || ejects != 1 {
		t.Fatalf("loser re-timed the ejection: until %d -> %d, ejects %d", until, p.until.Load(), ejects)
	}
}

// TestIdleDegradedRecovers: a Degraded peer gets no traffic, so its EWMA
// must decay without observations or it would stay Degraded forever.
func TestIdleDegradedRecovers(t *testing.T) {
	tr := newT(Config{})
	trackN(tr, 5, 5)
	tr.Tick(0)

	for i := range 5 {
		lat := 5 * time.Millisecond
		if i == 2 {
			lat = 200 * time.Millisecond
		}

		tr.Observe(uint32(i), Success, lat)
	}

	now := sec / 10
	tr.Tick(now)

	if tr.Usable(2) != Degraded {
		t.Fatal("slow peer must be degraded")
	}

	// Others keep getting traffic; peer 2 gets none.
	for range 600 {
		now += sec / 10

		for _, s := range []uint32{0, 1, 3, 4} {
			tr.Observe(s, Success, 5*time.Millisecond)
		}

		tr.Tick(now)
	}

	if tr.Usable(2) != Healthy {
		_, _, e := tr.PeerState(2)
		t.Fatalf("idle degraded peer never recovered, ewma %v", e)
	}

	// Idle peers do not decay below the median, so they do not drag it
	// down and make normal peers look slow.
	if _, _, e := tr.PeerState(2); e < 4*time.Millisecond {
		t.Fatalf("idle ewma decayed below the median: %v", e)
	}
}

func TestLatencyClamp(t *testing.T) {
	tr := newT(Config{})
	trackN(tr, 1, 1)

	for range 10 {
		tr.Observe(0, Success, time.Duration(1<<62))
	}

	tr.Tick(0)
	tr.Tick(sec)

	if _, _, e := tr.PeerState(0); e <= 0 || e > time.Hour {
		t.Fatalf("ewma %v", e)
	}
}

func BenchmarkTrackBootstrap(b *testing.B) {
	for _, n := range []int{256, 4096} {
		refs := make([]Ref, n)
		for i := range refs {
			refs[i] = Ref{Slot: uint32(i * 7), Key: uint64(i), Cell: uint32(i / 4)}
		}

		b.Run("Track/"+strconv.Itoa(n), func(b *testing.B) {
			for b.Loop() {
				tr := newT(Config{})
				for _, r := range refs {
					tr.Track(r.Slot, r.Key, r.Cell)
				}
			}
		})
		b.Run("Sync/"+strconv.Itoa(n), func(b *testing.B) {
			for b.Loop() {
				newT(Config{}).Sync(refs)
			}
		})
	}
}
