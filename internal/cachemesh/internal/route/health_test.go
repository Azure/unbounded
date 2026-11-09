// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

const sec = int64(time.Second)

func newT(cfg HealthConfig) *HealthTracker {
	return NewHealthTracker(cfg, rand.New(rand.NewPCG(1, 2)))
}

// trackN tracks n peers, slots 0..n-1, each in its own group of cellSize.
func trackN(t *HealthTracker, n, cellSize int) {
	for i := range n {
		t.Track(uint32(i), uint64(1000+i), uint32(i/cellSize))
	}
}

func failN(t *HealthTracker, slot uint32, n int) {
	for range n {
		t.Observe(slot, Failure, time.Millisecond)
	}
}

func TestBreakerTrips(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 20, 4)
	tr.Tick(0)

	failN(tr, 1, 4)
	tr.Observe(1, Success, time.Millisecond)
	failN(tr, 1, 4)

	if tr.Usable(1) != Healthy {
		t.Fatal("4 failures + success + 4 failures must not trip")
	}

	tr.Observe(1, Timeout, time.Millisecond)

	if tr.Usable(1) != Unusable {
		t.Fatal("5th consecutive failure must trip")
	}

	if s, n, _ := tr.PeerState(1); s != StateEjected || n != 1 {
		t.Fatalf("state %v ejections %d", s, n)
	}
}

func TestEjectionBackoffProbeAndCap(t *testing.T) {
	var (
		events []string
		durs   []time.Duration
	)

	tr := newT(HealthConfig{Hooks: HealthHooks{
		OnEject: func(_ uint32, r Reason, n int, d time.Duration) {
			events = append(events, r.String())
			durs = append(durs, d)
		},
		OnProbe:   func(uint32) { events = append(events, "probe") },
		OnUneject: func(uint32) { events = append(events, "uneject") },
	}})
	trackN(tr, 20, 4)

	now := int64(0)
	tr.Tick(now)
	failN(tr, 0, 5)

	for i := 1; i <= 12; i++ {
		now += sec / 10
		tr.Tick(now)

		d := durs[len(durs)-1]

		want := 30 * time.Second * time.Duration(i)
		if want > 300*time.Second {
			want = 300 * time.Second
		}

		if d < want || d > want+want/10 {
			t.Fatalf("ejection %d: %v not in [%v,%v]", i, d, want, want+want/10)
		}

		now += int64(d) - 1
		tr.Tick(now)

		if s, _, _ := tr.PeerState(0); s != StateEjected || tr.Usable(0) != Unusable {
			t.Fatal("must still be ejected")
		}

		now++
		tr.Tick(now)

		if s, _, _ := tr.PeerState(0); s != StateProbing || tr.Usable(0) != Unusable {
			t.Fatalf("want probing, got %v", s)
		}

		tr.Observe(0, Failure, time.Millisecond)

		if s, n, _ := tr.PeerState(0); s != StateEjected || n != i+1 {
			t.Fatalf("probe failure: %v %d", s, n)
		}
	}

	// Let it probe and succeed.
	now += sec / 10
	tr.Tick(now)
	now += int64(durs[len(durs)-1])
	tr.Tick(now)
	tr.Observe(0, Success, time.Millisecond)

	if tr.Usable(0) != Healthy {
		t.Fatal("probe success must heal")
	}

	tr.Tick(now + 1)

	if events[len(events)-1] != "uneject" || events[0] != "consecutive-failures" || events[1] != "probe" ||
		events[2] != "probe-failed" {
		t.Fatalf("events %v", events)
	}

	// Ejection count decays by one per healthy interval.
	_, n0, _ := tr.PeerState(0)

	for i := range 3 {
		now += 10 * sec
		tr.Tick(now)

		if _, n, _ := tr.PeerState(0); n != n0-i-1 {
			t.Fatalf("decay: %d want %d", n, n0-i-1)
		}
	}
}

func TestMaxEjectionPercent(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 50, 1000) // one cell, so only the percent limit applies
	tr.Tick(0)

	for i := range 50 {
		failN(tr, uint32(i), 5)
	}

	if c := tr.Counts(); c.Ejected != 5 {
		t.Fatalf("ejected %d want 5", c.Ejected)
	}

	// At least one may always be ejected.
	small := newT(HealthConfig{})
	trackN(small, 3, 3)
	failN(small, 0, 5)
	failN(small, 1, 5)

	if c := small.Counts(); c.Ejected != 1 {
		t.Fatalf("small: ejected %d want 1", c.Ejected)
	}
}

func TestLastUsableInCell(t *testing.T) {
	tr := newT(HealthConfig{MaxEjectionPercent: 100, PanicThreshold: 100})
	trackN(tr, 8, 2) // cells of 2
	failN(tr, 0, 5)
	failN(tr, 1, 5)

	if tr.Usable(0) != Unusable || tr.Usable(1) != Healthy {
		t.Fatal("last member of a cell must not be ejected")
	}

	// Move slot 1 (same key) into cell 1, which then has three usable
	// members; two of them may be ejected.
	tr.Track(1, 1001, 1)
	failN(tr, 1, 5)

	if tr.Usable(1) != Unusable {
		t.Fatal("slot 1 has peers in its new cell")
	}

	failN(tr, 2, 5)
	failN(tr, 3, 5)

	if c := tr.Counts(); c.Ejected != 3 || tr.Usable(3) != Healthy {
		t.Fatalf("ejected %d want 3", c.Ejected)
	}
}

func TestSuccessRate(t *testing.T) {
	var reasons []Reason

	tr := newT(HealthConfig{ConsecutiveFailures: 1000, Hooks: HealthHooks{OnEject: func(_ uint32, r Reason, _ int, _ time.Duration) {
		reasons = append(reasons, r)
	}}})
	trackN(tr, 10, 10)
	tr.Tick(0)

	// Nine peers at 100% success, one at 70%: mean 0.97, stdev 0.09,
	// threshold 0.799.
	for i := range 10 {
		for j := range 100 {
			r := Success
			if i == 7 && j >= 70 {
				r = Failure
			}

			tr.Observe(uint32(i), r, time.Millisecond)
		}
	}

	if tr.Usable(7) != Healthy {
		t.Fatal("no detection before interval")
	}

	tr.Tick(10 * sec)

	if tr.Usable(7) != Unusable || len(reasons) != 1 || reasons[0] != ReasonSuccessRate {
		t.Fatalf("success-rate ejection: %v %v", tr.Usable(7), reasons)
	}

	for i := range 10 {
		if i != 7 && tr.Usable(uint32(i)) != Healthy {
			t.Fatalf("slot %d ejected", i)
		}
	}
}

func TestSuccessRateMinHosts(t *testing.T) {
	tr := newT(HealthConfig{FailurePercentageThreshold: 100})
	trackN(tr, 4, 4)
	tr.Tick(0)

	for i := range 4 {
		for j := range 100 {
			r := Success
			if i == 0 && j%2 == 0 {
				r = Failure
			}

			tr.Observe(uint32(i), r, 0)
		}
	}

	tr.Tick(10 * sec)

	if tr.Counts().Ejected != 0 {
		t.Fatal("fewer than 5 hosts with volume must not eject")
	}
}

func TestFailurePercentage(t *testing.T) {
	tr := newT(HealthConfig{ConsecutiveFailures: 1000})
	trackN(tr, 20, 4)
	tr.Tick(0)

	obs := func(slot uint32, succ, fails int) {
		for range succ {
			tr.Observe(slot, Success, 0)
		}

		failN(tr, slot, fails)
	}
	obs(0, 7, 43)  // 86% of 50: eject
	obs(4, 8, 42)  // 84%: keep
	obs(8, 1, 48)  // 49 requests: below volume
	obs(12, 0, 50) // 100%: eject (second, still within limit of 2)

	tr.Tick(10 * sec)

	want := map[uint32]Usability{0: Unusable, 4: Healthy, 8: Healthy, 12: Unusable}
	for s, u := range want {
		if tr.Usable(s) != u {
			t.Errorf("slot %d: %v want %v", s, tr.Usable(s), u)
		}
	}
}

func TestPanicThreshold(t *testing.T) {
	var panics []bool

	tr := newT(HealthConfig{MaxEjectionPercent: 100, Hooks: HealthHooks{OnPanic: func(p bool) { panics = append(panics, p) }}})
	trackN(tr, 10, 10)

	for i := range 5 {
		failN(tr, uint32(i), 5)
	}

	tr.Tick(0)

	if tr.Panic() {
		t.Fatal("5/10 is not more than 50%")
	}

	failN(tr, 5, 5)
	tr.Tick(1)

	if !tr.Panic() || tr.Usable(0) != Healthy {
		t.Fatal("6/10 must panic and disable filtering")
	}

	tr.Untrack(0)
	tr.Untrack(1)
	tr.Track(20, 1, 0)
	tr.Track(21, 2, 0)
	tr.Track(22, 3, 0)

	if tr.Panic() {
		t.Fatal("4/11 must clear panic")
	}

	if len(panics) != 2 || !panics[0] || panics[1] {
		t.Fatalf("panic hooks %v", panics)
	}
}

func TestDegradedByEWMA(t *testing.T) {
	var flips int

	tr := newT(HealthConfig{Hooks: HealthHooks{OnDegraded: func(uint32, bool) { flips++ }}})
	trackN(tr, 5, 5)
	tr.Tick(0)

	for i := range 5 {
		lat := 5 * time.Millisecond
		if i == 2 {
			lat = 50 * time.Millisecond
		}

		tr.Observe(uint32(i), Success, lat)
	}

	tr.Tick(sec / 10)

	if tr.Usable(2) != Degraded || tr.Usable(1) != Healthy {
		t.Fatal("slow peer must be degraded")
	}

	if s, _, e := tr.PeerState(2); s != StateDegraded || e < 45*time.Millisecond {
		t.Fatalf("state %v ewma %v", s, e)
	}

	// Fast observations decay the EWMA on the tick clock.
	now := sec / 10
	for range 600 {
		now += sec / 10

		tr.Observe(2, Success, 5*time.Millisecond)
		tr.Tick(now)
	}

	if tr.Usable(2) != Healthy || flips != 2 {
		t.Fatalf("decay: %v flips %d", tr.Usable(2), flips)
	}

	// Slow observation is taken immediately.
	tr.Observe(2, Success, 80*time.Millisecond)

	if _, _, e := tr.PeerState(2); e != 80*time.Millisecond {
		t.Fatalf("peak not immediate: %v", e)
	}

	// Floor: everything fast, 3x median below floor is not degraded.
	tr2 := newT(HealthConfig{})
	trackN(tr2, 3, 3)
	tr2.Observe(0, Success, time.Millisecond)
	tr2.Observe(1, Success, time.Millisecond)
	tr2.Observe(2, Success, 9*time.Millisecond)
	tr2.Tick(0)

	if tr2.Usable(2) != Healthy {
		t.Fatal("below floor must stay healthy")
	}
}

func TestBusy(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 10, 2)
	tr.Tick(0)

	for range 20 {
		tr.Observe(1, Overload, time.Millisecond)
	}

	if tr.Usable(1) != Degraded {
		t.Fatal("overload must mark busy")
	}

	if s, n, _ := tr.PeerState(1); s != StateBusy || n != 0 {
		t.Fatal("overload must not trip the breaker")
	}

	tr.Tick(sec - 1)

	if tr.Usable(1) != Degraded {
		t.Fatal("still busy")
	}

	tr.Tick(sec)

	if tr.Usable(1) != Healthy {
		t.Fatal("busy must expire")
	}
}

func TestHedgeAfter(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 3, 3)

	if tr.HedgeAfter(0) != time.Second || tr.HedgeAfter(99) != time.Second {
		t.Fatal("no data must be HedgeMax")
	}

	tr.Observe(0, Success, time.Millisecond)
	tr.Observe(1, Success, 20*time.Millisecond)
	tr.Observe(2, Success, 10*time.Second)

	if a, b, c := tr.HedgeAfter(0), tr.HedgeAfter(1), tr.HedgeAfter(2); a != 5*time.Millisecond ||
		b != 60*time.Millisecond || c != time.Second {
		t.Fatalf("hedge after %v %v %v", a, b, c)
	}
}

func TestHedgeBudget(t *testing.T) {
	tr := newT(HealthConfig{})
	tr.Tick(0)

	for range 16000 {
		tr.NoteRouted()
	}

	got := 0

	for range 16000 {
		if tr.TryHedge() {
			got++
		}
	}

	if got != 800 {
		t.Fatalf("hedges %d want 800", got)
	}

	tr.Tick(10 * sec)

	if tr.TryHedge() {
		t.Fatal("budget resets with routed count")
	}

	for range 20 {
		tr.NoteRouted()
	}

	if !tr.TryHedge() || tr.TryHedge() {
		t.Fatal("1 hedge per 20 routed")
	}
}

func TestUntrackRetrack(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 20, 4)
	tr.Observe(3, Success, 40*time.Millisecond)
	failN(tr, 3, 5)

	if tr.Usable(3) != Unusable {
		t.Fatal("ejected")
	}

	tr.Track(3, 1003, 0) // same key: keeps state

	if tr.Usable(3) != Unusable {
		t.Fatal("same key must keep state")
	}

	tr.Track(3, 7, 0) // new key: reset

	if s, n, e := tr.PeerState(3); s != StateHealthy || n != 0 || e != 0 {
		t.Fatalf("reset: %v %d %v", s, n, e)
	}

	tr.Untrack(3)
	tr.Untrack(3)

	if tr.Tracked() != 19 || tr.Usable(3) != Healthy {
		t.Fatal("untracked is healthy")
	}

	tr.Observe(3, Failure, 0) // no-op

	if tr.Usable(3) != Healthy {
		t.Fatal("observe on untracked must be a no-op")
	}
}

func TestTableManySlots(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	tr := newT(HealthConfig{})
	want := map[uint32]uint64{}

	for range 5000 {
		s := rng.Uint32N(1 << 20)
		if rng.IntN(3) == 0 {
			tr.Untrack(s)
			delete(want, s)

			continue
		}

		k := rng.Uint64()
		tr.Track(s, k, s%64)
		want[s] = k
	}

	if tr.Tracked() != len(want) {
		t.Fatalf("tracked %d want %d", tr.Tracked(), len(want))
	}

	nt := tr.tab.Load()
	for s, k := range want {
		if p := nt.lookup(s); p == nil || p.key != k {
			t.Fatalf("slot %d lost", s)
		}
	}

	for i := 1; i < len(nt.peers); i++ {
		if nt.peers[i-1].slot >= nt.peers[i].slot {
			t.Fatal("peers not sorted")
		}
	}

	for range 10000 {
		s := rng.Uint32N(1 << 20)
		if _, ok := want[s]; (nt.lookup(s) != nil) != ok {
			t.Fatalf("lookup %d mismatch", s)
		}
	}

	if len(nt.entries) > 4*len(want)+8 {
		t.Fatalf("table too large: %d", len(nt.entries))
	}
}

// TestRandomizedInvariants drives random operations and checks the limits.
func TestRandomizedInvariants(t *testing.T) {
	for seed := range uint64(20) {
		rng := rand.New(rand.NewPCG(seed, 1))
		tr := NewHealthTracker(HealthConfig{MaxEjectionPercent: 30}, rand.New(rand.NewPCG(seed, 2)))
		now := int64(0)

		for step := range 3000 {
			switch rng.IntN(10) {
			case 0:
				s := rng.Uint32N(64)
				tr.Track(s, uint64(rng.IntN(2)), s%8)
			case 1:
				tr.Untrack(rng.Uint32N(64))
			case 2:
				now += rng.Int64N(5 * sec)
				tr.Tick(now)
			default:
				tr.Observe(rng.Uint32N(64), Result(rng.IntN(4)), time.Duration(rng.Int64N(int64(time.Second))))
			}

			nt := tr.tab.Load()
			ej := 0
			usable := map[uint32]int{}

			for _, p := range nt.peers {
				c := p.cc.Load().cell

				if p.state.Load() == bHealthy {
					usable[c]++
				} else {
					ej++
				}
			}

			for _, cc := range nt.cells {
				if int(cc.usable.Load()) != usable[cc.cell] {
					t.Fatalf("seed %d step %d: cell %d usable %d want %d",
						seed, step, cc.cell, cc.usable.Load(), usable[cc.cell])
				}
			}

			if int(tr.ejected.Load()) != ej {
				t.Fatalf("seed %d step %d: ejected counter %d want %d", seed, step, tr.ejected.Load(), ej)
			}
		}
	}
}

func BenchmarkObserve(b *testing.B) {
	tr := newT(HealthConfig{})
	trackN(tr, 250, 4)
	tr.Tick(0)

	i := uint32(0)
	for b.Loop() {
		tr.Observe(i%250, Success, time.Millisecond)
		i++
	}
}

func BenchmarkObserveParallel(b *testing.B) {
	tr := newT(HealthConfig{})
	trackN(tr, 250, 4)
	tr.Tick(0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := rand.Uint32()
		for pb.Next() {
			tr.Observe(i%250, Success, time.Millisecond)
			i++
		}
	})
}

func BenchmarkUsable(b *testing.B) {
	tr := newT(HealthConfig{})
	trackN(tr, 250, 4)
	tr.Tick(0)

	n := 0
	i := uint32(0)

	for b.Loop() {
		if tr.Usable(i%250) == Healthy {
			n++
		}

		i++
	}

	_ = n
}

func BenchmarkHedge(b *testing.B) {
	tr := newT(HealthConfig{})

	b.RunParallel(func(pb *testing.PB) {
		h := rand.Uint64()

		for pb.Next() {
			tr.NoteRouted()
			tr.TryHedge()

			h++
		}
	})
}

func TestConcurrentHotAndControl(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 100, 4)

	var (
		wg   sync.WaitGroup
		stop atomic.Bool
	)

	for g := range 4 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(g), 0))
			for !stop.Load() {
				s := rng.Uint32N(120)
				tr.Observe(s, Result(rng.IntN(4)), time.Duration(rng.Int64N(int64(time.Second))))
				tr.Usable(s)
				tr.HedgeAfter(s)
				tr.NoteRouted()
				tr.TryHedge()
			}
		})
	}

	rng := rand.New(rand.NewPCG(9, 9))
	for i := range 3000 {
		s := rng.Uint32N(120)
		if i%3 == 0 {
			tr.Untrack(s)
		} else {
			tr.Track(s, uint64(rng.IntN(2)), s%30)
		}

		tr.Tick(int64(i) * sec / 2)
	}

	stop.Store(true)
	wg.Wait()
	tr.Tick(4000 * sec)

	if c := tr.Counts(); c.Ejected+c.Probing != int(tr.ejected.Load()) {
		t.Fatalf("counter drift not repaired: %+v vs %d", c, tr.ejected.Load())
	}
}

func TestSyncReplacesTrackedSet(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 20, 4)
	tr.Tick(0)
	failN(tr, 0, 5)
	failN(tr, 1, 5)

	if tr.Usable(0) != Unusable || tr.Usable(1) != Unusable {
		t.Fatal("setup: slots 0 and 1 not ejected")
	}

	// Keep 0..9 (slot 1 with a new key, slot 3 in a new cell), add 30, drop
	// the rest; slot 5 appears twice and the last entry wins.
	var peers []TrackRef
	for i := range uint32(10) {
		peers = append(peers, TrackRef{Slot: i, Key: uint64(1000 + i), Cell: i / 4})
	}

	peers[1].Key = 7
	peers[3].Cell = 9
	peers = append(peers, TrackRef{Slot: 30, Key: 30, Cell: 9}, TrackRef{Slot: 5, Key: 1005, Cell: 2})
	tr.Sync(peers)

	if tr.Tracked() != 11 {
		t.Fatalf("tracked %d, want 11", tr.Tracked())
	}

	if tr.Usable(0) != Unusable {
		t.Fatal("Sync reset the state of an unchanged peer")
	}

	if tr.Usable(1) != Healthy {
		t.Fatal("Sync kept the state of a peer whose key changed")
	}

	tr.Sync(nil)

	if tr.Tracked() != 0 {
		t.Fatalf("tracked %d after empty Sync", tr.Tracked())
	}
}

func allUsableHealthy(tr *HealthTracker, n int) bool {
	for s := range uint32(n) {
		if tr.Usable(s) != Healthy {
			return false
		}
	}

	return true
}

func TestAllHealthyTransitions(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 20, 4)
	tr.Tick(0)

	if !tr.AllHealthy() {
		t.Fatal("fresh tracker must be all healthy")
	}

	// Ejection clears it; probing keeps it clear; a probe success sets it.
	failN(tr, 3, 5)

	if tr.AllHealthy() {
		t.Fatal("ejected peer must clear AllHealthy")
	}

	tr.Tick(sec)
	tr.Tick(40 * sec)

	if s, _, _ := tr.PeerState(3); s != StateProbing {
		t.Fatalf("state %v, want probing", s)
	}

	if tr.AllHealthy() {
		t.Fatal("probing peer must clear AllHealthy")
	}

	tr.Observe(3, Success, time.Millisecond)

	if !tr.AllHealthy() {
		t.Fatal("recovered peer must set AllHealthy")
	}

	// Busy lasts until the deadline passes.
	tr.Observe(5, Overload, 0)

	if tr.AllHealthy() {
		t.Fatal("busy peer must clear AllHealthy")
	}

	tr.Tick(40*sec + int64(time.Second))

	if !tr.AllHealthy() {
		t.Fatal("expired busy must set AllHealthy")
	}

	// Untracking an ejected peer restores it without a Tick.
	failN(tr, 7, 5)

	if tr.AllHealthy() {
		t.Fatal("ejected peer must clear AllHealthy")
	}

	tr.Untrack(7)

	if !tr.AllHealthy() {
		t.Fatal("untracked ejected peer must not count")
	}

	// A stale holder of the dropped peer cannot eject or revive it.
	tr.Track(7, 1007, 1)

	if !tr.AllHealthy() || tr.Usable(7) != Healthy {
		t.Fatal("retracked peer must start healthy")
	}
}

func TestAllHealthyRetiredPeer(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 10, 2)
	tr.Tick(0)

	p := tr.tab.Load().lookup(4)
	tr.Track(4, 9999, 2) // new key replaces the peer

	if p.state.Load() != bRetired {
		t.Fatalf("dropped peer state %d, want retired", p.state.Load())
	}

	// An Observe through the dropped peer (as by a caller holding the old
	// table) must not change any counter.
	for range 10 {
		p.state.Load()
		tr.tryEject(p, bHealthy, ReasonConsecutiveFailures)
	}

	if !tr.AllHealthy() || tr.ejected.Load() != 0 {
		t.Fatalf("retired peer ejected: all=%v ejected=%d", tr.AllHealthy(), tr.ejected.Load())
	}
}

func TestAllHealthyDegraded(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 10, 2)
	tr.Tick(0)

	for s := range uint32(10) {
		tr.Observe(s, Success, time.Millisecond)
	}

	tr.Observe(2, Success, time.Second)
	tr.Tick(1)

	if tr.Usable(2) != Degraded {
		t.Fatal("slow peer must be degraded")
	}

	if tr.AllHealthy() {
		t.Fatal("degraded peer must clear AllHealthy")
	}
}

func TestAllHealthyPanic(t *testing.T) {
	// The last usable member of a cell is never ejected, so use cells of 5.
	tr := newT(HealthConfig{MaxEjectionPercent: 100, PanicThreshold: 10})
	trackN(tr, 10, 5)
	tr.Tick(0)
	failN(tr, 0, 5)
	failN(tr, 5, 5)
	tr.Tick(1)

	if !tr.Panic() {
		t.Fatal("expected panic")
	}

	if !tr.AllHealthy() || !allUsableHealthy(tr, 10) {
		t.Fatal("panic must report all healthy")
	}
}

// TestAllHealthyRandomized checks that AllHealthy never claims health when a
// tracked peer is not Healthy, and that after a Tick it is exact.
func TestAllHealthyRandomized(t *testing.T) {
	for seed := range uint64(30) {
		rng := rand.New(rand.NewPCG(seed, 7))
		tr := newT(HealthConfig{ConsecutiveFailures: 2, BaseEjection: time.Second, MaxEjection: 4 * time.Second})

		const n = 24

		trackN(tr, n, 3)

		now := int64(0)
		tr.Tick(now)

		for step := range 3000 {
			slot := uint32(rng.IntN(n))

			switch op := rng.IntN(10); {
			case op < 5:
				tr.Observe(slot, Success, time.Duration(1+rng.IntN(5))*time.Millisecond)
			case op < 8:
				tr.Observe(slot, Result(1+rng.IntN(2)), time.Millisecond)
			case op < 9:
				tr.Observe(slot, Overload, 0)
			default:
				now += int64(time.Duration(rng.IntN(800)) * time.Millisecond)
				tr.Tick(now)

				if got, want := tr.AllHealthy(), allUsableHealthy(tr, n); got != want {
					t.Fatalf("seed %d step %d: after Tick AllHealthy=%v, all usable healthy=%v", seed, step, got, want)
				}
			}

			if tr.AllHealthy() && !allUsableHealthy(tr, n) {
				t.Fatalf("seed %d step %d: AllHealthy with an unhealthy peer", seed, step)
			}
		}
	}
}

// TestLatencySampling checks that the latency mean counts every success up
// to latSampleAll per interval and one in latSampleEvery after that, and
// every failure.
func TestLatencySampling(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 1, 1)
	tr.Tick(0)

	for range 2 * latSampleAll {
		tr.Observe(0, Success, 2*time.Millisecond)
	}

	p := tr.tab.Load().lookup(0)
	want := int64(latSampleAll + latSampleAll/latSampleEvery)

	if got := p.latN.Load(); got != want {
		t.Fatalf("latN = %d, want %d", got, want)
	}

	if got := p.latSum.Load(); got != want*int64(2*time.Millisecond) {
		t.Fatalf("latSum = %d", got)
	}

	tr.Observe(0, Failure, time.Millisecond)

	if got := p.latN.Load(); got != want+1 {
		t.Fatalf("failure not counted: latN = %d", got)
	}

	// Zero latency is never counted.
	tr.Observe(0, Failure, 0)

	if got := p.latN.Load(); got != want+1 {
		t.Fatalf("zero latency counted: latN = %d", got)
	}
}

// TestHedgeBudgetAcrossGoroutines checks that the budget counts requests
// routed from any goroutine, whichever shard they land in.
func TestHedgeBudgetAcrossGoroutines(t *testing.T) {
	tr := newT(HealthConfig{})
	tr.Tick(0)

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 2000 {
				tr.NoteRouted()
			}
		})
	}

	wg.Wait()

	got := 0

	for range 16000 {
		if tr.TryHedge() {
			got++
		}
	}

	if got != 800 {
		t.Fatalf("hedges %d want 800", got)
	}
}

type peerSnap struct {
	slot uint32
	key  uint64
	cell uint32
}

func snap(t *HealthTracker) []peerSnap {
	var out []peerSnap

	for _, p := range t.tab.Load().peers {
		out = append(out, peerSnap{p.slot, p.key, p.cc.Load().cell})
	}

	return out
}

func cellSnap(t *HealthTracker) []uint32 {
	var out []uint32

	for _, c := range t.tab.Load().cells {
		out = append(out, c.cell, uint32(c.n))
	}

	return out
}

// TestTrackManySyncMatchTrack checks that the batch APIs produce the same
// table as the equivalent sequence of Track and Untrack calls.
func TestTrackManySyncMatchTrack(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))

	for iter := range 300 {
		a, b := newT(HealthConfig{}), newT(HealthConfig{})

		for range 1 + rng.IntN(4) {
			refs := make([]TrackRef, rng.IntN(40))
			for i := range refs {
				refs[i] = TrackRef{Slot: rng.Uint32N(50), Key: uint64(rng.IntN(3)), Cell: rng.Uint32N(5)}
			}

			if rng.IntN(2) == 0 {
				for _, r := range refs {
					a.Track(r.Slot, r.Key, r.Cell)
				}

				b.TrackMany(refs)
			} else {
				keep := map[uint32]bool{}

				for _, r := range refs {
					a.Track(r.Slot, r.Key, r.Cell)
					keep[r.Slot] = true
				}

				for _, p := range slices.Clone(a.tab.Load().peers) {
					if !keep[p.slot] {
						a.Untrack(p.slot)
					}
				}

				b.Sync(refs)
			}

			if !slices.Equal(snap(a), snap(b)) || !slices.Equal(cellSnap(a), cellSnap(b)) {
				t.Fatalf("iter %d: track %v\nbatch %v", iter, snap(a), snap(b))
			}

			for s := range uint32(50) {
				if (a.tab.Load().lookup(s) == nil) != (b.tab.Load().lookup(s) == nil) {
					t.Fatalf("iter %d: lookup %d differs", iter, s)
				}
			}

			if a.maxEjected.Load() != b.maxEjected.Load() {
				t.Fatalf("iter %d: maxEjected", iter)
			}
		}
	}
}

func TestSyncKeepsState(t *testing.T) {
	tr := newT(HealthConfig{})

	var refs []TrackRef
	for i := range 20 {
		refs = append(refs, TrackRef{Slot: uint32(i), Key: uint64(100 + i), Cell: uint32(i / 4)})
	}

	tr.Sync(refs)
	failN(tr, 3, 5)
	tr.Tick(0)

	if s, _, _ := tr.PeerState(3); s != StateEjected {
		t.Fatalf("state %v", s)
	}

	// Same set, slot 5 with a new key, slot 19 removed.
	refs[5].Key = 999
	tr.Sync(refs[:19])

	if s, n, _ := tr.PeerState(3); s != StateEjected || n != 1 {
		t.Fatalf("sync must keep state: %v %d", s, n)
	}

	if tr.Tracked() != 19 {
		t.Fatalf("tracked %d", tr.Tracked())
	}

	// No-op Sync must not rebuild.
	before := tr.tab.Load()
	tr.Sync(refs[:19])

	if tr.tab.Load() != before {
		t.Fatal("no-op Sync rebuilt the table")
	}

	tr.Sync(nil)

	if tr.Tracked() != 0 || len(tr.cells) != 0 {
		t.Fatalf("Sync(nil): tracked %d cells %d", tr.Tracked(), len(tr.cells))
	}
}

// TestEjectLoserKeepsTiming: a tryEject that loses the race against
// another ejection must not reset the winner's ejection timing.
func TestEjectLoserKeepsTiming(t *testing.T) {
	var ejects int

	tr := newT(HealthConfig{Hooks: HealthHooks{OnEject: func(uint32, Reason, int, time.Duration) { ejects++ }}})
	trackN(tr, 20, 4)
	tr.Tick(0)
	failN(tr, 1, 5)
	tr.Tick(sec)

	p := tr.tab.Load().lookup(1)
	until := p.until.Load()

	if until == 0 || ejects != 1 {
		t.Fatalf("not timed: %d %d", until, ejects)
	}

	if tr.tryEject(p, bHealthy, ReasonSuccessRate) {
		t.Fatal("second ejection must fail")
	}

	tr.Tick(2 * sec)

	if p.until.Load() != until || ejects != 1 {
		t.Fatalf("loser re-timed the ejection: until %d -> %d, ejects %d", until, p.until.Load(), ejects)
	}
}

// TestIdleDegradedRecovers: a Degraded peer gets no traffic, so its EWMA
// must decay without observations or it would stay Degraded forever.
func TestIdleDegradedRecovers(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 5, 5)
	tr.Tick(0)

	for i := range 5 {
		lat := 5 * time.Millisecond
		if i == 2 {
			lat = 200 * time.Millisecond
		}

		tr.Observe(uint32(i), Success, lat)
	}

	now := sec / 10
	tr.Tick(now)

	if tr.Usable(2) != Degraded {
		t.Fatal("slow peer must be degraded")
	}

	// Others keep getting traffic; peer 2 gets none.
	for range 600 {
		now += sec / 10

		for _, s := range []uint32{0, 1, 3, 4} {
			tr.Observe(s, Success, 5*time.Millisecond)
		}

		tr.Tick(now)
	}

	if tr.Usable(2) != Healthy {
		_, _, e := tr.PeerState(2)
		t.Fatalf("idle degraded peer never recovered, ewma %v", e)
	}

	// Idle peers do not decay below the median, so they do not drag it
	// down and make normal peers look slow.
	if _, _, e := tr.PeerState(2); e < 4*time.Millisecond {
		t.Fatalf("idle ewma decayed below the median: %v", e)
	}
}

func TestLatencyClamp(t *testing.T) {
	tr := newT(HealthConfig{})
	trackN(tr, 1, 1)

	for range 10 {
		tr.Observe(0, Success, time.Duration(1<<62))
	}

	tr.Tick(0)
	tr.Tick(sec)

	if _, _, e := tr.PeerState(0); e <= 0 || e > time.Hour {
		t.Fatalf("ewma %v", e)
	}
}

func BenchmarkTrackBootstrap(b *testing.B) {
	for _, n := range []int{256, 4096} {
		refs := make([]TrackRef, n)
		for i := range refs {
			refs[i] = TrackRef{Slot: uint32(i * 7), Key: uint64(i), Cell: uint32(i / 4)}
		}

		b.Run("Track/"+strconv.Itoa(n), func(b *testing.B) {
			for b.Loop() {
				tr := newT(HealthConfig{})
				for _, r := range refs {
					tr.Track(r.Slot, r.Key, r.Cell)
				}
			}
		})
		b.Run("Sync/"+strconv.Itoa(n), func(b *testing.B) {
			for b.Loop() {
				newT(HealthConfig{}).Sync(refs)
			}
		})
	}
}

// TestPeerLayout checks that the three access groups of Peer each fill one
// 64-byte line.
func TestPeerLayout(t *testing.T) {
	var p healthPeer

	if got := unsafe.Sizeof(p); got != 192 {
		t.Fatalf("sizeof(Peer) = %d, want 192", got)
	}

	if got := unsafe.Offsetof(p.latSum); got != 64 {
		t.Fatalf("offset of latSum = %d, want 64", got)
	}

	if got := unsafe.Offsetof(p.until); got != 128 {
		t.Fatalf("offset of until = %d, want 128", got)
	}

	if got := unsafe.Offsetof(p.degraded); got >= 64 {
		t.Fatalf("offset of degraded = %d, want < 64", got)
	}
}
