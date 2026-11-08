// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"bytes"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

// mutate returns a possibly invalid variant of op on cur.
func (g *gen) mutate(cur *view.View, op view.Op) view.Op {
	r := g.r
	ms := g.members(cur)
	anySlot := func() uint32 {
		switch r.IntN(4) {
		case 0:
			return ms[r.IntN(len(ms))]
		case 1:
			return cur.SlotCap() + uint32(r.IntN(3))
		case 2:
			return view.NoSlot
		default:
			return r.Uint32N(cur.SlotCap() + 1)
		}
	}

	switch r.IntN(14) {
	case 0:
		op.Slot = anySlot()
	case 1:
		op.Cell = r.Uint32N(cur.Shape().Cells() + 2)
	case 2:
		op.State = view.MemberState(r.IntN(5))
		op.Weight = r.Uint32N(placement.MaxWeight + 2)
	case 3:
		// Duplicate ID of an existing member.
		var m *view.Member

		m, _ = cur.Member(ms[r.IntN(len(ms))])
		op = view.OpAddMember(cur.NextSlot(), *m)
	case 4:
		op = view.OpNewTerm(cur.Term()+uint64(r.IntN(2)), anySlot())
	case 5:
		op = view.OpRemoveMember(anySlot())
	case 6:
		op = view.OpAssign([]uint32{0, 0}, []uint32{ms[0], ms[0]})
	case 7:
		op = view.OpAssign([]uint32{uint32(cur.Table().Len())}, []uint32{ms[0]})
	case 8:
		op = view.OpAssign([]uint32{0}, []uint32{anySlot()})
	case 9:
		op = view.OpEpoch(cur.Epoch()-uint64(r.IntN(2)), time.Duration(r.IntN(3))*time.Second)
	case 10:
		op = view.OpConfig(view.Config{PartitionBits: uint8(r.IntN(30))})
	case 11:
		op = view.OpInvalidate(r.Uint64N(3), cur.Gen(0)+uint32(r.IntN(2)))
	case 12:
		m := viewtest.NewMember("x-"+strconv.Itoa(r.IntN(1000)), r.Uint64())
		m.Key ^= uint64(r.IntN(2))
		m.State = view.MemberState(r.IntN(4))
		op = view.OpAddMember(anySlot(), m)
	default:
		sh, err := topology.NewShape(1 + r.Uint32N(3))
		if err != nil {
			g.t.Fatal(err)
		}

		cells := make([]uint32, int(cur.SlotCap())+r.IntN(3)-1)
		for i := range cells {
			cells[i] = r.Uint32N(sh.Cells() + 1)
			if r.IntN(3) == 0 {
				cells[i] = topology.NoCell
			}
		}

		op = view.OpReshape(sh, cells)
	}

	return op
}

// TestHostileDeltas applies random, often invalid deltas: Apply must either
// fail and leave base untouched, or return a view that passes Validate.
// Every retained old view must keep its content across later applies.
func TestHostileDeltas(t *testing.T) {
	seeds, steps := 30, 300
	if testing.Short() {
		seeds, steps = 4, 80
	}

	type kept struct {
		v    *view.View
		snap []byte
	}

	for seed := range uint64(seeds) {
		g := &gen{t: t, r: rand.New(rand.NewPCG(seed, 0xbad))}

		v, err := view.Apply(view.Genesis(), g.first())
		if err != nil {
			t.Fatal(err)
		}

		var old []kept

		accepted := 0

		for i := range steps {
			var ops []view.Op

			cur := v
			for range 1 + g.r.IntN(4) {
				o := g.op(cur)
				if len(o) == 0 {
					continue
				}

				if g.r.IntN(3) == 0 {
					j := g.r.IntN(len(o))
					o[j] = g.mutate(cur, o[j])
				}

				ops = append(ops, o...)

				if nv, err := view.Apply(cur, view.NewDelta(cur, o...)); err == nil {
					cur = nv
				}
			}

			before := view.EncodeSnapshot(v)

			nv, err := view.Apply(v, view.NewDelta(v, ops...))
			if !bytes.Equal(view.EncodeSnapshot(v), before) {
				t.Fatalf("seed %d step %d: base changed by Apply (err %v)", seed, i, err)
			}

			if err != nil {
				continue
			}

			if err := view.Validate(nv); err != nil {
				t.Fatalf("seed %d step %d: accepted invalid view: %v", seed, i, err)
			}

			accepted++

			if g.r.IntN(4) == 0 {
				old = append(old, kept{v, before})
			}

			v = nv
		}

		for _, k := range old {
			if !bytes.Equal(view.EncodeSnapshot(k.v), k.snap) {
				t.Fatalf("seed %d: retained view %v changed", seed, k.v.Version())
			}

			if err := view.Validate(k.v); err != nil {
				t.Fatalf("seed %d: retained view %v: %v", seed, k.v.Version(), err)
			}
		}

		if accepted < steps/4 {
			t.Fatalf("seed %d: only %d of %d deltas accepted", seed, accepted, steps)
		}
	}
}

// TestEpochOverflow checks that a removal at a huge epoch does not wrap the
// slot reuse epoch around and make the slot reusable at once.
func TestEpochOverflow(t *testing.T) {
	v := boot(t, 3)
	v = mustApply(t, v, view.OpEpoch(math.MaxUint64-1, time.Second))

	c, _ := v.Config()
	if c.SlotReuseEpochs == 0 {
		t.Skip("boot config has no reuse delay")
	}

	victim := uint32(2)
	if v.Leader() == victim {
		victim = 1
	}

	var parts, owners []uint32

	tb := v.Table()
	for p := range uint32(tb.Len()) {
		if tb.Owner(p) == victim {
			parts, owners = append(parts, p), append(owners, v.Leader())
		}
	}

	var ops []view.Op
	if len(parts) > 0 {
		ops = append(ops, view.OpAssign(parts, owners))
	}

	v = mustApply(t, v, append(ops, view.OpRemoveMember(victim))...)
	mustFail(t, v, view.ErrInvalid, view.OpAddMember(victim, active(99, 0)))
}
