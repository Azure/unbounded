// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"bytes"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

// gen generates random valid deltas. It plans each op on a scratch view
// that has every earlier op of the delta applied.
type gen struct {
	t      *testing.T
	r      *rand.Rand
	nextID int
}

func (g *gen) members(v *view.View) []uint32 {
	var out []uint32

	v.ForEachMember(func(s uint32, _ *view.Member) bool {
		out = append(out, s)
		return true
	})

	return out
}

func (g *gen) newMember(v *view.View) view.Member {
	g.nextID++
	m := viewtest.NewMember("g-"+strconv.Itoa(g.nextID), g.r.Uint64())
	m.Cell = g.r.Uint32N(v.Shape().Cells())

	switch g.r.IntN(3) {
	case 0:
		m.State = view.Joining
	case 1:
		m.State, m.Weight = view.Active, 1+g.r.Uint32N(view.MaxWeight)
	default:
		m.State = view.Draining
	}

	return m
}

func (g *gen) first() *view.Delta {
	c := view.Config{
		PartitionBits:   uint8(4 + g.r.IntN(7)),
		InitialOwner:    0,
		SlotReuseEpochs: uint32(g.r.IntN(5)),
		HandoffEpochs:   2,
	}

	ops := []view.Op{view.OpNewTerm(1, 0), view.OpConfig(c), view.OpEpoch(1, time.Second)}

	g.nextID++

	m := viewtest.NewMember("g-"+strconv.Itoa(g.nextID), 1)
	m.State = view.Active

	ops = append(ops, view.OpAddMember(0, m))

	return view.NewDelta(view.Genesis(), ops...)
}

// op returns a random op valid on cur, or ops (assign then remove).
func (g *gen) op(cur *view.View) []view.Op {
	r := g.r
	ms := g.members(cur)
	leader := cur.Leader()

	switch r.IntN(10) {
	case 0:
		return []view.Op{view.OpEpoch(cur.Epoch()+uint64(r.IntN(6)), time.Duration(1+r.IntN(3))*time.Second)}
	case 1:
		seed := uint64(r.IntN(4))
		return []view.Op{view.OpInvalidate(seed, cur.Gen(seed)+1+uint32(r.IntN(2)))}
	case 2, 3:
		slot := cur.SlotCap()
		if r.IntN(2) == 0 {
			slot = cur.NextSlot()
		}

		return []view.Op{view.OpAddMember(slot, g.newMember(cur))}
	case 4:
		cand := make([]uint32, 0, len(ms))
		for _, s := range ms {
			if s != leader {
				cand = append(cand, s)
			}
		}

		if len(cand) == 0 {
			return nil
		}

		victim := cand[r.IntN(len(cand))]

		var parts, owners []uint32

		t := cur.Table()
		for p := range uint32(t.Len()) {
			if t.Owner(p) == victim {
				parts = append(parts, p)
				owners = append(owners, cand[r.IntN(len(cand))])
			}
		}

		for i, o := range owners {
			if o == victim {
				owners[i] = leader
			}
		}

		var ops []view.Op
		if len(parts) > 0 {
			ops = append(ops, view.OpAssign(parts, owners))
		}

		return append(ops, view.OpRemoveMember(victim))
	case 5:
		s := ms[r.IntN(len(ms))]

		st := view.MemberState(1 + r.IntN(3))

		w := uint32(0)
		if st == view.Active {
			w = r.Uint32N(view.MaxWeight + 1)
		}

		return []view.Op{view.OpSetState(s, st, w)}
	case 6:
		return []view.Op{view.OpSetCell(ms[r.IntN(len(ms))], r.Uint32N(cur.Shape().Cells()))}
	case 7:
		if r.IntN(3) != 0 {
			return nil
		}

		sizes := make([]uint32, 1+r.IntN(3))
		for i := range sizes {
			sizes[i] = 1 + r.Uint32N(4)
		}

		sh, err := view.NewShape(sizes...)
		if err != nil {
			g.t.Fatal(err)
		}

		cells := make([]uint32, cur.SlotCap())
		for s := range cells {
			cells[s] = view.NoCell
			if cur.IsMember(uint32(s)) {
				cells[s] = r.Uint32N(sh.Cells())
			}
		}

		return []view.Op{view.OpReshape(sh, cells)}
	default:
		n := 1 + r.IntN(40)
		perm := r.Perm(cur.Table().Len())[:min(n, cur.Table().Len())]
		parts := make([]uint32, len(perm))
		owners := make([]uint32, len(perm))

		for i, p := range perm {
			parts[i], owners[i] = uint32(p), ms[r.IntN(len(ms))]
		}

		return []view.Op{view.OpAssign(parts, owners)}
	}
}

func (g *gen) delta(base *view.View) *view.Delta {
	var ops []view.Op

	cur := base
	if g.r.IntN(15) == 0 {
		ms := g.members(base)
		ops = append(ops, view.OpNewTerm(base.Term()+1+uint64(g.r.IntN(2)), ms[g.r.IntN(len(ms))]))
		cur = g.step(cur, ops)
	}

	for range 1 + g.r.IntN(6) {
		o := g.op(cur)
		if len(o) == 0 {
			continue
		}

		for _, op := range o {
			cur = g.step(cur, []view.Op{op})
		}

		ops = append(ops, o...)
	}

	return view.NewDelta(base, ops...)
}

func (g *gen) step(v *view.View, ops []view.Op) *view.View {
	nv, err := view.Apply(v, view.NewDelta(v, ops...))
	if err != nil {
		g.t.Fatalf("generator produced an invalid op %v: %v", ops[0].Kind, err)
	}

	return nv
}

func checkRoundTrips(t *testing.T, base, v *view.View, d *view.Delta) {
	t.Helper()

	enc := view.EncodeDelta(d)

	dd, err := view.DecodeDelta(enc)
	if err != nil {
		t.Fatalf("decode delta: %v", err)
	}

	if !bytes.Equal(view.EncodeDelta(dd), enc) {
		t.Fatal("delta re-encoding differs")
	}

	v2, err := view.Apply(base, dd)
	if err != nil || !view.Equal(v, v2) {
		t.Fatalf("decoded delta applies differently: %v", err)
	}

	snap := view.EncodeSnapshot(v)

	sv, err := view.DecodeSnapshot(snap)
	if err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}

	if !view.Equal(v, sv) {
		t.Fatal("snapshot round trip not equal")
	}

	if !bytes.Equal(view.EncodeSnapshot(sv), snap) {
		t.Fatal("snapshot re-encoding differs")
	}
}

func TestRandomDeltas(t *testing.T) {
	seeds := 20
	steps := 150

	if testing.Short() {
		seeds, steps = 4, 60
	}

	for seed := range uint64(seeds) {
		g := &gen{t: t, r: rand.New(rand.NewPCG(seed, 0x5eed))}
		v := view.Genesis()

		d := g.first()

		nv, err := view.Apply(v, d)
		if err != nil {
			t.Fatal(err)
		}

		checkRoundTrips(t, v, nv, d)
		v = nv

		for i := range steps {
			d := g.delta(v)

			nv, err := view.Apply(v, d)
			if err != nil {
				t.Fatalf("seed %d step %d: %v", seed, i, err)
			}

			if err := view.Validate(nv); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, i, err)
			}

			if err := view.Validate(v); err != nil {
				t.Fatalf("seed %d step %d: base changed: %v", seed, i, err)
			}

			checkRoundTrips(t, v, nv, d)
			v = nv
		}
	}
}
