// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"errors"
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
