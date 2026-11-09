// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"errors"
	"math/bits"
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
	"time"

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
					if m != d.Next.Slot && view.Better(view.Score(v.Key(m), uint64(p)), v.Key(m), view.Score(d.Next.Key, uint64(p)), d.Next.Key) {
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
				if m != owner && view.Better(view.Score(v.Key(m), uint64(p)), v.Key(m), view.Score(v.Key(best), uint64(p)), v.Key(best)) {
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

	v.Table().ForEach(func(p uint32, e view.Entry) {
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

	// 2 and 9: this member's epoch lags the one that stamped Moved=10; a
	// lag of up to HandoffEpochs still counts as within the window.
	for cur, want := range map[uint64]bool{1: false, 2: true, 9: true, 10: true, 17: true, 18: false, 1 << 33: false} {
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

	nv.Table().ForEach(func(q uint32, e view.Entry) {
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
	seed := view.Seed("ks")

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

	if out[0].Header.Partition != view.Partition(view.KeyHash(42, keys[0]), 12) {
		t.Fatal("partition hash")
	}

	if st.RouteHash(view.KeyHash(42, keys[1])) != out[1] {
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

	if st.Env().Health().Usable(4) != Healthy {
		t.Fatal("untracked not healthy")
	}
}

// TestStandInIgnoresOwnHealthView is a regression test: a backup that
// receives a Backup-flagged request serves it even when its own health view
// still finds the owner usable. It used to send the request back to the
// owner, which took D+1 hops and, with caller failovers to the next backup,
// bounced between the owner and its backups until the TTL ran out (found by
// the DST data-plane workload).
func TestStandInIgnoresOwnHealthView(t *testing.T) {
	v := smallView(t)
	senderTr := newTracker(0)
	sender := newFixture(v, newEnv(senderTr, 10))
	backups := newFixture(v, newEnv(newTracker(0), 10)) // sees everyone healthy
	self := view.Slot(5)
	p := findKind(t, sender.st[self], KindOwner)
	owner := v.Table().Owner(p)

	if v.Cell(owner) == v.Cell(self) {
		t.Skip("owner is a cell-mate")
	}

	eject(t, senderTr, v, owner)

	d := sender.st[self].RoutePartition(p)
	if d.Kind != KindBackup {
		t.Fatalf("%+v", d)
	}

	bd, err := backups.st[d.Next.Slot].Forward(d.Header)
	if err != nil || bd.Kind != KindSelf || bd.Owner.Slot != owner || bd.Header.Flags&FlagBackup == 0 {
		t.Fatalf("backup: %+v %v", bd, err)
	}

	// Without the flag the same member routes to the owner it finds healthy.
	h := d.Header
	h.Flags &^= FlagBackup

	if od, err := backups.st[d.Next.Slot].Forward(h); err != nil || od.Kind != KindOwner {
		t.Fatalf("unflagged: %+v %v", od, err)
	}
}

func TestAlternateHeader(t *testing.T) {
	v := smallView(t)
	f := newFixture(v, newEnv(newTracker(0), 10))
	self := view.Slot(5)

	d := f.st[self].RoutePartition(findKind(t, f.st[self], KindOwner))
	if d.Alternate.IsZero() {
		t.Skip("no alternate")
	}

	ah := d.AlternateHeader()
	if ah.Flags&FlagBackup == 0 || d.Header.Flags&FlagBackup != 0 {
		t.Fatalf("owner: %+v / %+v", ah, d.Header)
	}

	// The backup serves a failover from the owner hop.
	if bd, err := f.st[d.Alternate.Slot].Forward(ah); err != nil || bd.Kind != KindSelf {
		t.Fatalf("failover: %+v %v", bd, err)
	}

	r := f.st[self].RoutePartition(findKind(t, f.st[self], KindRelay))
	if r.AlternateHeader() != r.Header {
		t.Fatalf("relay alternate header changed: %+v", r.AlternateHeader())
	}
}

// TestFreshWithoutView is a regression test: a member with no view (just
// started, epoch counted from its own start) found every expiring stamp
// fresh, since its epoch was below any stamp's expiry (found by the DST
// data-plane workload).
func TestFreshWithoutView(t *testing.T) {
	env := newEnv(newTracker(0), 0)
	st := NewState(nil, view.NoSlot, env)
	seed := uint64(7)

	if st.Fresh(seed, Stamp{Expires: 25}) {
		t.Fatal("expiring stamp fresh without a view")
	}

	if !st.Fresh(seed, StampFor(0, 0, 0, 0)) {
		t.Fatal("never-expiring stamp must stay fresh")
	}
}

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

// TestInPlaceVariantsMatch checks that RouteTo, RouteHashTo, and ForwardTo
// produce exactly what Route, RouteHash, and Forward return, with and
// without filtered peers.
func TestInPlaceVariantsMatch(t *testing.T) {
	v := viewtest.Build(t, 400, viewtest.Options{PartitionBits: 12})

	for _, filtered := range []bool{false, true} {
		tr := newTracker(0)
		if filtered {
			eject(t, tr, v, 3, 50, 77)
			degrade(t, tr, v, 120)
		}

		st := NewState(v, 7, newEnv(tr, 1))

		for i := range 2000 {
			key := []byte("k" + strconv.Itoa(i))

			var d Decision

			st.RouteTo(&d, 9, key)

			if want := st.Route(9, key); d != want {
				t.Fatalf("RouteTo %q: %+v, want %+v", key, d, want)
			}

			var dh Decision

			h := view.KeyHash(9, key)
			st.RouteHashTo(&dh, h)

			if want := st.RouteHash(h); dh != want || dh != d {
				t.Fatalf("RouteHashTo %q: %+v, want %+v", key, dh, want)
			}

			in := d.Header
			in.Sender = 11

			var df Decision

			err := st.ForwardTo(&df, in)

			want, werr := st.Forward(in)
			if df != want || !errors.Is(err, werr) {
				t.Fatalf("ForwardTo %q: %+v %v, want %+v %v", key, df, err, want, werr)
			}
		}

		var d Decision

		if err := st.ForwardTo(&d, Header{}); err == nil || d != (Decision{}) {
			t.Fatalf("bad header: %v %+v", err, d)
		}
	}
}

// TestDiffMaskMatchesShape checks State.diffMask and the relay cell
// arithmetic against view.Shape.
func TestDiffMaskMatchesShape(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))

	for _, sizes := range [][]uint32{{7}, {5, 3}, {4, 6, 5}, {2, 3, 2, 5, 3}} {
		sh, err := view.NewShape(sizes...)
		if err != nil {
			t.Fatal(err)
		}

		for range 500 {
			self := rng.Uint32N(sh.Cells())
			st := &State{shape: &sh, selfCell: self}
			sh.Coords(&st.selfCoords, self)

			c := rng.Uint32N(sh.Cells())

			var oc [view.MaxDims]uint32

			if got, want := st.diffMask(&oc, c), sh.DiffMask(self, c); got != want {
				t.Fatalf("%v: diffMask(%d, %d) = %b, want %b", sizes, self, c, got, want)
			}

			for dim := range int(sh.Dims) {
				got := self + (oc[dim]-st.selfCoords[dim])*sh.Stride(dim)
				if want := sh.Replace(self, dim, sh.Coord(c, dim)); got != want {
					t.Fatalf("%v: replace dim %d: %d, want %d", sizes, dim, got, want)
				}
			}
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := Header{
		Format: HeaderFormat, Flags: FlagHedge | FlagBackup, Hops: 2, TTL: 6,
		Partition: 0xdeadbeef, Sender: 12345, Term: 7, Seq: 1<<40 + 3, Epoch: 1<<63 + 9,
	}

	b := h.Append([]byte("x"))
	if len(b) != 1+HeaderSize || b[0] != 'x' {
		t.Fatalf("len %d", len(b))
	}

	got, err := ParseHeader(b[1:])
	if err != nil || got != h {
		t.Fatalf("%+v %v", got, err)
	}

	var a [HeaderSize]byte

	h.Put(&a)

	if string(a[:]) != string(b[1:]) {
		t.Fatal("Put and Append differ")
	}

	// Big-endian partition at offset 4.
	if a[4] != 0xde || a[7] != 0xef {
		t.Fatalf("% x", a)
	}
}

func TestParseHeaderMalformed(t *testing.T) {
	good := Header{Format: HeaderFormat, TTL: 4}.Append(nil)

	for name, mut := range map[string]func([]byte) []byte{
		"short":  func(b []byte) []byte { return b[:HeaderSize-1] },
		"empty":  func([]byte) []byte { return nil },
		"format": func(b []byte) []byte { b[0] = 0; return b },
		"flags":  func(b []byte) []byte { b[1] = 0x40; return b },
		"hops":   func(b []byte) []byte { b[2] = 5; return b },
	} {
		b := mut(append([]byte(nil), good...))
		if _, err := ParseHeader(b); !errors.Is(err, ErrBadHeader) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func FuzzParseHeader(f *testing.F) {
	f.Add(Header{Format: HeaderFormat, TTL: 4, Partition: 3}.Append(nil))
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParseHeader(b)
		if err != nil {
			return
		}

		if h.Hops > h.TTL || h.Format != HeaderFormat {
			t.Fatalf("accepted %+v", h)
		}

		if got, err := ParseHeader(h.Append(nil)); err != nil || got != h {
			t.Fatalf("round trip %+v %v", got, err)
		}

		if string(h.Append(nil)) != string(b[:HeaderSize]) {
			t.Fatal("not canonical")
		}
	})
}

// propertyWalk walks every origin in origins to sampled partitions and
// returns the hop count histogram.
func propertyWalk(t *testing.T, f *fixture, origins []view.Slot, parts int, rng *rand.Rand, maxHops int) []int {
	t.Helper()

	hist := make([]int, 16)
	bits := f.v.PartitionBits()

	for _, o := range origins {
		for range parts {
			p := rng.Uint32() >> (32 - bits)
			path, d := walk(t, f, o, p)

			if d.Kind != KindSelf {
				t.Fatalf("ended with %v", d.Kind)
			}

			last := o
			if len(path) > 0 {
				last = path[len(path)-1]
			}

			owner := f.v.Table().Owner(p)
			if last != owner && d.Header.Flags&FlagBackup == 0 {
				t.Fatalf("ended at %d, owner %d", last, owner)
			}

			if len(path) > maxHops {
				t.Fatalf("%d hops > %d: %v", len(path), maxHops, path)
			}

			hist[len(path)]++
		}
	}

	return hist
}

func testProperty(t *testing.T, n, originStep int) {
	t.Helper()

	v := viewtest.Build(t, n, viewtest.Options{Seed: uint64(n)})
	d := int(v.Shape().Dims)
	rng := rand.New(rand.NewPCG(uint64(n), 7))

	var origins []view.Slot
	for o := 0; o < n; o += originStep {
		origins = append(origins, view.Slot(o))
	}

	f := newFixture(v, newEnv(nil, 1))
	hist := propertyWalk(t, f, origins, 4, rng, d)
	t.Logf("n=%d D=%d healthy hops: %v", n, d, hist[:d+2])

	// Random unusable members (10%, at most 2000 to keep tracking cheap):
	// loop-free and within TTL.
	tr := newTracker(0)
	bad := make([]view.Slot, 0, min(n/10, 2000))

	for _, s := range rng.Perm(n)[:cap(bad)] {
		bad = append(bad, view.Slot(s))
	}

	eject(t, tr, v, bad...)

	f = newFixture(v, newEnv(tr, 1))
	hist = propertyWalk(t, f, origins, 4, rng, d+2)
	t.Logf("n=%d D=%d with %d unusable hops: %v", n, d, len(bad), hist[:d+3])
}

func TestPropertyHops10k(t *testing.T) {
	testProperty(t, 10_000, 1)
}

func TestPropertyHops100k(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}

	testProperty(t, 100_000, 10)
}

var (
	fuzzOnce sync.Once
	fuzzFix  *fixture
)

// FuzzForward feeds arbitrary headers to Forward on every member of a small
// view: it must never panic, accept only well-formed in-range headers, never
// route to itself, and keep the hop accounting consistent.
func FuzzForward(f *testing.F) {
	f.Add(Header{Format: HeaderFormat, TTL: 4, Partition: 3, Sender: 1}.Append(nil), uint16(0))
	f.Add(Header{Format: HeaderFormat, Flags: FlagBackup, Hops: 3, TTL: 4, Partition: 99, Sender: 7, Term: 9, Seq: 1 << 40}.Append(nil), uint16(5))
	f.Fuzz(func(t *testing.T, b []byte, self uint16) {
		fuzzOnce.Do(func() {
			v := viewtest.Build(t, 200, viewtest.Options{PartitionBits: 12, MaxNeighbors: 30})
			tr := newTracker(0)
			eject(t, tr, v, 3, 50, 77)
			fuzzFix = newFixture(v, newEnv(tr, 1))
		})

		h, err := ParseHeader(b)
		if err != nil {
			return
		}

		st := fuzzFix.st[int(self)%len(fuzzFix.st)]

		d, err := st.Forward(h)
		if err != nil {
			if !errors.Is(err, ErrBadHeader) && !errors.Is(err, ErrTTLExhausted) {
				t.Fatalf("unexpected error %v", err)
			}

			if errors.Is(err, ErrBadHeader) && h.Partition < 1<<12 {
				t.Fatalf("in-range header rejected: %+v", h)
			}

			return
		}

		if h.Partition >= 1<<12 {
			t.Fatalf("out-of-range partition accepted: %+v", h)
		}

		if !d.Next.IsZero() && d.Next.Slot == st.Self() {
			t.Fatalf("routed to self: %+v", d)
		}

		if d.Header.Hops != h.Hops+1 || d.Header.TTL != h.TTL || d.Header.Flags&FlagHedge != h.Flags&FlagHedge {
			t.Fatalf("header %+v from %+v", d.Header, h)
		}

		if !d.Next.IsZero() && d.Next.Slot == h.Sender && d.Next.Slot != d.Owner.Slot && h.Sender != view.NoSlot {
			t.Fatalf("sent back to sender %d: %+v", h.Sender, d)
		}
	})
}

// fakeCell groups every peer a test ejects into one health cell together
// with a never-ejected dummy, so the tracker's last-usable-in-cell rule does
// not get in the way.
const (
	fakeCell  = 1 << 30
	dummySlot = 1 << 30
)

// newTracker returns a tracker that can eject any number of peers and
// never panics unless panicAt is set.
func newTracker(panicAt int) *HealthTracker {
	if panicAt <= 0 {
		panicAt = 100
	}

	tr := NewHealthTracker(HealthConfig{MaxEjectionPercent: 100, PanicThreshold: panicAt}, rand.New(rand.NewPCG(1, 2)))
	tr.Track(dummySlot, 1, fakeCell)

	return tr
}

func eject(t testing.TB, tr *HealthTracker, v *view.View, slots ...view.Slot) {
	t.Helper()

	for _, s := range slots {
		tr.Track(s, v.Key(s), fakeCell)
	}

	for _, s := range slots {
		for range 5 {
			tr.Observe(s, Failure, time.Millisecond)
		}

		if tr.Usable(s) != Unusable {
			t.Fatalf("slot %d not ejected", s)
		}
	}
}

func degrade(t testing.TB, tr *HealthTracker, v *view.View, s view.Slot) {
	t.Helper()
	tr.Track(s, v.Key(s), fakeCell)
	tr.Observe(s, Overload, 0)

	if tr.Usable(s) != Degraded {
		t.Fatalf("slot %d not degraded", s)
	}
}

type fixture struct {
	v   *view.View
	env *Env
	st  []*State
}

func newFixture(v *view.View, env *Env) *fixture {
	f := &fixture{v: v, env: env, st: make([]*State, v.SlotCap())}
	for i := range f.st {
		f.st[i] = NewState(v, view.Slot(i), env)
	}

	return f
}

func newEnv(tr *HealthTracker, cur uint64) *Env {
	e := &EpochTracker{}
	e.Anchor(cur, 15*time.Second, 0)

	return NewEnv(Config{Health: tr, Epoch: e})
}

// findKind returns a partition for which st routes with kind k.
func findKind(t testing.TB, st *State, k Kind) uint32 {
	t.Helper()

	n := uint32(1) << st.PartitionBits()
	for p := range n {
		if st.RoutePartition(p).Kind == k {
			return p
		}
	}

	t.Fatalf("no partition with kind %v", k)

	return 0
}

func diffDims(v *view.View, a, b view.Slot) int {
	return bits.OnesCount8(v.Shape().DiffMask(v.Cell(a), v.Cell(b)))
}

// walk follows a request for partition p from origin hop by hop. It returns
// the slots visited after the origin and the final decision.
func walk(t testing.TB, f *fixture, origin view.Slot, p uint32) ([]view.Slot, Decision) {
	t.Helper()

	d := f.st[origin].RoutePartition(p)

	var path []view.Slot

	for !d.Next.IsZero() {
		next := d.Next.Slot
		for _, s := range path {
			if s == next {
				t.Fatalf("loop at %d for p=%d from %d: %v", next, p, origin, path)
			}
		}

		if next == origin {
			t.Fatalf("routed back to origin %d", origin)
		}

		path = append(path, next)

		var err error

		d, err = f.st[next].Forward(d.Header)
		if err != nil {
			t.Fatalf("forward at %d (p=%d from %d, path %v): %v", next, p, origin, path, err)
		}
	}

	return path, d
}

var (
	benchOnce sync.Once
	benchView *view.View
)

// big returns a shared 100k-member view with 2^20 partitions.
func big(b *testing.B) *view.View {
	b.Helper()
	benchOnce.Do(func() { benchView = viewtest.Build(b, 100_000, viewtest.Options{}) })

	return benchView
}

func BenchmarkRoute(b *testing.B) {
	v := big(b)
	tr := newTracker(0)
	env := newEnv(tr, 1)
	self := view.Slot(12345)
	st := NewState(v, self, env)

	// Pick a partition per kind; the backup case ejects an adjacent owner.
	parts := map[Kind]uint32{}
	for _, k := range []Kind{KindSelf, KindOwner, KindRelay} {
		parts[k] = findKind(b, st, k)
	}

	for p := range uint32(1) << 20 {
		o := v.Table().Owner(p)
		if o != self && v.Cell(o) != v.Cell(self) && diffDims(v, self, o) == 1 && o != v.Table().Owner(parts[KindOwner]) {
			eject(b, tr, v, o)

			parts[KindBackup] = p

			break
		}
	}

	for _, k := range []Kind{KindSelf, KindOwner, KindRelay, KindBackup} {
		p := parts[k]
		if got := st.RoutePartition(p).Kind; got != k {
			b.Fatalf("%v: got %v", k, got)
		}

		// Find a key that hashes to the partition's kind, to include
		// hashing in the measurement.
		var key [8]byte

		seed := uint64(7)

		for i := uint64(0); ; i++ {
			key = [8]byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24), byte(i >> 32)}
			if st.Route(seed, key[:]).Kind == k {
				break
			}
		}

		b.Run("Route/"+k.String(), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_ = st.Route(seed, key[:])
			}
		})

		d := st.RoutePartition(p)
		if d.Next.IsZero() {
			continue
		}

		next := NewState(v, d.Next.Slot, env)
		h := d.Header

		b.Run("Forward/"+k.String(), func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				_, _ = next.Forward(h)
			}
		})
	}
}

func BenchmarkFresh(b *testing.B) {
	v := big(b)
	st := NewState(v, 1, newEnv(nil, 1))
	seed := view.Seed("ks")
	s := st.Stamp(seed, 0)

	b.ReportAllocs()

	for b.Loop() {
		if !st.Fresh(seed, s) {
			b.Fatal("stale")
		}
	}
}

func BenchmarkNewState(b *testing.B) {
	v := big(b)
	env := newEnv(nil, 1)

	b.ReportAllocs()

	for b.Loop() {
		_ = NewState(v, 777, env)
	}
}

func BenchmarkRouteParallel(b *testing.B) {
	v := big(b)
	st := NewState(v, 12345, newEnv(nil, 1))

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var k [8]byte

		i := uint64(0)
		for pb.Next() {
			i++
			k[0], k[1], k[2] = byte(i), byte(i>>8), byte(i>>16)
			_ = st.Route(3, k[:])
		}
	})
}

// NoteHeader records h as if it had been received by a member whose view
// is at version own.
func (n *Notes) NoteHeader(h Header, own view.Version) {
	n.note(&h, uint32(own.Term), own.Seq)
}

// Peek returns the recorded note without clearing it.
func (n *Notes) Peek() (Note, bool) {
	if p := n.p.Load(); p != nil {
		return *p, true
	}

	return Note{}, false
}
