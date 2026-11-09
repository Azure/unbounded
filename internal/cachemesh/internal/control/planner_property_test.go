// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"math/rand/v2"
	"slices"
	"testing"
)

// TestPlanProperties checks per-plan invariants on random member sets with
// duplicate keys, Gone members, weight-0 members, and random tables: moves
// are bounded, partitions are unique, owners are assignable, no member is
// pushed above its target, and the plan does not depend on member order.
func TestPlanProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 9))

	for iter := range 200 {
		bits := uint8(view.MinPartitionBits + r.IntN(4))
		n := 1 + r.IntN(30)
		ms := make([]Member, n)

		for i := range ms {
			ms[i] = Member{
				Slot:   uint32(i * 2),
				Key:    uint64(r.IntN(8)), // many duplicate keys
				Weight: []uint32{0, 250, 500, view.MaxWeight}[r.IntN(4)],
				Cell:   uint32(r.IntN(4)),
				Gone:   r.IntN(8) == 0,
			}
		}

		tb := view.NewTableFunc(bits, func(uint32) view.Entry {
			return view.Entry{Owner: uint32(r.IntN(2*n + 4)), Prev: view.NoMember}
		})
		maxMoves := r.IntN(64) - 1
		pl := NewPlanner(Config{MaxMoves: maxMoves})

		plan, err := pl.Plan(tb, ms)
		if err != nil {
			t.Fatal(err)
		}

		parts, owners := slices.Clone(plan.Parts), slices.Clone(plan.Owners)

		limit := maxMoves
		if maxMoves == 0 {
			limit = DefaultMaxMoves
		} else if maxMoves < 0 {
			limit = 0
		}

		if len(parts)-plan.Mandatory > limit {
			t.Fatalf("iter %d: %d optional moves > %d", iter, len(parts)-plan.Mandatory, limit)
		}

		tg, _ := pl.Targets(bits, ms)
		idx := map[uint32]int{}

		for i, m := range ms {
			idx[m.Slot] = i
		}

		loads := tb.Loads(nil)
		grow := func(s uint32) int64 {
			if int(s) < len(loads) {
				return int64(loads[s])
			}

			return 0
		}
		after := map[uint32]int64{}

		for i, p := range parts {
			o := owners[i]

			j, ok := idx[o]
			if !ok || ms[j].Gone || tg[j] == 0 {
				t.Fatalf("iter %d: partition %d to non-assignable %d", iter, p, o)
			}

			if _, seen := after[o]; !seen {
				after[o] = grow(o)
			}

			after[o]++
			if after[o] > tg[j] && grow(o) < after[o] {
				t.Fatalf("iter %d: member %d pushed to %d above target %d", iter, o, after[o], tg[j])
			}
		}

		sorted := slices.Clone(parts)
		slices.Sort(sorted)

		if len(slices.Compact(sorted)) != len(parts) {
			t.Fatalf("iter %d: duplicate partitions", iter)
		}

		if _, err := tb.Assign(parts, owners, 1); err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}

		sh := slices.Clone(ms)
		r.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })

		p2, _ := NewPlanner(Config{MaxMoves: maxMoves}).Plan(tb, sh)
		if !slices.Equal(parts, p2.Parts) || !slices.Equal(owners, p2.Owners) {
			t.Fatalf("iter %d: plan depends on member order", iter)
		}
	}
}
