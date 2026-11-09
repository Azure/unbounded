// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

// leaderHarness applies the leader's deltas to a view on a simulated clock.
type leaderHarness struct {
	tb   testing.TB
	l    *Leader
	v    *view.View
	now  int64
	term uint64
}

func smallCfg(id string) LeaderConfig {
	return LeaderConfig{Self: Joiner{ID: id, Incarnation: 1, Addr: id + ":1"}, PartitionBits: 10}
}

func newLeaderHarness(tb testing.TB, cfg LeaderConfig, v *view.View) *leaderHarness {
	tb.Helper()

	if v == nil {
		v = view.Genesis()
	}

	return &leaderHarness{tb: tb, l: NewLeader(cfg, rand.New(rand.NewPCG(1, 2))), v: v, now: 100 * sec}
}

func (h *leaderHarness) epochNow() uint64 {
	return max(h.v.Epoch(), uint64(h.now/int64(h.l.cfg.EpochPeriod)))
}

// tick runs one leader tick, applies the delta, validates the result, and
// returns the ops (nil when there was nothing to do).
func (h *leaderHarness) tick(newTerm uint64) []view.Op {
	h.tb.Helper()

	ops := h.l.Tick(h.now, h.epochNow(), h.v, newTerm)
	if newTerm != 0 && ops == nil {
		h.tb.Fatalf("no ops when starting term %d", newTerm)
	}

	if ops != nil {
		h.apply(ops)
	}

	return ops
}

func (h *leaderHarness) apply(ops []view.Op) {
	h.tb.Helper()

	d := view.NewDelta(h.v, ops...)

	nv, err := view.Apply(h.v, d)
	if err != nil {
		h.tb.Fatalf("apply %v: %v", opKinds(ops), err)
	}

	if err := view.Validate(nv); err != nil {
		h.tb.Fatalf("validate after %v: %v", opKinds(ops), err)
	}

	h.v = nv
}

func opKinds(ops []view.Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.Kind.String()
	}

	return out
}

func count(ops []view.Op, k view.OpKind) int {
	n := 0

	for _, op := range ops {
		if op.Kind == k {
			n++
		}
	}

	return n
}

// run ticks every second for d.
func (h *leaderHarness) run(d time.Duration) {
	h.tb.Helper()

	for end := h.now + int64(d); h.now < end; {
		h.now += sec
		h.tick(0)
	}
}

func (h *leaderHarness) bootstrap() {
	h.tb.Helper()

	h.term++
	h.tick(h.term)
}

func member(i int) Joiner {
	id := "m" + strconv.Itoa(i)

	return Joiner{ID: id, Incarnation: 7, Addr: id + ":1", Zone: "z"}
}

func (h *leaderHarness) settled() bool {
	pm := make([]PlanMember, 0, h.v.Len())
	h.v.ForEachMember(func(s view.Slot, m *view.Member) bool {
		pm = append(pm, PlanMember{Slot: s, Key: m.Key, Weight: m.Weight, Cell: m.Cell})

		return true
	})

	ok, err := NewPlanner(PlannerConfig{}).Settled(h.v.Table(), pm)
	if err != nil {
		h.tb.Fatal(err)
	}

	return ok
}

func (h *leaderHarness) cellsBalanced() bool {
	sh := h.v.Shape()
	lo, hi := h.v.Len(), 0

	for c := range sh.Cells() {
		o := h.v.Occupancy(c)
		lo, hi = min(lo, o), max(hi, o)
	}

	return hi-lo <= 1
}

func TestBootstrap(t *testing.T) {
	h := newLeaderHarness(t, smallCfg("a"), nil)

	if ops := h.l.Tick(h.now, 0, h.v, 0); ops != nil {
		t.Fatalf("ops without a term on an unconfigured view: %v", opKinds(ops))
	}

	h.bootstrap()

	if h.v.Len() != 1 || h.v.Leader() != 0 || h.v.Term() != 1 {
		t.Fatalf("len %d leader %d term %d", h.v.Len(), h.v.Leader(), h.v.Term())
	}

	m, _ := h.v.Member(0)
	if m.State != view.Active || m.Weight != view.MaxWeight || h.v.Load(0) != 1<<10 {
		t.Fatalf("self %+v load %d", m, h.v.Load(0))
	}

	if h.v.Period() != DefaultEpochPeriod {
		t.Fatalf("period %v", h.v.Period())
	}

	// Nothing else to do until the epoch advances.
	if ops := h.tick(0); ops != nil {
		t.Fatalf("idle tick produced %v", opKinds(ops))
	}

	h.now += int64(DefaultEpochPeriod)
	if ops := h.tick(0); count(ops, view.KindEpoch) != 1 {
		t.Fatalf("epoch heartbeat missing: %v", opKinds(ops))
	}
}

func TestJoinQuarantineWarmup(t *testing.T) {
	h := newLeaderHarness(t, smallCfg("a"), nil)
	h.bootstrap()

	for i := range 5 {
		h.l.Join(h.now, member(i))
	}

	h.now += sec
	ops := h.tick(0)

	if got := count(ops, view.KindAddMember); got != 5 {
		t.Fatalf("admitted %d, want 5", got)
	}

	s, ok := h.v.Lookup("m0")
	if !ok || h.v.State(s) != view.Joining {
		t.Fatalf("m0 not joining")
	}

	// Duplicate join is ignored.
	h.l.Join(h.now, member(0))
	h.now += sec

	if ops := h.tick(0); count(ops, view.KindAddMember) != 0 {
		t.Fatalf("readmitted: %v", opKinds(ops))
	}

	h.run(DefaultQuarantineFor)

	m, _ := h.v.Member(s)
	if m.State != view.Active || m.Weight != DefaultWarmupWeights[0] {
		t.Fatalf("after quarantine %+v", m)
	}

	h.run(DefaultWarmupStep)

	if m, _ = h.v.Member(s); m.Weight != DefaultWarmupWeights[1] {
		t.Fatalf("after one warmup step weight %d", m.Weight)
	}

	h.run(2 * DefaultWarmupStep)

	if m, _ = h.v.Member(s); m.Weight != view.MaxWeight {
		t.Fatalf("after warmup weight %d", m.Weight)
	}

	h.run(30 * time.Second)

	if !h.settled() {
		t.Fatal("placement not settled")
	}

	if h.v.Load(s) == 0 {
		t.Fatal("joined member owns nothing")
	}
}

func TestJoinCap(t *testing.T) {
	cfg := smallCfg("a")
	cfg.MaxJoinsPerTick = 3
	h := newLeaderHarness(t, cfg, nil)
	h.bootstrap()

	for i := range 7 {
		h.l.Join(h.now, member(i))
	}

	for _, want := range []int{3, 3, 1, 0} {
		h.now += sec
		if got := count(h.tick(0), view.KindAddMember); got != want {
			t.Fatalf("admitted %d, want %d", got, want)
		}
	}
}

func TestJoinQueueBound(t *testing.T) {
	cfg := smallCfg("a")
	cfg.MaxPending = 2
	drops := 0
	l := NewLeader(cfg, nil)
	l.SetHooks(LeaderHooks{OnDrop: func(string) { drops++ }})

	for i := range 4 {
		l.Join(0, member(i))
	}

	l.Join(0, member(0)) // replaces, not dropped

	if j, _, _, _ := l.Pending(); j != 2 || drops != 2 {
		t.Fatalf("pending %d drops %d", j, drops)
	}

	l.Join(0, Joiner{}) // invalid

	if j, _, _, _ := l.Pending(); j != 2 {
		t.Fatalf("invalid join queued")
	}
}

func TestRejoinReplacesIncarnation(t *testing.T) {
	h := newLeaderHarness(t, smallCfg("a"), nil)
	h.bootstrap()
	h.l.Join(h.now, member(1))
	h.run(2 * time.Minute)

	old, _ := h.v.Lookup("m1")
	if h.v.Load(old) == 0 {
		t.Fatal("m1 owns nothing")
	}

	m := member(1)
	m.Incarnation = 8
	h.l.Join(h.now, m)
	h.now += sec
	ops := h.tick(0)

	if count(ops, view.KindRemoveMember) != 1 || count(ops, view.KindAddMember) != 1 || count(ops, view.KindAssign) != 1 {
		t.Fatalf("ops %v", opKinds(ops))
	}

	s, _ := h.v.Lookup("m1")

	got, _ := h.v.Member(s)
	if got.Incarnation != 8 || got.State != view.Joining {
		t.Fatalf("member %+v", got)
	}

	// The leader's own ID from another process is never admitted.
	h.l.Join(h.now, Joiner{ID: "a", Incarnation: 99})
	h.now += sec

	if ops := h.tick(0); count(ops, view.KindAddMember) != 0 {
		t.Fatalf("admitted own ID: %v", opKinds(ops))
	}
}

func TestLeaveDrain(t *testing.T) {
	h := newLeaderHarness(t, smallCfg("a"), nil)
	h.bootstrap()

	for i := range 3 {
		h.l.Join(h.now, member(i))
	}

	h.run(2 * time.Minute)

	s, _ := h.v.Lookup("m1")
	h.l.Leave(h.now, "m1", 6) // wrong incarnation: ignored
	h.run(time.Second)

	if h.v.State(s) != view.Active {
		t.Fatal("left with wrong incarnation")
	}

	h.l.Leave(h.now, "m1", 7)
	h.run(time.Second)

	if h.v.State(s) != view.Draining {
		t.Fatalf("state %v", h.v.State(s))
	}

	h.run(5 * time.Second)

	if h.v.Load(s) != 0 {
		t.Fatalf("draining member still owns %d", h.v.Load(s))
	}

	if e := h.v.Table().Get(0); e.Owner == s {
		t.Fatal("owner")
	}

	h.run(DefaultDrainFor)

	if h.v.IsMember(s) {
		t.Fatal("drained member not removed")
	}

	// The leader drains itself but never removes itself.
	h.l.Leave(h.now, "a", 1)
	h.run(2 * time.Minute)

	if !h.v.IsMember(0) || h.v.State(0) != view.Draining || h.v.Load(0) != 0 {
		t.Fatalf("self state %v load %d", h.v.State(0), h.v.Load(0))
	}
}

// growLeader builds a cluster of n members through the leader and lets it settle.
func growLeader(t *testing.T, cfg LeaderConfig, n int) *leaderHarness {
	t.Helper()

	h := newLeaderHarness(t, cfg, nil)
	h.bootstrap()

	for i := range n - 1 {
		h.l.Join(h.now, member(i))
	}

	h.run(3 * time.Minute)

	if h.v.Len() != n {
		t.Fatalf("len %d, want %d", h.v.Len(), n)
	}

	return h
}

func TestSuspicionRemoval(t *testing.T) {
	h := growLeader(t, smallCfg("a"), 40)

	if h.v.Shape().Cells() < 4 {
		t.Fatalf("shape %v", h.v.Shape().Sizes())
	}

	// Pick a target that is not the leader.
	target := view.Slot(5)
	key := h.v.Key(target)
	obs := view.Observers(h.v, target, nil)
	need := min(DefaultObserverCells, view.ObserverCells(h.v, target))

	// Reports from a non-observer are ignored.
	var stranger view.Slot

	for s := range h.v.SlotCap() {
		if s != target && h.v.IsMember(s) && !contains(obs, s) {
			stranger = s

			break
		}
	}

	h.l.Suspect(h.now, target, key, stranger)
	h.run(time.Second)

	if !h.v.IsMember(target) {
		t.Fatal("removed on a non-observer report")
	}

	// Reports with a stale key are ignored.
	h.l.Suspect(h.now, target, key+1, obs[0])

	// Reports from observers in fewer than need cells do not remove.
	cells := map[uint32]bool{}

	for _, o := range obs {
		c := h.v.Cell(o)
		if !cells[c] && len(cells) < need-1 {
			cells[c] = true

			h.l.Suspect(h.now, target, key, o)
		}
	}

	h.run(time.Second)

	if !h.v.IsMember(target) {
		t.Fatal("removed below the observer cell threshold")
	}

	for _, o := range obs {
		h.l.Suspect(h.now, target, key, o)
	}

	h.now += sec
	ops := h.tick(0)

	if h.v.IsMember(target) || count(ops, view.KindAssign) != 1 {
		t.Fatalf("not removed: %v", opKinds(ops))
	}

	// Expired reports do not count.
	t2 := view.Slot(6)
	obs = view.Observers(h.v, t2, nil)

	for _, o := range obs {
		h.l.Suspect(h.now, t2, h.v.Key(t2), o)
	}

	h.now += int64(DefaultSuspicionWindow) + sec
	h.tick(0)

	if !h.v.IsMember(t2) {
		t.Fatal("removed on expired reports")
	}

	if _, _, s, _ := h.l.Pending(); s != 0 {
		t.Fatalf("suspicions not expired: %d", s)
	}
}

func contains(s []view.Slot, x view.Slot) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}

	return false
}

func TestSuspicionDamping(t *testing.T) {
	cfg := smallCfg("a")
	cfg.CellSize = 2
	h := growLeader(t, cfg, 60)
	damped := 0

	h.l.SetHooks(LeaderHooks{OnDamped: func(int, int) { damped++ }})

	// Suspect 10 of 60 members (more than 10%), each fully observed.
	suspectAll := func() {
		for s := view.Slot(1); s <= 10; s++ {
			if !h.v.IsMember(s) {
				continue
			}

			for _, o := range view.Observers(h.v, s, nil) {
				h.l.Suspect(h.now, s, h.v.Key(s), o)
			}
		}
	}

	suspectAll()

	h.now += sec

	ops := h.tick(0)
	if got := count(ops, view.KindRemoveMember); got != 1 || damped == 0 {
		t.Fatalf("removed %d damped %d", got, damped)
	}
}

func TestReshapeOnGrowth(t *testing.T) {
	reshapes := 0
	cfg := smallCfg("a")
	h := newLeaderHarness(t, cfg, nil)
	h.l.SetHooks(LeaderHooks{OnReshape: func(_, _ view.Shape) { reshapes++ }})
	h.bootstrap()

	for i := range 99 {
		h.l.Join(h.now, member(i))
	}

	h.run(3 * time.Minute)

	if reshapes == 0 || h.v.Shape().Cells() < 16 {
		t.Fatalf("reshapes %d shape %v", reshapes, h.v.Shape().Sizes())
	}

	if !h.cellsBalanced() || !h.settled() {
		t.Fatal("not converged")
	}
}

func TestCellBalance(t *testing.T) {
	h := growLeader(t, smallCfg("a"), 64)

	// Unbalance: move several members into cell 0 by hand.
	var ops []view.Op

	for s := view.Slot(1); s < 12; s++ {
		ops = append(ops, view.OpSetCell(s, 0))
	}

	h.apply(ops)

	if h.cellsBalanced() {
		t.Fatal("setup did not unbalance")
	}

	h.run(5 * time.Second)

	if !h.cellsBalanced() {
		t.Fatal("cells not rebalanced")
	}
}

func TestFailoverContinuity(t *testing.T) {
	h := newLeaderHarness(t, smallCfg("a"), nil)
	h.bootstrap()

	for i := range 8 {
		h.l.Join(h.now, member(i))
	}

	h.run(3 * time.Second)

	// Hand over to m3 (a member) mid-quarantine.
	h.l = NewLeader(LeaderConfig{Self: member(3), PartitionBits: 10}, nil)
	h.term++
	h.now += sec
	ops := h.tick(h.term)

	s3, _ := h.v.Lookup("m3")
	if ops[0].Kind != view.KindNewTerm || ops[0].Slot != s3 || h.v.Leader() != s3 {
		t.Fatalf("ops %v leader %d", opKinds(ops), h.v.Leader())
	}

	h.run(3 * time.Minute)

	if !h.settled() || !h.cellsBalanced() {
		t.Fatal("not converged after failover")
	}

	h.v.ForEachMember(func(_ view.Slot, m *view.Member) bool {
		if m.State != view.Active || m.Weight != view.MaxWeight {
			t.Fatalf("member %s %v %d", m.ID, m.State, m.Weight)
		}

		return true
	})

	// A non-member leader admits itself in the NewTerm delta.
	outsider := Joiner{ID: "z", Incarnation: 3, Addr: "z:1"}
	h.l = NewLeader(LeaderConfig{Self: outsider, PartitionBits: 10}, nil)

	if early := h.l.Tick(h.now, h.epochNow(), h.v, 0); early != nil {
		t.Fatalf("non-member published outside a term start: %v", opKinds(early))
	}

	h.term++
	h.now += sec
	ops = h.tick(h.term)

	sz, ok := h.v.Lookup("z")
	if !ok || h.v.Leader() != sz || count(ops, view.KindAddMember) != 1 {
		t.Fatalf("outsider not admitted: %v", opKinds(ops))
	}

	// A restarted leader process (new incarnation) replaces its old record.
	outsider.Incarnation = 4
	h.l = NewLeader(LeaderConfig{Self: outsider, PartitionBits: 10}, nil)
	h.term++
	h.now += sec
	ops = h.tick(h.term)

	if count(ops, view.KindRemoveMember) != 1 || h.v.Key(h.v.Leader()) != view.MemberKey("z", 4) {
		t.Fatalf("restart: %v", opKinds(ops))
	}
}

func TestInvalidate(t *testing.T) {
	h := newLeaderHarness(t, smallCfg("a"), nil)
	h.bootstrap()
	h.l.Invalidate(42)
	h.l.Invalidate(42)
	h.l.Invalidate(7)
	h.now += sec
	h.tick(0)

	if h.v.Gen(42) != 1 || h.v.Gen(7) != 1 {
		t.Fatalf("gens %d %d", h.v.Gen(42), h.v.Gen(7))
	}

	h.l.Invalidate(42)
	h.now += sec
	h.tick(0)

	if h.v.Gen(42) != 2 {
		t.Fatalf("gen %d", h.v.Gen(42))
	}
}

// TestConvergenceProperty drives random inputs and checks that every delta
// applies and validates (in harness.apply), and that the cluster converges
// once inputs stop.
func TestConvergenceProperty(t *testing.T) {
	seeds := 20
	if testing.Short() {
		seeds = 5
	}

	for seed := range uint64(seeds) {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, 99))
			cfg := smallCfg("a")
			cfg.CellSize = 1 + r.IntN(6)
			cfg.MaxNeighbors = 8 + r.IntN(64)
			cfg.MaxCellMovesPerTick = 1 + r.IntN(8)
			cfg.MaxMoves = 1 + r.IntN(300)
			cfg.MaxJoinsPerTick = 1 + r.IntN(40)
			h := newLeaderHarness(t, cfg, nil)
			h.bootstrap()

			next := 0

			for range 200 {
				switch k := r.IntN(10); {
				case k < 4:
					for range r.IntN(10) {
						h.l.Join(h.now, member(next))
						next++
					}
				case k < 5 && next > 0:
					m := member(r.IntN(next))
					m.Incarnation = 1 + r.Uint64N(3)
					h.l.Join(h.now, m)
				case k < 6 && next > 0:
					m := member(r.IntN(next))
					if s, ok := h.v.Lookup(m.ID); ok {
						mm, _ := h.v.Member(s)
						h.l.Leave(h.now, m.ID, mm.Incarnation)
					}
				case k < 8 && h.v.Len() > 1:
					s := view.Slot(r.IntN(int(h.v.SlotCap())))
					if h.v.IsMember(s) {
						for _, o := range view.Observers(h.v, s, nil) {
							h.l.Suspect(h.now, s, h.v.Key(s), o)
						}
					}
				case k < 9:
					h.l.Invalidate(r.Uint64N(4))
				}

				h.now += sec * (1 + r.Int64N(3))
				h.tick(0)
			}

			h.run(10 * time.Minute)

			if !h.settled() {
				t.Fatal("placement not settled")
			}

			if !h.cellsBalanced() {
				t.Fatal("cells not balanced")
			}

			if ops := h.l.Tick(h.now, h.epochNow(), h.v, 0); ops != nil {
				t.Fatalf("not quiescent: %v", opKinds(ops))
			}
		})
	}
}

func TestLargeChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}

	v := viewtest.Build(t, 10000, viewtest.Options{Seed: 3})
	cfg := LeaderConfig{Self: Joiner{ID: "x"}, PartitionBits: v.PartitionBits()}
	// Slot 0 is the viewtest leader; take over as its identity.
	m0, _ := v.Member(0)
	cfg.Self = Joiner{ID: m0.ID, Incarnation: m0.Incarnation, Addr: m0.Addr}
	h := newLeaderHarness(t, cfg, v)
	h.term = v.Term()

	h.term++
	h.tick(h.term)

	for i := range 500 {
		h.l.Join(h.now, member(100000+i))
	}

	for s := view.Slot(100); s < 120; s++ {
		for _, o := range view.Observers(h.v, s, nil) {
			h.l.Suspect(h.now, s, h.v.Key(s), o)
		}
	}

	h.run(3 * time.Minute)

	if h.v.Len() != 10000+500-20 {
		t.Fatalf("len %d", h.v.Len())
	}

	if !h.settled() || !h.cellsBalanced() {
		t.Fatal("not converged")
	}
}

func benchTick(b *testing.B, n int, churn bool) {
	v := viewtest.Build(b, n, viewtest.Options{Seed: 3})
	m0, _ := v.Member(0)
	l := NewLeader(LeaderConfig{Self: Joiner{ID: m0.ID, Incarnation: m0.Incarnation}, PartitionBits: v.PartitionBits()}, nil)
	now := 1000 * sec
	ep := v.Epoch()

	applyOps := func(ops []view.Op) {
		if ops == nil {
			return
		}

		nv, err := view.Apply(v, view.NewDelta(v, ops...))
		if err != nil {
			b.Fatal(err)
		}

		v = nv
	}

	// Settle placement and warm the settled cache.
	for range 100 {
		ops := l.Tick(now, ep, v, 0)
		if ops == nil {
			break
		}

		applyOps(ops)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		if churn {
			l.Join(now, member(1_000_000+i))
		}

		ops := l.Tick(now, ep, v, 0)
		if churn && ops == nil {
			b.Fatal("no ops")
		}

		b.StopTimer()
		applyOps(ops)
		b.StartTimer()
	}
}

func BenchmarkTickIdle10k(b *testing.B)  { benchTick(b, 10000, false) }
func BenchmarkTickIdle100k(b *testing.B) { benchTick(b, 100000, false) }
func BenchmarkTickJoin10k(b *testing.B)  { benchTick(b, 10000, true) }
func BenchmarkTickJoin100k(b *testing.B) { benchTick(b, 100000, true) }

// TestSuspicionMutualObservers covers two failed members that observe each
// other: the suspected observer's cell does not count towards the cells
// required.
func TestSuspicionMutualObservers(t *testing.T) {
	h := growLeader(t, smallCfg("a"), 40)
	v := h.v

	cellsOf := func(slots []view.Slot) map[uint32]int {
		m := map[uint32]int{}
		for _, s := range slots {
			m[v.Cell(s)]++
		}

		return m
	}

	for t1 := view.Slot(1); t1 < v.SlotCap(); t1++ {
		obs := view.Observers(v, t1, nil)
		cells := cellsOf(obs)

		if len(cells) != DefaultObserverCells {
			continue
		}

		for _, b := range obs {
			if b == 0 || cells[v.Cell(b)] != 1 || v.Cell(b) == v.Cell(t1) {
				continue
			}

			// b is suspected by one of its own observers (not t1).
			for _, o := range view.Observers(v, b, nil) {
				if o != t1 {
					h.l.Suspect(h.now, b, v.Key(b), o)

					break
				}
			}

			for _, o := range obs {
				if o != b {
					h.l.Suspect(h.now, t1, v.Key(t1), o)
				}
			}

			h.now += sec
			h.tick(0)

			if h.v.IsMember(t1) {
				t.Fatalf("slot %d not removed with its suspected observer %d excluded", t1, b)
			}

			return
		}
	}

	t.Skip("no suitable target in this view")
}

// TestOpBudget checks that a tick with more due work than one delta can
// hold stays under view.MaxDeltaOps and finishes on later ticks.
func TestOpBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("large")
	}

	const n = 70000

	v := viewtest.Build(t, n, viewtest.Options{Seed: 5, Joining: n - 100, PartitionBits: 12})
	m0, _ := v.Member(0)
	h := newLeaderHarness(t, LeaderConfig{Self: Joiner{ID: m0.ID, Incarnation: m0.Incarnation}, PartitionBits: 12}, v)
	h.now = 1000 * sec

	ops := h.tick(0)
	if len(ops) > view.MaxDeltaOps || count(ops, view.KindSetState) < maxMemberOps-10 {
		t.Fatalf("%d ops, %d SetState", len(ops), count(ops, view.KindSetState))
	}

	h.now += sec
	h.tick(0)

	joining := 0

	h.v.ForEachMember(func(_ view.Slot, m *view.Member) bool {
		if m.State == view.Joining {
			joining++
		}

		return true
	})

	if joining != 0 {
		t.Fatalf("%d members still joining", joining)
	}
}
