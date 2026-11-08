// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sec = int64(time.Second)

func newT(cfg Config) *Tracker {
	return NewTracker(cfg, rand.New(rand.NewPCG(1, 2)))
}

// trackN tracks n peers, slots 0..n-1, each in its own group of cellSize.
func trackN(t *Tracker, n, cellSize int) {
	for i := range n {
		t.Track(uint32(i), uint64(1000+i), uint32(i/cellSize))
	}
}

func fail(t *Tracker, slot uint32, n int) {
	for range n {
		t.Observe(slot, Failure, time.Millisecond)
	}
}

func TestBreakerTrips(t *testing.T) {
	tr := newT(Config{})
	trackN(tr, 20, 4)
	tr.Tick(0)

	fail(tr, 1, 4)
	tr.Observe(1, Success, time.Millisecond)
	fail(tr, 1, 4)

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

	tr := newT(Config{Hooks: Hooks{
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
	fail(tr, 0, 5)

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

		if tr.Usable(0) != Unusable || tr.TryProbe(0) {
			t.Fatal("must still be ejected")
		}

		now++
		tr.Tick(now)

		if s, _, _ := tr.PeerState(0); s != StateProbing || tr.Usable(0) != Unusable {
			t.Fatalf("want probing, got %v", s)
		}

		if !tr.TryProbe(0) || tr.TryProbe(0) {
			t.Fatal("exactly one probe")
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
	tr.TryProbe(0)
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

func TestProbeClaimConcurrent(t *testing.T) {
	for round := range 50 {
		tr := newT(Config{})
		trackN(tr, 20, 4)
		tr.Tick(0)
		fail(tr, 3, 5)
		tr.Tick(1)
		tr.Tick(400 * sec)

		var (
			wins atomic.Int32
			wg   sync.WaitGroup
		)

		for range 8 {
			wg.Go(func() {
				for range 100 {
					if tr.TryProbe(3) {
						wins.Add(1)
					}
				}
			})
		}

		wg.Wait()

		if wins.Load() != 1 {
			t.Fatalf("round %d: %d winners", round, wins.Load())
		}
	}
}

func TestMaxEjectionPercent(t *testing.T) {
	tr := newT(Config{})
	trackN(tr, 50, 1000) // one cell, so only the percent limit applies
	tr.Tick(0)

	for i := range 50 {
		fail(tr, uint32(i), 5)
	}

	if c := tr.Counts(); c.Ejected != 5 {
		t.Fatalf("ejected %d want 5", c.Ejected)
	}

	// At least one may always be ejected.
	small := newT(Config{})
	trackN(small, 3, 3)
	fail(small, 0, 5)
	fail(small, 1, 5)

	if c := small.Counts(); c.Ejected != 1 {
		t.Fatalf("small: ejected %d want 1", c.Ejected)
	}
}

func TestLastUsableInCell(t *testing.T) {
	tr := newT(Config{MaxEjectionPercent: 100, PanicThreshold: 100})
	trackN(tr, 8, 2) // cells of 2
	fail(tr, 0, 5)
	fail(tr, 1, 5)

	if tr.Usable(0) != Unusable || tr.Usable(1) != Healthy {
		t.Fatal("last member of a cell must not be ejected")
	}

	// Move slot 1 (same key) into cell 1, which then has three usable
	// members; two of them may be ejected.
	tr.Track(1, 1001, 1)
	fail(tr, 1, 5)

	if tr.Usable(1) != Unusable {
		t.Fatal("slot 1 has peers in its new cell")
	}

	fail(tr, 2, 5)
	fail(tr, 3, 5)

	if c := tr.Counts(); c.Ejected != 3 || tr.Usable(3) != Healthy {
		t.Fatalf("ejected %d want 3", c.Ejected)
	}
}

func TestSuccessRate(t *testing.T) {
	var reasons []Reason

	tr := newT(Config{ConsecutiveFailures: 1000, Hooks: Hooks{OnEject: func(_ uint32, r Reason, _ int, _ time.Duration) {
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
	tr := newT(Config{FailurePercentageThreshold: 100})
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
	tr := newT(Config{ConsecutiveFailures: 1000})
	trackN(tr, 20, 4)
	tr.Tick(0)

	obs := func(slot uint32, succ, fails int) {
		for range succ {
			tr.Observe(slot, Success, 0)
		}

		fail(tr, slot, fails)
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

	tr := newT(Config{MaxEjectionPercent: 100, Hooks: Hooks{OnPanic: func(p bool) { panics = append(panics, p) }}})
	trackN(tr, 10, 10)

	for i := range 5 {
		fail(tr, uint32(i), 5)
	}

	tr.Tick(0)

	if tr.Panic() {
		t.Fatal("5/10 is not more than 50%")
	}

	fail(tr, 5, 5)
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

	tr := newT(Config{Hooks: Hooks{OnDegraded: func(uint32, bool) { flips++ }}})
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
	tr2 := newT(Config{})
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
	tr := newT(Config{})
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
	tr := newT(Config{})
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
	tr := newT(Config{})
	tr.Tick(0)

	for i := range uint64(16000) {
		tr.NoteRouted(i)
	}

	got := 0

	for i := range uint64(16000) {
		if tr.TryHedge(i) {
			got++
		}
	}

	if got != 800 {
		t.Fatalf("hedges %d want 800", got)
	}

	tr.Tick(10 * sec)

	if tr.TryHedge(0) {
		t.Fatal("budget resets with routed count")
	}

	for range 20 {
		tr.NoteRouted(0)
	}

	if !tr.TryHedge(0) || tr.TryHedge(0) {
		t.Fatal("1 hedge per 20 routed")
	}
}

func TestUntrackRetrack(t *testing.T) {
	tr := newT(Config{})
	trackN(tr, 20, 4)
	tr.Observe(3, Success, 40*time.Millisecond)
	fail(tr, 3, 5)

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

	if tr.TryProbe(3) {
		t.Fatal("untracked cannot probe")
	}
}

func TestTableManySlots(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	tr := newT(Config{})
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
		tr := NewTracker(Config{MaxEjectionPercent: 30}, rand.New(rand.NewPCG(seed, 2)))
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
			case 3:
				tr.TryProbe(rng.Uint32N(64))
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
	tr := newT(Config{})
	trackN(tr, 250, 4)
	tr.Tick(0)

	i := uint32(0)
	for b.Loop() {
		tr.Observe(i%250, Success, time.Millisecond)
		i++
	}
}

func BenchmarkObserveParallel(b *testing.B) {
	tr := newT(Config{})
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
	tr := newT(Config{})
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
	tr := newT(Config{})

	b.RunParallel(func(pb *testing.PB) {
		h := rand.Uint64()
		for pb.Next() {
			tr.NoteRouted(h)
			tr.TryHedge(h)
			h++
		}
	})
}

func TestConcurrentHotAndControl(t *testing.T) {
	tr := newT(Config{})
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
				tr.TryProbe(s)
				tr.HedgeAfter(s)
				tr.NoteRouted(uint64(s))
				tr.TryHedge(uint64(s))
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
