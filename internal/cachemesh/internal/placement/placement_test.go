// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
)

func TestNewTable(t *testing.T) {
	for _, bits := range []uint8{1, 5, 10, 11, 20} {
		tb := NewTable(bits, 7)
		if tb.Len() != 1<<bits || tb.Bits() != bits {
			t.Fatalf("bits %d: len %d", bits, tb.Len())
		}

		n := 0

		tb.ForEach(func(p uint32, e Entry) {
			if p != uint32(n) || e != (Entry{Owner: 7, Prev: NoMember}) {
				t.Fatalf("entry %d = %+v", p, e)
			}

			n++
		})

		if n != tb.Len() {
			t.Fatalf("ForEach visited %d", n)
		}
	}

	if ValidBits(0) || ValidBits(25) || !ValidBits(24) {
		t.Fatal("ValidBits")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("NewTable(0) must panic")
		}
	}()

	NewTable(0, 0)
}

func TestAssign(t *testing.T) {
	a := NewTable(12, 1) // 4 chunks

	b, err := a.Assign([]uint32{5, 6, 2000}, []uint32{2, 1, 3}, 9)
	if err != nil {
		t.Fatal(err)
	}

	if got := b.Get(5); got != (Entry{Owner: 2, Prev: 1, Moved: 9}) {
		t.Fatalf("moved entry %+v", got)
	}

	if got := b.Get(6); got != (Entry{Owner: 1, Prev: NoMember}) {
		t.Fatalf("same-owner entry changed: %+v", got)
	}

	if b.Owner(2000) != 3 || a.Owner(5) != 1 || a.Owner(2000) != 1 {
		t.Fatal("original modified or assign lost")
	}

	if SameChunk(a, b, 0) || SameChunk(a, b, 1) || !SameChunk(a, b, 2) || !SameChunk(a, b, 3) {
		t.Fatal("chunk sharing wrong")
	}

	c, _ := b.Assign([]uint32{5}, []uint32{4}, 10)
	if got := c.Get(5); got != (Entry{Owner: 4, Prev: 2, Moved: 10}) {
		t.Fatalf("second move %+v", got)
	}

	if !SameChunk(b, c, 1) {
		t.Fatal("chunk 1 must be shared")
	}

	// No-op assign returns the same table.
	if d, _ := c.Assign([]uint32{5}, []uint32{4}, 11); d != c {
		t.Fatal("no-op assign must return the receiver")
	}

	if !Equal(a, NewTable(12, 1)) || Equal(a, b) || a.Hash() == b.Hash() || a.Hash() != NewTable(12, 1).Hash() {
		t.Fatal("Equal/Hash")
	}

	if Equal(a, NewTable(11, 1)) {
		t.Fatal("different bits must differ")
	}

	// Bootstrap: Prev stays NoMember.
	e, _ := NewTable(4, NoMember).Assign([]uint32{3}, []uint32{0}, 1)
	if e.Get(3) != (Entry{Owner: 0, Prev: NoMember, Moved: 1}) {
		t.Fatalf("bootstrap %+v", e.Get(3))
	}

	bad := []struct {
		parts, owners []uint32
	}{
		{[]uint32{1}, nil},
		{[]uint32{1 << 12}, []uint32{0}},
		{[]uint32{1}, []uint32{NoMember}},
		{[]uint32{1, 2, 1}, []uint32{3, 4, 5}},
	}
	for _, c := range bad {
		if _, err := a.Assign(c.parts, c.owners, 1); !errors.Is(err, ErrInvalid) {
			t.Errorf("Assign(%v,%v) err %v", c.parts, c.owners, err)
		}
	}
}

func TestLoads(t *testing.T) {
	tb := NewTable(4, NoMember)
	tb, _ = tb.Assign([]uint32{0, 1, 2}, []uint32{3, 3, 0}, 1)

	dst := make([]uint32, 2, 10)
	dst[1] = 99
	dst[:10][7] = 42 // stale capacity must be cleared

	got := tb.Loads(dst)
	if !slices.Equal(got, []uint32{1, 0, 0, 2}) {
		t.Fatalf("loads %v", got)
	}
}

func randomMembers(r *rand.Rand, n int, slot0 uint32) []Member {
	ms := make([]Member, n)
	for i := range ms {
		ms[i] = Member{Slot: slot0 + uint32(i), Key: r.Uint64(), Weight: MaxWeight, Cell: uint32(r.IntN(max(1, n/4)))}
	}

	return ms
}

// step applies one plan and checks its invariants.
func step(t *testing.T, pl *Planner, tb *Table, ms []Member, epoch uint32) (*Table, Plan) {
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
		nt.ForEach(func(p uint32, e Entry) {
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

	tb := NewTable(bits, NoMember)
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

	tb := NewTable(10, NoMember)

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
		best := NoMember

		var bs uint64

		for _, j := range []uint32{1, 2} {
			if used[j] >= tg[j] {
				continue
			}

			sc := hashing.Score(ms[j].Key, uint64(p))
			if best == NoMember || hashing.Better(sc, ms[j].Key, bs, ms[best].Key) {
				best, bs = j, sc
			}
		}

		if best != NoMember && o != best {
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
	tb := NewTable(12, NoMember)

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
	_, _ = pl.Plan(NewTable(3, 0), sh[:3]) // dirty the buffers

	p2, _ := pl.Plan(tb, sh)
	if !slices.Equal(p1.Parts, p2.Parts) || !slices.Equal(p1.Owners, p2.Owners) || p1.Mandatory != p2.Mandatory {
		t.Fatal("plan depends on member order or planner history")
	}
}

func TestPlannerErrorsAndEmpty(t *testing.T) {
	pl := NewPlanner(Config{})
	tb := NewTable(4, 1)

	if _, err := pl.Plan(tb, []Member{{Slot: 1}, {Slot: 1}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate slot")
	}

	if _, err := pl.Plan(tb, []Member{{Slot: NoMember}}); !errors.Is(err, ErrInvalid) {
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

var sink uint32

func BenchmarkGet(b *testing.B) {
	tb := NewTable(20, 3)

	var x uint32
	for i := range b.N {
		x += tb.Get(uint32(i) & (1<<20 - 1)).Owner
	}

	sink = x
}

func BenchmarkAssign(b *testing.B) {
	for _, k := range []int{1, 64, 4096} {
		b.Run(strconv.Itoa(k), func(b *testing.B) {
			r := rand.New(rand.NewPCG(1, 2))
			tb := NewTable(20, 0)
			parts := make([]uint32, k)
			owners := make([]uint32, k)

			seen := map[uint32]bool{}

			for i := range parts {
				p := r.Uint32N(1 << 20)
				for seen[p] {
					p = r.Uint32N(1 << 20)
				}

				seen[p] = true
				parts[i], owners[i] = p, 1
			}

			b.ReportAllocs()

			for range b.N {
				if _, err := tb.Assign(parts, owners, 1); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPlan(b *testing.B) {
	for _, removed := range []int{0, 1, 10} {
		b.Run("removed="+strconv.Itoa(removed), func(b *testing.B) {
			r := rand.New(rand.NewPCG(3, 4))
			ms := randomMembers(r, 100_000, 0)
			pl := NewPlanner(Config{})
			tb := NewTable(20, NoMember)

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
