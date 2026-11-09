// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"bytes"
	"errors"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

const testBits = 8

func member(i int) view.Member {
	return viewtest.NewMember("node-"+strconv.Itoa(i), uint64(i)+1)
}

func active(i int, cell uint32) view.Member {
	m := member(i)
	m.State, m.Weight, m.Cell = view.Active, view.MaxWeight, cell

	return m
}

func cfg() view.Config {
	return view.Config{PartitionBits: testBits, InitialOwner: 0, SlotReuseEpochs: 16, HandoffEpochs: 8}
}

func mustApply(t testing.TB, v *view.View, ops ...view.Op) *view.View {
	t.Helper()

	nv, err := view.Apply(v, view.NewDelta(v, ops...))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if err := view.Validate(nv); err != nil {
		t.Fatalf("validate: %v", err)
	}

	return nv
}

func mustFail(t *testing.T, v *view.View, want error, ops ...view.Op) {
	t.Helper()

	before := view.EncodeSnapshot(v)

	_, err := view.Apply(v, view.NewDelta(v, ops...))
	if !errors.Is(err, want) {
		t.Fatalf("apply error %v, want %v", err, want)
	}

	if string(view.EncodeSnapshot(v)) != string(before) {
		t.Fatal("failed apply modified the base view")
	}
}

// boot returns a term-1 view with n active members in one cell, slot 0 the
// leader owning every partition.
func boot(t testing.TB, n int) *view.View {
	t.Helper()

	ops := []view.Op{view.OpNewTerm(1, 0), view.OpConfig(cfg()), view.OpEpoch(10, time.Second)}
	for i := range n {
		ops = append(ops, view.OpAddMember(uint32(i), active(i, 0)))
	}

	return mustApply(t, view.Genesis(), ops...)
}

func TestGenesis(t *testing.T) {
	g := view.Genesis()
	if err := view.Validate(g); err != nil {
		t.Fatal(err)
	}

	if !g.Version().IsZero() || g.Digest() != view.GenesisDigest() || g.Leader() != view.NoSlot || g.Len() != 0 {
		t.Fatal("unexpected genesis")
	}

	if _, ok := g.Config(); ok || g.Table() != nil || g.NextSlot() != 0 {
		t.Fatal("genesis has config")
	}

	// Term 0 deltas need no leader.
	v := mustApply(t, g, view.OpEpoch(1, time.Second), view.OpInvalidate(7, 1))
	if v.Version() != (view.Version{Term: 0, Seq: 1}) || v.Gen(7) != 1 || v.Gen(8) != 0 {
		t.Fatal("term 0 delta")
	}
}

func TestBootstrap(t *testing.T) {
	v := boot(t, 3)

	if v.Len() != 3 || v.SlotCap() != 3 || v.Leader() != 0 || v.Term() != 1 {
		t.Fatalf("len %d cap %d leader %d", v.Len(), v.SlotCap(), v.Leader())
	}

	if v.Load(0) != 1<<testBits || v.Load(1) != 0 || v.Load(1<<30) != 0 {
		t.Fatal("initial owner load")
	}

	if c, ok := v.Config(); !ok || c != cfg() || v.PartitionBits() != testBits {
		t.Fatal("config")
	}

	if v.Shape().Cells() != 1 || len(v.CellMembers(0)) != 3 || v.Occupancy(0) != 3 || v.CellMembers(5) != nil {
		t.Fatal("cells")
	}

	m, ok := v.Member(2)
	if !ok || m.ID != "node-2" || m.Joined != 10 || m.Since != 10 {
		t.Fatalf("member %+v", m)
	}

	if s, ok := v.Lookup("node-1"); !ok || s != 1 {
		t.Fatal("lookup")
	}

	if _, ok := v.Lookup("nope"); ok {
		t.Fatal("lookup of absent ID")
	}

	if _, ok := v.Member(3); ok || v.Key(3) != 0 || v.Cell(3) != view.NoCell || v.State(3) != 0 {
		t.Fatal("absent member")
	}

	// Initial owner must be a member by the end of the delta.
	_, err := view.Apply(view.Genesis(), view.NewDelta(view.Genesis(), view.OpConfig(cfg()),
		view.OpAddMember(0, active(5, 0)), view.OpAddMember(1, active(6, 0))))
	if err != nil {
		t.Fatal(err)
	}

	mustFail(t, view.Genesis(), view.ErrInvalid, view.OpConfig(cfg()), view.OpAddMember(0, active(5, 0)),
		view.OpAddMember(1, active(6, 0)), view.OpRemoveMember(0))

	// InitialOwner NoSlot: a configured view with no members.
	c := cfg()
	c.InitialOwner = view.NoSlot
	e := mustApply(t, view.Genesis(), view.OpConfig(c))

	if e.Len() != 0 || e.Table().Owner(0) != view.NoMember {
		t.Fatal("empty configured view")
	}
}

func TestVersionRules(t *testing.T) {
	v := boot(t, 2)

	d := view.NewDelta(v, view.OpEpoch(11, time.Second))
	d.Base.Seq++

	if _, err := view.Apply(v, d); !errors.Is(err, view.ErrBase) {
		t.Fatalf("base: %v", err)
	}

	d = view.NewDelta(v, view.OpEpoch(11, time.Second))
	d.Version.Seq++
	view.Seal(v, d)

	if _, err := view.Apply(v, d); !errors.Is(err, view.ErrVersion) {
		t.Fatalf("seq: %v", err)
	}

	mustFail(t, v, view.ErrVersion, view.OpNewTerm(1, 0))
	mustFail(t, v, view.ErrInvalid, view.OpEpoch(11, time.Second), view.OpNewTerm(2, 0))

	d = view.NewDelta(v, view.OpNewTerm(3, 1))
	d.Version = view.Version{Term: 3, Seq: 2}
	view.Seal(v, d)

	if _, err := view.Apply(v, d); !errors.Is(err, view.ErrVersion) {
		t.Fatalf("new term seq: %v", err)
	}

	d = view.NewDelta(v, view.OpEpoch(11, time.Second))
	d.Digest[0] ^= 1

	if _, err := view.Apply(v, d); !errors.Is(err, view.ErrDigest) {
		t.Fatalf("digest: %v", err)
	}

	nv := mustApply(t, v, view.OpNewTerm(5, 1))
	if nv.Version() != (view.Version{Term: 5, Seq: 1}) || nv.Leader() != 1 {
		t.Fatal("new term")
	}

	nv = mustApply(t, nv)
	if nv.Version() != (view.Version{Term: 5, Seq: 2}) {
		t.Fatal("empty delta")
	}

	mustFail(t, v, view.ErrInvalid, view.OpNewTerm(6, 9))
}

func TestConfigOnce(t *testing.T) {
	v := boot(t, 1)
	mustFail(t, v, view.ErrInvalid, view.OpConfig(cfg()))

	g := view.Genesis()
	mustFail(t, g, view.ErrInvalid, view.OpAddMember(0, active(0, 0)))
	mustFail(t, g, view.ErrInvalid, view.OpAssign([]uint32{0}, []uint32{0}))
	mustFail(t, g, view.ErrInvalid, view.OpConfig(cfg()), view.OpConfig(cfg()))

	bad := cfg()
	bad.PartitionBits = 0
	mustFail(t, g, view.ErrInvalid, view.OpConfig(bad))

	bad = cfg()
	bad.InitialOwner = view.MaxSlots
	mustFail(t, g, view.ErrInvalid, view.OpConfig(bad))
}

func TestAddMember(t *testing.T) {
	v := boot(t, 2)

	bad := func(mut func(*view.Member)) view.Op {
		m := member(9)
		mut(&m)

		return view.OpAddMember(2, m)
	}

	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.Key++ }))
	mustFail(t, v, view.ErrInvalid, view.OpAddMember(2, member(1)))
	mustFail(t, v, view.ErrInvalid, view.OpAddMember(3, member(9)))
	mustFail(t, v, view.ErrInvalid, view.OpAddMember(1, member(9)))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.Cell = 1 }))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.Weight = 1 }))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.State = 0 }))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.State = 4 }))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) {
		m.State, m.Weight = view.Active, view.MaxWeight+1
	}))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { *m = viewtest.NewMember("", 1) }))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) {
		*m = viewtest.NewMember(string(make([]byte, view.MaxIDLen+1)), 1)
	}))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.Addr = string(make([]byte, view.MaxAddrLen+1)) }))
	mustFail(t, v, view.ErrInvalid, bad(func(m *view.Member) { m.Zone = string(make([]byte, view.MaxZoneLen+1)) }))
	// Duplicate ID within one delta.
	mustFail(t, v, view.ErrInvalid, view.OpAddMember(2, member(9)), view.OpAddMember(3, member(9)))

	in := member(9)
	in.Joined, in.Since = 999, 999
	nv := mustApply(t, v, view.OpEpoch(12, time.Second), view.OpAddMember(2, in))

	m, _ := nv.Member(2)
	if m.Joined != 12 || m.Since != 12 || m.State != view.Joining {
		t.Fatalf("member %+v", m)
	}
}

func TestRemoveAndReuse(t *testing.T) {
	v := boot(t, 3)
	mustFail(t, v, view.ErrInvalid, view.OpRemoveMember(7))
	mustFail(t, v, view.ErrInvalid, view.OpRemoveMember(0)) // leader
	// Slot 0 owns everything; removing an owner without reassigning fails.
	v = mustApply(t, v, view.OpNewTerm(2, 1))
	mustFail(t, v, view.ErrInvalid, view.OpRemoveMember(0))

	parts := make([]uint32, 1<<testBits)
	owners := make([]uint32, len(parts))

	for i := range parts {
		parts[i], owners[i] = uint32(i), 1+uint32(i%2)
	}

	v = mustApply(t, v, view.OpAssign(parts, owners), view.OpRemoveMember(0))

	if v.Len() != 2 || v.SlotCap() != 3 || v.IsMember(0) || v.Load(0) != 0 {
		t.Fatal("remove")
	}

	if _, ok := v.Lookup("node-0"); ok {
		t.Fatal("removed ID still indexed")
	}

	if fs := v.FreeSlots(); len(fs) != 1 || fs[0] != (view.FreeSlot{Slot: 0, ReusableAt: 26}) {
		t.Fatalf("free %+v", fs)
	}

	if v.NextSlot() != 3 {
		t.Fatalf("next slot %d before reuse epoch", v.NextSlot())
	}

	mustFail(t, v, view.ErrInvalid, view.OpAddMember(0, member(5)))
	mustFail(t, v, view.ErrInvalid, view.OpEpoch(25, time.Second), view.OpAddMember(0, member(5)))

	// A removed ID may come back with a new incarnation in a new slot.
	v = mustApply(t, v, view.OpAddMember(3, viewtest.NewMember("node-0", 77)))

	v = mustApply(t, v, view.OpEpoch(26, time.Second))
	if v.NextSlot() != 0 {
		t.Fatalf("next slot %d at reuse epoch", v.NextSlot())
	}

	v = mustApply(t, v, view.OpAddMember(0, member(5)))
	if v.NextSlot() != 4 || len(v.FreeSlots()) != 0 {
		t.Fatal("reuse")
	}
}

func TestSetStateAndCell(t *testing.T) {
	v := boot(t, 2)
	mustFail(t, v, view.ErrInvalid, view.OpSetState(5, view.Active, 1))
	mustFail(t, v, view.ErrInvalid, view.OpSetState(1, view.Draining, 1))
	mustFail(t, v, view.ErrInvalid, view.OpSetState(1, 0, 0))

	v = mustApply(t, v, view.OpEpoch(20, time.Second), view.OpSetState(1, view.Active, 500))
	if m, _ := v.Member(1); m.Since != 10 || m.Weight != 500 {
		t.Fatalf("unchanged state moved Since: %+v", m)
	}

	v = mustApply(t, v, view.OpSetState(1, view.Draining, 0))
	if m, _ := v.Member(1); m.Since != 20 || m.State != view.Draining || m.Joined != 10 {
		t.Fatalf("state change: %+v", m)
	}

	sh, _ := view.NewShape(2, 2)

	mustFail(t, v, view.ErrInvalid, view.OpSetCell(1, 1))
	v = mustApply(t, v, view.OpReshape(sh, []uint32{0, 0}))
	mustFail(t, v, view.ErrInvalid, view.OpSetCell(1, 4))
	mustFail(t, v, view.ErrInvalid, view.OpSetCell(9, 1))

	v = mustApply(t, v, view.OpSetCell(1, 3))
	if v.Cell(1) != 3 || v.Occupancy(0) != 1 || v.CellMembers(3)[0] != 1 {
		t.Fatal("set cell")
	}
}

func TestReshape(t *testing.T) {
	v := boot(t, 6)
	v = mustApply(t, v, view.OpNewTerm(2, 1), view.OpAssign([]uint32{0}, []uint32{1}))
	v = mustApply(t, v, view.OpAssign(allParts(), fill(1)), view.OpRemoveMember(0))

	sh, _ := view.NewShape(3)
	mustFail(t, v, view.ErrInvalid, view.OpReshape(sh, []uint32{view.NoCell, 0, 1, 2, 0}))
	mustFail(t, v, view.ErrInvalid, view.OpReshape(sh, []uint32{0, 0, 1, 2, 0, 1}))
	mustFail(t, v, view.ErrInvalid, view.OpReshape(sh, []uint32{view.NoCell, 0, 1, 3, 0, 1}))
	mustFail(t, v, view.ErrInvalid, view.OpReshape(view.Shape{}, []uint32{view.NoCell, 0, 1, 2, 0, 1}))

	big, _ := view.NewShape(1<<12, 1<<11)
	mustFail(t, v, view.ErrInvalid, view.OpReshape(big, []uint32{view.NoCell, 0, 1, 2, 0, 1}))

	v = mustApply(t, v, view.OpReshape(sh, []uint32{view.NoCell, 0, 1, 2, 0, 1}))
	if *v.Shape() != sh || v.Occupancy(0) != 2 || v.Occupancy(1) != 2 || v.Occupancy(2) != 1 {
		t.Fatal("reshape occupancy")
	}

	cs := v.CellMembers(0)
	if v.Key(cs[0]) > v.Key(cs[1]) {
		t.Fatal("cell not sorted by key")
	}

	// Ops after a reshape in the same delta use the new index.
	v = mustApply(t, v, view.OpReshape(sh, []uint32{view.NoCell, 2, 2, 2, 0, 1}), view.OpSetCell(1, 0),
		view.OpAddMember(6, active(40, 2)))
	if v.Occupancy(0) != 2 || v.Occupancy(2) != 3 {
		t.Fatal("ops after reshape")
	}
}

func allParts() []uint32 {
	p := make([]uint32, 1<<testBits)
	for i := range p {
		p[i] = uint32(i)
	}

	return p
}

func fill(o uint32) []uint32 {
	p := make([]uint32, 1<<testBits)
	for i := range p {
		p[i] = o
	}

	return p
}

func TestAssign(t *testing.T) {
	v := boot(t, 3)
	mustFail(t, v, view.ErrInvalid, view.OpAssign([]uint32{1}, []uint32{7}))
	mustFail(t, v, view.ErrInvalid, view.OpAssign([]uint32{1, 1}, []uint32{1, 2}))
	mustFail(t, v, view.ErrInvalid, view.OpAssign([]uint32{1 << testBits}, []uint32{1}))
	mustFail(t, v, view.ErrInvalid, view.OpAssign([]uint32{1}, []uint32{1, 2}))
	mustFail(t, v, view.ErrInvalid, view.OpAssign([]uint32{1}, []uint32{view.NoSlot}))

	v = mustApply(t, v, view.OpEpoch(12, time.Second), view.OpAssign([]uint32{1, 2, 3}, []uint32{1, 2, 0}))
	if v.Load(0) != 1<<testBits-2 || v.Load(1) != 1 || v.Load(2) != 1 {
		t.Fatal("loads")
	}

	if e := v.Table().Get(1); e.Owner != 1 || e.Prev != 0 || e.Moved != 12 {
		t.Fatalf("entry %+v", e)
	}

	if e := v.Table().Get(3); e.Owner != 0 || e.Prev != view.NoMember {
		t.Fatalf("unchanged entry %+v", e)
	}
}

func TestEpochAndInvalidate(t *testing.T) {
	v := boot(t, 1)
	mustFail(t, v, view.ErrInvalid, view.OpEpoch(9, time.Second))
	mustFail(t, v, view.ErrInvalid, view.OpEpoch(11, 0))

	v = mustApply(t, v, view.OpEpoch(10, 2*time.Second))
	if v.Epoch() != 10 || v.Period() != 2*time.Second {
		t.Fatal("epoch")
	}

	mustFail(t, v, view.ErrInvalid, view.OpInvalidate(5, 0))
	v = mustApply(t, v, view.OpInvalidate(5, 1), view.OpInvalidate(3, 4), view.OpInvalidate(9, 1), view.OpInvalidate(5, 2))
	mustFail(t, v, view.ErrInvalid, view.OpInvalidate(5, 2))

	gens := v.Gens()
	if len(gens) != 3 || gens[0] != (view.KeyspaceGen{Seed: 3, Gen: 4}) || gens[1].Seed != 5 || v.Gen(5) != 2 {
		t.Fatalf("gens %+v", gens)
	}
}

func TestDigestChain(t *testing.T) {
	build := func(epoch uint64) *view.View {
		v := boot(t, 4)
		v = mustApply(t, v, view.OpEpoch(epoch, time.Second))

		return mustApply(t, v, view.OpSetState(2, view.Draining, 0))
	}

	a, b, c := build(11), build(11), build(12)
	if a.Digest() != b.Digest() || !view.Equal(a, b) {
		t.Fatal("same deltas gave different digests")
	}

	if a.Version() != c.Version() || a.Digest() == c.Digest() || view.Equal(a, c) {
		t.Fatal("different deltas gave the same digest")
	}

	// Fork: a delta sealed for a fails on c (same version, other digest).
	d := view.NewDelta(a, view.OpEpoch(20, time.Second))
	if _, err := view.Apply(c, d); !errors.Is(err, view.ErrDigest) {
		t.Fatalf("fork: %v", err)
	}

	if _, err := view.Apply(a, d); err != nil {
		t.Fatal(err)
	}
}

func TestCopyOnWrite(t *testing.T) {
	v := viewtest.Build(t, 2000, viewtest.Options{PartitionBits: 14})
	snap := view.EncodeSnapshot(v)

	for i := range 3 {
		d := viewtest.Delta(t, v, viewtest.Churn{Admit: 5, Remove: 5, Moves: 500, Epoch: i == 1})

		nv, err := view.Apply(v, d)
		if err != nil {
			t.Fatal(err)
		}

		if err := view.Validate(nv); err != nil {
			t.Fatal(err)
		}

		if string(view.EncodeSnapshot(v)) != string(snap) || view.Validate(v) != nil {
			t.Fatal("apply modified its base")
		}

		v, snap = nv, view.EncodeSnapshot(nv)
	}
}

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
		op.Weight = r.Uint32N(view.MaxWeight + 2)
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
		sh, err := view.NewShape(1 + r.Uint32N(3))
		if err != nil {
			g.t.Fatal(err)
		}

		cells := make([]uint32, int(cur.SlotCap())+r.IntN(3)-1)
		for i := range cells {
			cells[i] = r.Uint32N(sh.Cells() + 1)
			if r.IntN(3) == 0 {
				cells[i] = view.NoCell
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

			if i%10 == 0 {
				// Random shapes include dimensions of size 1 and 2 and
				// sparse cells.
				checkMonitoring(t, nv)
			}

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

// TestDeltaLogAheadAndFork checks that Since refuses a view ahead of the
// head and a view on a fork created by a different leader of the same term
// and seq.
func TestDeltaLogAheadAndFork(t *testing.T) {
	vs, ds := chain(t, 6)

	l := view.NewDeltaLog(8)
	l.Reset(vs[0])

	for _, d := range ds {
		if err := l.Append(d, nil); err != nil {
			t.Fatal(err)
		}
	}

	head := vs[len(vs)-1]

	ahead := mustApply(t, head, view.OpEpoch(head.Epoch()+1, time.Second))
	if _, ok := l.Since(ahead.Version(), ahead.Digest(), 0); ok {
		t.Fatal("served a view ahead of the head")
	}

	newTerm := mustApply(t, head, view.OpNewTerm(head.Term()+1, head.Leader()))
	if _, ok := l.Since(newTerm.Version(), newTerm.Digest(), 0); ok {
		t.Fatal("served a view in a later term")
	}

	// A fork off vs[2] with the same version as vs[3] but another digest.
	fork := mustApply(t, vs[2], view.OpEpoch(vs[2].Epoch()+5, time.Second))
	if fork.Version() != vs[3].Version() || fork.Digest() == vs[3].Digest() {
		t.Fatal("fork setup")
	}

	if _, ok := l.Since(fork.Version(), fork.Digest(), 0); ok {
		t.Fatal("served a forked view")
	}

	if got, ok := l.Since(vs[3].Version(), vs[3].Digest(), 0); !ok || len(got) != 3 {
		t.Fatalf("got %d entries, ok %v", len(got), ok)
	}
}

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
