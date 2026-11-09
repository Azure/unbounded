// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

// TestDivergentHealthViews gives every member its own health tracker with
// different opinions (random ejected and degraded peers, each member's
// opinion drawn independently). Requests must still be loop-free, finish
// within the TTL, take at most D hops plus one failover, and end at the
// owner or at a member of the owner's cell serving as a backup. Relay hops
// fix one coordinate each regardless of health, and the backup rule never
// chains, so differing opinions cannot bounce a request between members.
func TestDivergentHealthViews(t *testing.T) {
	for _, tc := range []struct {
		n, maxN int
		bad     int
		panicP  int
	}{
		{n: 300, maxN: 256, bad: 30},
		{n: 1500, maxN: 90, bad: 150},
		{n: 2000, maxN: 20, bad: 200},
		{n: 1500, maxN: 40, bad: 600, panicP: 30},
	} {
		v := viewtest.Build(t, tc.n, viewtest.Options{Seed: uint64(tc.n + tc.bad), MaxNeighbors: tc.maxN})
		d := int(v.Shape().Dims)
		rng := rand.New(rand.NewPCG(uint64(tc.n), uint64(tc.bad)))

		f := &fixture{v: v, st: make([]*State, v.SlotCap())}
		for i := range f.st {
			tr := newTracker(tc.panicP)
			perm := rng.Perm(tc.n)[:tc.bad]

			refs := make([]TrackRef, 0, len(perm))

			for _, s := range perm {
				if s != i {
					refs = append(refs, TrackRef{Slot: view.Slot(s), Key: v.Key(view.Slot(s)), Cell: fakeCell})
				}
			}

			tr.TrackMany(refs)

			for k, r := range refs {
				if k%3 == 0 {
					tr.Observe(r.Slot, Overload, 0)
				} else {
					fail(tr, r.Slot)
				}
			}

			tr.Tick(0)

			f.st[i] = NewState(v, view.Slot(i), newEnv(tr, 1))
		}

		hist := make([]int, d+3)
		bits := v.PartitionBits()

		for o := range tc.n {
			for range 8 {
				p := rng.Uint32() >> (32 - bits)
				path, dec := walk(t, f, view.Slot(o), p)

				if dec.Kind != KindSelf {
					t.Fatalf("n=%d: ended with %v", tc.n, dec.Kind)
				}

				if len(path) > d+1 {
					t.Fatalf("n=%d: %d hops > D+1=%d: %v", tc.n, len(path), d+1, path)
				}

				last := view.Slot(o)
				if len(path) > 0 {
					last = path[len(path)-1]
				}

				owner := v.Table().Owner(p)
				if last != owner && (dec.Header.Flags&FlagBackup == 0 || v.Cell(last) != v.Cell(owner)) {
					t.Fatalf("n=%d: ended at %d (cell %d), owner %d (cell %d)", tc.n, last, v.Cell(last), owner, v.Cell(owner))
				}

				hist[len(path)]++
			}
		}

		t.Logf("n=%d D=%d bad=%d hops: %v", tc.n, d, tc.bad, hist)
	}
}

func fail(tr *HealthTracker, s view.Slot) {
	for range 5 {
		tr.Observe(s, Failure, time.Millisecond)
	}
}
