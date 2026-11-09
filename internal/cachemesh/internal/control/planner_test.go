// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"errors"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

func randomMembers(r *rand.Rand, n int, slot0 uint32) []Member {
	ms := make([]Member, n)
	for i := range ms {
		ms[i] = Member{Slot: slot0 + uint32(i), Key: r.Uint64(), Weight: view.MaxWeight, Cell: uint32(r.IntN(max(1, n/4)))}
	}

	return ms
}

// step applies one plan and checks its invariants.
func step(t *testing.T, pl *Planner, tb *view.Table, ms []Member, epoch uint32) (*view.Table, Plan) {
	t.Helper()

	plan, err := pl.Plan(tb, ms)
	if err != nil {
		t.Fatal(err)
	}

	parts, owners := slices.Clone(plan.Parts), slices.Clone(plan.Owners)

	nt, err := tb.Assign(parts, owners, epoch)
	if err != nil {
		t.Fatal(err)
	}

	assignable := map[uint32]bool{}
	anyW := false

	for _, m := range ms {
		if !m.Gone && m.Weight > 0 {
			anyW = true
		}
	}

	for _, m := range ms {
		if !m.Gone && (m.Weight > 0 || !anyW) {
			assignable[m.Slot] = true
		}
	}

	if len(assignable) > 0 {
		nt.ForEach(func(p uint32, e view.Entry) {
			if !assignable[e.Owner] {
				t.Fatalf("partition %d left on non-assignable owner %d", p, e.Owner)
			}
		})
	}

	return nt, plan
}

func TestPlannerConverges(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 1))
	pl := NewPlanner(Config{MaxMoves: 512})

	const bits = 14

	tb := view.NewTable(bits, view.NoMember)
	ms := randomMembers(r, 40, 0)

	for i := range ms {
		ms[i].Weight = 0 // bootstrap
	}

	nextSlot := uint32(len(ms))
	epoch := uint32(1)

	settle := func(label string) {
		t.Helper()

		for range 200 {
			var plan Plan

			tb, plan = step(t, pl, tb, ms, epoch)
			epoch++

			if len(plan.Parts) == 0 {
				break
			}
		}

		ok, err := pl.Settled(tb, ms)
		if err != nil || !ok {
			t.Fatalf("%s: not settled", label)
		}

		tg, _ := pl.Targets(bits, ms)
		loads := tb.Loads(nil)

		for i, m := range ms {
			if !m.Gone && int(m.Slot) < len(loads) && int64(loads[m.Slot]) != tg[i] {
				t.Fatalf("%s: member %d load %d target %d", label, m.Slot, loads[m.Slot], tg[i])
			}
		}
	}

	settle("bootstrap")

	for range 30 {
		switch r.IntN(4) {
		case 0: // add joiners and ramp them
			for range r.IntN(5) + 1 {
				m := Member{Slot: nextSlot, Key: r.Uint64(), Cell: uint32(r.IntN(10))}
				nextSlot++

				ms = append(ms, m)
			}
		case 1: // remove some, sometimes listing them as Gone
			for range r.IntN(5) + 1 {
				if len(ms) < 3 {
					break
				}

				i := r.IntN(len(ms))
				if r.IntN(2) == 0 {
					ms[i].Gone = true
				} else {
					ms = slices.Delete(ms, i, i+1)
				}
			}
		case 2: // drain
			ms[r.IntN(len(ms))].Weight = 0
		case 3: // ramp
			for i := range ms {
				switch ms[i].Weight {
				case 0:
					ms[i].Weight = 250
				case 250:
					ms[i].Weight = 500
				case 500:
					ms[i].Weight = 1000
				}
			}
		}

		settle("round")

		ms = slices.DeleteFunc(ms, func(m Member) bool { return m.Gone })
	}
}

func TestPlannerCellMatePreference(t *testing.T) {
	pl := NewPlanner(Config{MaxMoves: -1})
	ms := []Member{
		{Slot: 0, Key: 10, Weight: 1000, Cell: 0},
		{Slot: 1, Key: 20, Weight: 1000, Cell: 0},
		{Slot: 2, Key: 30, Weight: 1000, Cell: 0},
		{Slot: 3, Key: 40, Weight: 1000, Cell: 1},
		{Slot: 4, Key: 50, Weight: 1000, Cell: 1},
	}

	tb := view.NewTable(10, view.NoMember)

	tb, _ = step(t, pl, tb, ms, 1)

	// Slot 0 leaves (drains): its partitions go to cell-mates 1 and 2 while
	// they are under target, ranked by score.
	ms[0].Weight = 0
	before := tb

	tb, plan := step(t, pl, tb, ms, 2)
	if plan.Mandatory != len(plan.Parts) {
		t.Fatal("optional moves disabled")
	}

	tg, _ := pl.Targets(10, ms)
	used := map[uint32]int64{}

	for _, s := range []uint32{1, 2, 3, 4} {
		used[s] = int64(before.Loads(nil)[s])
	}

	for i, p := range plan.Parts {
		o := plan.Owners[i]

		if before.Owner(p) != 0 {
			t.Fatal("moved a partition not owned by slot 0")
		}

		// Expected: best-scored cell-mate with room, else under-target fallback.
		best := view.NoMember

		var bs uint64

		for _, j := range []uint32{1, 2} {
			if used[j] >= tg[j] {
				continue
			}

			sc := view.Score(ms[j].Key, uint64(p))
			if best == view.NoMember || view.Better(sc, ms[j].Key, bs, ms[best].Key) {
				best, bs = j, sc
			}
		}

		if best != view.NoMember && o != best {
			t.Fatalf("partition %d went to %d, want cell-mate %d", p, o, best)
		}

		used[o]++
	}

	if tb.Get(plan.Parts[0]).Prev != 0 {
		t.Fatal("Prev must be set")
	}
}

func TestPlannerDeterministic(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 7))
	ms := randomMembers(r, 200, 5)
	tb := view.NewTable(12, view.NoMember)

	tb, _ = step(t, NewPlanner(Config{}), tb, ms, 1)

	for i := range 20 {
		ms[i*3].Weight = 0
	}

	ms = ms[:190]
	p1, _ := NewPlanner(Config{MaxMoves: 100}).Plan(tb, ms)
	p1 = Plan{Parts: slices.Clone(p1.Parts), Owners: slices.Clone(p1.Owners), Mandatory: p1.Mandatory}

	sh := slices.Clone(ms)
	r.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })

	pl := NewPlanner(Config{MaxMoves: 100})
	_, _ = pl.Plan(view.NewTable(3, 0), sh[:3]) // dirty the buffers

	p2, _ := pl.Plan(tb, sh)
	if !slices.Equal(p1.Parts, p2.Parts) || !slices.Equal(p1.Owners, p2.Owners) || p1.Mandatory != p2.Mandatory {
		t.Fatal("plan depends on member order or planner history")
	}
}

func TestPlannerErrorsAndEmpty(t *testing.T) {
	pl := NewPlanner(Config{})
	tb := view.NewTable(4, 1)

	if _, err := pl.Plan(tb, []Member{{Slot: 1}, {Slot: 1}}); !errors.Is(err, view.ErrInvalidTable) {
		t.Fatal("duplicate slot")
	}

	if _, err := pl.Plan(tb, []Member{{Slot: view.NoMember}}); !errors.Is(err, view.ErrInvalidTable) {
		t.Fatal("NoMember slot")
	}

	p, err := pl.Plan(tb, nil)
	if err != nil || len(p.Parts) != 0 {
		t.Fatal("empty membership must plan nothing")
	}

	if ok, _ := pl.Settled(tb, nil); !ok {
		t.Fatal("empty membership is settled")
	}

	if ok, _ := pl.Settled(tb, []Member{{Slot: 2, Weight: 1}}); ok {
		t.Fatal("orphans are not settled")
	}

	// All Gone: nothing to assign to.
	p, _ = pl.Plan(tb, []Member{{Slot: 1, Gone: true}})
	if len(p.Parts) != 0 {
		t.Fatal("gone-only membership must plan nothing")
	}
}

func TestTargets(t *testing.T) {
	pl := NewPlanner(Config{})
	ms := []Member{
		{Slot: 0, Key: 3, Weight: 1000},
		{Slot: 1, Key: 1, Weight: 1000},
		{Slot: 2, Key: 2, Weight: 1000},
		{Slot: 3, Key: 0, Weight: 0},
	}

	tg, _ := pl.Targets(4, ms) // 16 / 3 = 5 rem 1 -> key 1 (slot 1)
	if !slices.Equal(tg, []int64{5, 6, 5, 0}) {
		t.Fatalf("targets %v", tg)
	}

	ms[0].Weight = 250 // W=2250: 16*250/2250=1, 16*1000/2250=7,7 -> rem 1
	tg, _ = pl.Targets(4, ms)

	if !slices.Equal(tg, []int64{1, 8, 7, 0}) {
		t.Fatalf("weighted targets %v", tg)
	}
}

func BenchmarkPlan(b *testing.B) {
	for _, removed := range []int{0, 1, 10} {
		b.Run("removed="+strconv.Itoa(removed), func(b *testing.B) {
			r := rand.New(rand.NewPCG(3, 4))
			ms := randomMembers(r, 100_000, 0)
			pl := NewPlanner(Config{})
			tb := view.NewTable(20, view.NoMember)

			for {
				plan, _ := pl.Plan(tb, ms)
				if len(plan.Parts) == 0 {
					break
				}

				tb, _ = tb.Assign(plan.Parts, plan.Owners, 1)
			}

			ms = ms[removed:]

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if _, err := pl.Plan(tb, ms); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
