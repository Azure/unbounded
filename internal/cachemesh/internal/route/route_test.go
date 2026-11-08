// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/health"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

func smallView(t testing.TB) *view.View {
	t.Helper()

	v := viewtest.Build(t, 512, viewtest.Options{PartitionBits: 12, MaxNeighbors: 40, Epoch: 10})
	if v.Shape().Dims < 3 {
		t.Fatalf("want at least 3 dims, got %d", v.Shape().Dims)
	}

	return v
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{KindNoRoute: "NoRoute", KindSelf: "Self", KindDirect: "Direct", 99: "Kind(99)"} {
		if got := k.String(); got != want {
			t.Errorf("%d: %q want %q", k, got, want)
		}
	}
}

func TestNoRoute(t *testing.T) {
	for name, v := range map[string]*view.View{
		"nil":     nil,
		"genesis": view.Genesis(),
		"empty":   viewtest.Build(t, 0, viewtest.Options{PartitionBits: 8}),
	} {
		st := NewState(v, 0, nil)
		if st.Member() || st.Self() != view.NoSlot {
			t.Errorf("%s: member", name)
		}

		if d := st.Route(1, []byte("k")); d.Kind != KindNoRoute || !d.Next.IsZero() {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

func TestSingleMember(t *testing.T) {
	v := viewtest.Build(t, 1, viewtest.Options{PartitionBits: 8})

	st := NewState(v, 0, nil)
	for i := range 100 {
		d := st.Route(7, []byte{byte(i)})
		if d.Kind != KindSelf || d.Owner.Slot != 0 || !d.Next.IsZero() || d.Header.Sender != 0 {
			t.Fatalf("%+v", d)
		}

		if d.Header.TTL != v.Shape().Dims+2 || d.Header.Format != HeaderFormat {
			t.Fatalf("header %+v", d.Header)
		}
	}
}

func TestRulesHealthy(t *testing.T) {
	v := smallView(t)
	f := newFixture(v, newEnv(nil, 10))
	counts := map[Kind]int{}

	for _, self := range []view.Slot{0, 17, 300, 511} {
		st := f.st[self]
		for p := range uint32(1) << 12 {
			d := st.RoutePartition(p)
			counts[d.Kind]++
			owner := v.Table().Owner(p)

			if d.Owner.Slot != owner || d.Owner.ID == "" {
				t.Fatalf("owner %+v want %d", d.Owner, owner)
			}

			if d.Next.Slot == self && !d.Next.IsZero() {
				t.Fatal("next is self")
			}

			switch n := diffDims(v, self, owner); {
			case owner == self:
				if d.Kind != KindSelf {
					t.Fatalf("p=%d: %v want Self", p, d.Kind)
				}
			case n <= 1:
				if d.Kind != KindOwner || d.Next.Slot != owner {
					t.Fatalf("p=%d: %v want Owner", p, d.Kind)
				}

				if !d.Alternate.IsZero() && (v.Cell(d.Alternate.Slot) != v.Cell(owner) || d.Alternate.Slot == self || d.Alternate.Slot == owner) {
					t.Fatalf("bad alternate %+v", d.Alternate)
				}

				if d.HedgeAfter <= 0 {
					t.Fatal("no hedge delay")
				}
			default:
				if d.Kind != KindRelay {
					t.Fatalf("p=%d: %v want Relay", p, d.Kind)
				}
				// The relay is in self's cell with the lowest differing
				// coordinate fixed, and is the best scorer there.
				sh := v.Shape()
				dim := firstDiff(v, self, owner)
				want := sh.Replace(v.Cell(self), dim, sh.Coord(v.Cell(owner), dim))

				if v.Cell(d.Next.Slot) != want {
					t.Fatalf("relay cell %d want %d", v.Cell(d.Next.Slot), want)
				}

				for _, m := range v.CellMembers(want) {
					if m != d.Next.Slot && hashing.Better(hashing.Score(v.Key(m), uint64(p)), v.Key(m), hashing.Score(d.Next.Key, uint64(p)), d.Next.Key) {
						t.Fatalf("relay %d is not the best of its cell", d.Next.Slot)
					}
				}

				if d.Alternate.IsZero() || d.Alternate.Slot == d.Next.Slot {
					t.Fatalf("bad alternate %+v", d.Alternate)
				}
			}
		}
	}

	for _, k := range []Kind{KindSelf, KindOwner, KindRelay} {
		if counts[k] == 0 {
			t.Errorf("no %v decisions", k)
		}
	}
}

func firstDiff(v *view.View, a, b view.Slot) int {
	m := v.Shape().DiffMask(v.Cell(a), v.Cell(b))
	for i := range 8 {
		if m&(1<<i) != 0 {
			return i
		}
	}

	return -1
}

func TestJoiningDrainingOwnAndRelay(t *testing.T) {
	v := viewtest.Build(t, 256, viewtest.Options{PartitionBits: 10, MaxNeighbors: 40, Joining: 40, Draining: 40})
	f := newFixture(v, newEnv(nil, 1))

	for o := range view.Slot(256) {
		for p := uint32(0); p < 1024; p += 37 {
			path, d := walk(t, f, o, p)
			if d.Kind != KindSelf || (len(path) > 0 && path[len(path)-1] != v.Table().Owner(p)) {
				t.Fatalf("did not reach owner: %v %v", path, d.Kind)
			}
		}
	}
}

func TestOwnerUnusableBackup(t *testing.T) {
	v := smallView(t)
	tr := newTracker(0)
	f := newFixture(v, newEnv(tr, 10))
	self := view.Slot(5)
	st := f.st[self]
	p := findKind(t, st, KindOwner)
	owner := v.Table().Owner(p)

	if v.Cell(owner) == v.Cell(self) {
		t.Skip("owner is a cell-mate")
	}

	eject(t, tr, v, owner)

	d := st.RoutePartition(p)
	if d.Kind != KindBackup || d.Next.Slot == owner || v.Cell(d.Next.Slot) != v.Cell(owner) || d.Header.Flags&FlagBackup == 0 {
		t.Fatalf("%+v", d)
	}

	if !d.PrevOwner.IsZero() {
		t.Fatal("prev owner on a backup hop")
	}

	// The backup serves the request itself.
	bd, err := f.st[d.Next.Slot].Forward(d.Header)
	if err != nil || bd.Kind != KindSelf || bd.Owner.Slot != owner || bd.Header.Flags&FlagBackup == 0 {
		t.Fatalf("backup: %+v %v", bd, err)
	}

	// With every backup unusable as well, health is ignored.
	var mates []view.Slot

	for _, m := range v.CellMembers(v.Cell(owner)) {
		if m != owner {
			mates = append(mates, m)
		}
	}

	eject(t, tr, v, mates...)

	if d := st.RoutePartition(p); d.Kind != KindOwner || d.Next.Slot != owner {
		t.Fatalf("all unusable: %+v", d)
	}
}

func TestOwnerDegraded(t *testing.T) {
	v := smallView(t)
	tr := newTracker(0)
	f := newFixture(v, newEnv(tr, 10))
	self := view.Slot(5)
	p := findKind(t, f.st[self], KindOwner)
	owner := v.Table().Owner(p)

	if v.Cell(owner) == v.Cell(self) {
		t.Skip("owner is a cell-mate")
	}

	degrade(t, tr, v, owner)

	d := f.st[self].RoutePartition(p)
	if d.Kind != KindBackup || d.Alternate.Slot != owner && len(v.CellMembers(v.Cell(owner))) <= 2 {
		t.Fatalf("%+v", d)
	}

	// Degraded backups do not beat a degraded owner.
	for _, m := range v.CellMembers(v.Cell(owner)) {
		if m != owner {
			degrade(t, tr, v, m)
		}
	}

	if d := f.st[self].RoutePartition(p); d.Kind != KindOwner {
		t.Fatalf("%+v", d)
	}
}

func TestCellMateStandIn(t *testing.T) {
	v := smallView(t)
	tr := newTracker(0)
	f := newFixture(v, newEnv(tr, 10))

	// Find a self whose cell-mate owns a partition for which self is the
	// best backup.
	for self := range view.Slot(512) {
		st := f.st[self]

		for p := range uint32(1) << 12 {
			owner := v.Table().Owner(p)
			if owner == self || v.Cell(owner) != v.Cell(self) {
				continue
			}

			best := self
			for _, m := range v.CellMembers(v.Cell(owner)) {
				if m != owner && hashing.Better(hashing.Score(v.Key(m), uint64(p)), v.Key(m), hashing.Score(v.Key(best), uint64(p)), v.Key(best)) {
					best = m
				}
			}

			if best != self {
				continue
			}

			eject(t, tr, v, owner)

			d := st.RoutePartition(p)
			if d.Kind != KindSelf || d.Owner.Slot != owner || d.Header.Flags&FlagBackup == 0 {
				t.Fatalf("%+v", d)
			}

			return
		}
	}

	t.Fatal("no stand-in case found")
}

func TestRelayCellUnusable(t *testing.T) {
	v := smallView(t)
	tr := newTracker(0)
	f := newFixture(v, newEnv(tr, 10))
	self := view.Slot(3)
	st := f.st[self]
	sh := v.Shape()

	// Find a relay partition whose owner differs in at least two dims.
	p := findKind(t, st, KindRelay)
	owner := v.Table().Owner(p)
	mask := sh.DiffMask(v.Cell(self), v.Cell(owner))

	var cells []uint32

	for i := range 8 {
		if mask&(1<<i) != 0 {
			cells = append(cells, sh.Replace(v.Cell(self), i, sh.Coord(v.Cell(owner), i)))
		}
	}

	eject(t, tr, v, v.CellMembers(cells[0])...)

	d := st.RoutePartition(p)
	if d.Kind != KindRelay || v.Cell(d.Next.Slot) != cells[1] {
		t.Fatalf("want relay in cell %d: %+v (cell %d)", cells[1], d, v.Cell(d.Next.Slot))
	}

	for _, c := range cells[1:] {
		eject(t, tr, v, v.CellMembers(c)...)
	}

	// Every candidate is unusable: health is ignored, first cell wins.
	d = st.RoutePartition(p)
	if d.Kind != KindRelay || v.Cell(d.Next.Slot) != cells[0] {
		t.Fatalf("panic fallback: %+v", d)
	}
}

func TestPanicMode(t *testing.T) {
	v := smallView(t)
	tr := newTracker(40) // dummy + one ejected peer = 50%
	f := newFixture(v, newEnv(tr, 10))
	st := f.st[3]
	p := findKind(t, st, KindRelay)
	healthy := st.RoutePartition(p)

	eject(t, tr, v, healthy.Next.Slot)
	tr.Tick(1)

	if !tr.Panic() {
		t.Fatal("tracker not in panic")
	}

	if d := st.RoutePartition(p); d.Next != healthy.Next {
		t.Fatalf("panic should ignore health: %+v", d)
	}
}

// removeCells removes every member of cells, giving their partitions to
// heir first.
func removeCells(t testing.TB, v *view.View, heir view.Slot, cells ...uint32) *view.View {
	t.Helper()

	gone := map[view.Slot]bool{}

	var ops []view.Op

	for _, c := range cells {
		for _, m := range v.CellMembers(c) {
			gone[m] = true
		}
	}

	var parts, owners []uint32

	v.Table().ForEach(func(p uint32, e placement.Entry) {
		if gone[e.Owner] {
			parts = append(parts, p)
			owners = append(owners, heir)
		}
	})

	if len(parts) > 0 {
		ops = append(ops, view.OpAssign(parts, owners))
	}

	for _, c := range cells {
		for _, m := range v.CellMembers(c) {
			ops = append(ops, view.OpRemoveMember(m))
		}
	}

	nv, err := view.Apply(v, view.NewDelta(v, ops...))
	if err != nil {
		t.Fatal(err)
	}

	return nv
}

func TestDirectWhenCellsEmpty(t *testing.T) {
	v := smallView(t)
	self := view.Slot(3)
	sh := v.Shape()

	for p := range uint32(1) << 12 {
		owner := v.Table().Owner(p)
		mask := sh.DiffMask(v.Cell(self), v.Cell(owner))

		if mask&(mask-1) == 0 {
			continue
		}

		var cells []uint32

		for i := range 8 {
			if mask&(1<<i) != 0 {
				cells = append(cells, sh.Replace(v.Cell(self), i, sh.Coord(v.Cell(owner), i)))
			}
		}

		nv := removeCells(t, v, owner, cells...)
		st := NewState(nv, self, nil)

		d := st.RoutePartition(p)
		if d.Kind != KindDirect || d.Next.Slot != owner || d.Header.Flags&FlagDirect == 0 {
			t.Fatalf("%+v", d)
		}

		return
	}

	t.Fatal("no relay partition")
}

func TestHandoffWindow(t *testing.T) {
	v := smallView(t) // epoch 10
	p := uint32(77)
	old := v.Table().Owner(p)
	nu := (old + 1) % 512

	nv, err := view.Apply(v, view.NewDelta(v, view.OpAssign([]uint32{p}, []uint32{nu})))
	if err != nil {
		t.Fatal(err)
	}

	if e := nv.Table().Get(p); e.Prev != old || e.Moved != 10 {
		t.Fatalf("entry %+v", e)
	}

	for cur, want := range map[uint64]bool{10: true, 17: true, 18: false, 1 << 33: false} {
		st := NewState(nv, nu, newEnv(nil, cur))

		d := st.RoutePartition(p)
		if d.Kind != KindSelf {
			t.Fatalf("kind %v", d.Kind)
		}

		if got := d.PrevOwner.Slot == old && !d.PrevOwner.IsZero(); got != want {
			t.Errorf("epoch %d: prev %+v want %v", cur, d.PrevOwner, want)
		}

		// A member routing to the new owner also sees it when the hop
		// is final.
		for s := range view.Slot(512) {
			ds := NewState(nv, s, newEnv(nil, cur)).RoutePartition(p)
			if ds.Kind == KindOwner || ds.Kind == KindDirect {
				if got := !ds.PrevOwner.IsZero(); got != want {
					t.Errorf("epoch %d from %d: prev %+v", cur, s, ds.PrevOwner)
				}

				break
			}
		}
	}

	// Moved at a wrapped 32-bit epoch still compares by difference.
	wrap := uint64(1)<<32 + 12
	if d := NewState(nv, nu, newEnv(nil, wrap)).RoutePartition(p); d.PrevOwner.IsZero() {
		t.Error("wrapped epoch within the window should report the previous owner")
	}

	// A removed previous owner is ignored.
	heir := (old + 2) % 512
	if heir == nu {
		heir = (old + 3) % 512
	}

	var parts, owners []uint32

	nv.Table().ForEach(func(q uint32, e placement.Entry) {
		if e.Owner == old {
			parts = append(parts, q)
			owners = append(owners, heir)
		}
	})

	ops := []view.Op{view.OpRemoveMember(old)}
	if len(parts) > 0 {
		ops = append([]view.Op{view.OpAssign(parts, owners)}, ops...)
	}

	if old == 0 {
		t.Skip("leader cannot be removed")
	}

	rv, err := view.Apply(nv, view.NewDelta(nv, ops...))
	if err != nil {
		t.Fatal(err)
	}

	if d := NewState(rv, nu, newEnv(nil, 11)).RoutePartition(p); !d.PrevOwner.IsZero() {
		t.Errorf("removed prev owner reported: %+v", d.PrevOwner)
	}
}

func TestGateway(t *testing.T) {
	v := smallView(t)
	gws := []PeerRef{{ID: "g1", Addr: "g1:1"}, {ID: "g2", Addr: "g2:1"}, {ID: "g3", Addr: "g3:1"}, {}}
	env := NewEnv(Config{Gateways: gws})
	seen := map[string]int{}

	for _, vv := range []*view.View{nil, v} {
		st := NewState(vv, view.NoSlot, env)
		if st.Member() {
			t.Fatal("member")
		}

		for i := range 300 {
			d := st.Route(1, []byte{byte(i), 1})
			if d.Kind != KindGateway || d.Next.IsZero() || d.Alternate.IsZero() || d.Next == d.Alternate ||
				d.Header.Sender != view.NoSlot || d.Header.Flags&FlagGateway == 0 {
				t.Fatalf("%+v", d)
			}

			seen[d.Next.ID]++

			// Deterministic.
			if d2 := st.Route(1, []byte{byte(i), 1}); d2.Next != d.Next {
				t.Fatal("not deterministic")
			}
		}
	}

	if len(seen) != 3 {
		t.Errorf("gateways used: %v", seen)
	}

	// No gateways with a view: direct to the owner.
	st := NewState(v, view.NoSlot, nil)
	if d := st.RoutePartition(5); d.Kind != KindDirect || d.Next.Slot != v.Table().Owner(5) {
		t.Fatalf("%+v", d)
	}
}

func TestSlotReuse(t *testing.T) {
	v := smallView(t)
	m, _ := v.Member(7)

	if !NewState(v, 7, NewEnv(Config{SelfID: m.ID, SelfIncarnation: m.Incarnation})).Member() {
		t.Fatal("not a member")
	}

	for _, c := range []Config{{SelfID: "other"}, {SelfID: m.ID, SelfIncarnation: m.Incarnation + 1}} {
		st := NewState(v, 7, NewEnv(c))
		if st.Member() || st.Self() != view.NoSlot {
			t.Fatalf("%+v: reused slot taken as self", c)
		}
	}
}

func TestForward(t *testing.T) {
	v := smallView(t)
	f := newFixture(v, newEnv(nil, 10))
	st := f.st[3]
	p := findKind(t, st, KindRelay)
	d := st.RoutePartition(p)
	relay := f.st[d.Next.Slot]

	// TTL exhausted at a relay that must forward.
	h := d.Header
	h.Hops = h.TTL - 1

	if _, err := relay.Forward(h); !errors.Is(err, ErrTTLExhausted) {
		t.Fatalf("err %v", err)
	}

	// The owner serves regardless of TTL.
	h.Hops = h.TTL
	if od, err := f.st[d.Owner.Slot].Forward(h); err != nil || od.Kind != KindSelf {
		t.Fatalf("%+v %v", od, err)
	}

	// Malformed headers.
	for _, bad := range []Header{
		{Format: 2, TTL: 5},
		{Format: HeaderFormat, Flags: 0x80, TTL: 5},
		{Format: HeaderFormat, Hops: 6, TTL: 5},
		{Format: HeaderFormat, TTL: 5, Partition: 1 << 12},
	} {
		if _, err := relay.Forward(bad); !errors.Is(err, ErrBadHeader) {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	// Hops increment, the hedge flag is kept, sender becomes the relay.
	h = d.Header
	h.Flags |= FlagHedge

	rd, err := relay.Forward(h)
	if err != nil || rd.Header.Hops != 1 || rd.Header.Flags&FlagHedge == 0 || rd.Header.Sender != d.Next.Slot || rd.Header.TTL != h.TTL {
		t.Fatalf("%+v %v", rd.Header, err)
	}

	// Never back to the sender: claim the relay's best choice sent it.
	if !rd.Next.IsZero() && rd.Next.Slot != d.Owner.Slot {
		h2 := d.Header
		h2.Sender = rd.Next.Slot

		rd2, err := relay.Forward(h2)
		if err != nil || rd2.Next.Slot == rd.Next.Slot {
			t.Fatalf("routed back to sender: %+v %v", rd2, err)
		}
	}
}

func TestNotes(t *testing.T) {
	v := smallView(t)
	env := newEnv(nil, 10)
	st := NewState(v, 1, env)
	own := v.Version()

	h := Header{Format: HeaderFormat, TTL: 6, Sender: 9, Term: uint32(own.Term), Seq: own.Seq}
	if _, err := st.Forward(h); err != nil {
		t.Fatal(err)
	}

	if _, ok := env.Notes().Peek(); ok {
		t.Fatal("noted a non-ahead header")
	}

	h.Seq += 5
	h.Epoch = 99
	_, _ = st.Forward(h)
	h.Seq -= 2
	h.Sender = 10
	_, _ = st.Forward(h)

	n, ok := env.Notes().Take()
	if !ok || n.Sender != 9 || n.Seq != own.Seq+5 || n.Epoch != 99 {
		t.Fatalf("%+v %v", n, ok)
	}

	if _, ok := env.Notes().Take(); ok {
		t.Fatal("take did not clear")
	}

	// A newer term wins over a larger seq.
	var ns Notes

	ns.NoteHeader(Header{Sender: 1, Term: 1, Seq: 100}, view.Version{})
	ns.NoteHeader(Header{Sender: 2, Term: 2, Seq: 1}, view.Version{})
	ns.NoteHeader(Header{Sender: 3, Term: 1, Seq: 200}, view.Version{})
	ns.NoteHeader(Header{Sender: view.NoSlot, Term: 9, Seq: 1}, view.Version{})

	if n, _ := ns.Take(); n.Sender != 2 {
		t.Fatalf("%+v", n)
	}
}

func TestStampFresh(t *testing.T) {
	v := smallView(t)
	env := newEnv(nil, 10)
	st := NewState(v, 1, env)
	seed := hashing.Seed("ks")

	s := st.Stamp(seed, 0)
	if !st.Fresh(seed, s) {
		t.Fatal("never-expiring stamp stale")
	}

	s = st.Stamp(seed, 300e9) // 20 periods
	if s.Expires != 10+20+1 || !st.Fresh(seed, s) {
		t.Fatalf("%+v", s)
	}

	env.Epoch().Advance(int64(21) * 15e9)

	if st.Fresh(seed, s) {
		t.Fatal("expired stamp fresh")
	}

	nv, err := view.Apply(v, view.NewDelta(v, view.OpInvalidate(seed, 1)))
	if err != nil {
		t.Fatal(err)
	}

	if st2 := NewState(nv, 1, env); st2.Fresh(seed, st.Stamp(seed, 0)) {
		t.Fatal("old generation fresh")
	}
}

func TestRouteManyAndHedge(t *testing.T) {
	v := smallView(t)
	st := NewState(v, 3, newEnv(nil, 10))
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	out := make([]Decision, 4)

	st.RouteMany(42, keys, out)

	for i, k := range keys {
		if out[i] != st.Route(42, k) {
			t.Fatalf("%d differs", i)
		}

		if out[i].Header.Partition != st.Partition(42, k) {
			t.Fatal("partition")
		}
	}

	if out[0].Header.Partition != hashing.Partition(hashing.KeyHash(42, keys[0]), 12) {
		t.Fatal("partition hash")
	}

	if st.RouteHash(hashing.KeyHash(42, keys[1])) != out[1] {
		t.Fatal("route hash")
	}

	// Budget: 5% of routed requests per shard.
	h := Header{Partition: 0}

	for range 100 {
		st.RoutePartition(0)
	}

	n := 0

	for range 100 {
		if st.TryHedge(h) {
			n++
		}
	}

	if n == 0 || n > 6 {
		t.Fatalf("hedges admitted: %d", n)
	}

	if _, ok := st.Peer(view.Slot(9999)); ok {
		t.Fatal("peer out of range")
	}

	if p, ok := st.Peer(4); !ok || p.Slot != 4 || p.Addr == "" {
		t.Fatalf("%+v", p)
	}

	if st.Env().Health().Usable(4) != health.Healthy {
		t.Fatal("untracked not healthy")
	}
}
