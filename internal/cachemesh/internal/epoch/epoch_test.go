// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package epoch

import (
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

func TestZeroValue(t *testing.T) {
	var tr Tracker
	if tr.Current() != 0 || tr.Period() != DefaultPeriod {
		t.Fatalf("zero value: %d %v", tr.Current(), tr.Period())
	}

	if got := tr.Advance(int64(31 * time.Second)); got != 2 {
		t.Fatalf("advance: got %d want 2", got)
	}

	if got := tr.Advance(-5); got != 2 {
		t.Fatalf("advance backward: got %d want 2", got)
	}
}

func TestAnchorForwardAndBackward(t *testing.T) {
	var tr Tracker

	p := 10 * time.Second
	if got := tr.Anchor(100, p, 0); got != 100 {
		t.Fatalf("anchor: %d", got)
	}

	if got := tr.Advance(int64(25 * time.Second)); got != 102 {
		t.Fatalf("advance: %d", got)
	}
	// A stale (behind) anchor is ignored: the phase is kept, no stall.
	if got := tr.Anchor(101, p, int64(26*time.Second)); got != 102 {
		t.Fatalf("backward anchor: %d", got)
	}

	if got := tr.Advance(int64(35 * time.Second)); got != 103 {
		t.Fatalf("kept phase: %d", got)
	}

	// An anchor for the epoch the tracker already computes keeps the phase
	// too, even though it arrives late in the epoch.
	if got := tr.Anchor(103, p, int64(39*time.Second)); got != 103 {
		t.Fatalf("same-epoch anchor: %d", got)
	}

	if got := tr.Advance(int64(40 * time.Second)); got != 104 {
		t.Fatalf("same-epoch anchor moved the phase: %d", got)
	}
	// Forward jump.
	if got := tr.Anchor(200, p, int64(47*time.Second)); got != 200 {
		t.Fatalf("forward anchor: %d", got)
	}
	// Anchor in the future relative to now (clock read reordered): no change.
	if got := tr.Advance(int64(30 * time.Second)); got != 200 {
		t.Fatalf("before anchor: %d", got)
	}
}

func TestPeriodChange(t *testing.T) {
	var tr Tracker

	tr.Anchor(0, 10*time.Second, 0)
	tr.Advance(int64(20 * time.Second))
	tr.Anchor(2, time.Second, int64(20*time.Second))

	if got := tr.Advance(int64(25 * time.Second)); got != 7 {
		t.Fatalf("got %d want 7", got)
	}

	if tr.Period() != time.Second {
		t.Fatal(tr.Period())
	}

	tr.Anchor(7, 0, int64(25*time.Second))

	if tr.Period() != DefaultPeriod {
		t.Fatal("non-positive period must default")
	}
}

func TestStampFor(t *testing.T) {
	cases := []struct {
		cur    uint64
		ttl, p time.Duration
		want   uint64
	}{
		{10, 0, time.Second, Never},
		{10, 5 * time.Minute, 15 * time.Second, 10 + 20 + 1},
		{10, 5*time.Minute + 1, 15 * time.Second, 10 + 21 + 1},
		{10, 5 * time.Minute, 0, 10 + 20 + 1},
		{math.MaxUint64 - 3, 5 * time.Minute, 15 * time.Second, Never},
	}
	for _, c := range cases {
		if got := StampFor(c.cur, c.ttl, c.p, 3); got.Expires != c.want || got.Gen != 3 {
			t.Errorf("StampFor(%d,%v,%v)=%+v want %d", c.cur, c.ttl, c.p, got, c.want)
		}
	}
}

func TestFresh(t *testing.T) {
	s := Stamp{Expires: 5, Gen: 1}
	if !Fresh(s, 4, 1) || Fresh(s, 5, 1) || Fresh(s, 4, 2) {
		t.Fatal("fresh mismatch")
	}

	if !Fresh(StampFor(math.MaxUint64-1, 0, 0, 0), math.MaxUint64-1, 0) {
		t.Fatal("never-expiring stamp must be fresh")
	}
}

// TestLifetimeBounds checks, with randomized issue times and TTLs, that a
// stamp issued at local time t stays fresh for at least ttl and at most
// ceil(ttl/period)*period + period (ttl+period when ttl is a multiple of the
// period) on a tracker whose clock matches the anchor.
func TestLifetimeBounds(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		period := time.Duration(1+rng.IntN(30)) * time.Second
		ttl := MinTTL + time.Duration(rng.Int64N(int64(time.Hour)))

		var tr Tracker

		anchor := rng.Int64N(int64(time.Hour))
		tr.Anchor(rng.Uint64N(1<<40), period, anchor)

		issue := anchor + rng.Int64N(int64(time.Hour))
		s := StampFor(tr.Advance(issue), ttl, period, 0)
		// First instant (on a 1ns grid) at which the stamp is stale.
		stale := anchor + int64(s.Expires-tr.anchorEpoch)*int64(period)

		var probe Tracker

		probe.Anchor(tr.anchorEpoch, period, anchor)

		if !Fresh(s, probe.Advance(stale-1), 0) || Fresh(s, probe.Advance(stale), 0) {
			t.Fatalf("stale boundary wrong")
		}

		life := time.Duration(stale - issue)

		hi := time.Duration(int64((ttl+period-1)/period)+1) * period
		if life < ttl || life > hi || hi >= ttl+2*period {
			t.Fatalf("lifetime %v not in [%v,%v]", life, ttl, hi)
		}

		if ttl%period == 0 && hi != ttl+period {
			t.Fatalf("multiple-of-period ttl bound %v", hi)
		}
	}
}

func TestConcurrentReaders(t *testing.T) {
	var (
		tr Tracker
		wg sync.WaitGroup
	)

	stop := make(chan struct{})

	for range 4 {
		wg.Go(func() {
			var last uint64

			for {
				select {
				case <-stop:
					return
				default:
				}

				c := tr.Current()
				if c < last {
					t.Errorf("decreased %d -> %d", last, c)
					return
				}

				last = c
			}
		})
	}

	for i := range 10000 {
		now := int64(i) * int64(time.Second)
		if i%100 == 0 {
			tr.Anchor(uint64(i/20), 15*time.Second, now)
		}

		tr.Advance(now)
	}

	close(stop)
	wg.Wait()
}

func BenchmarkFresh(b *testing.B) {
	s := Stamp{Expires: 100, Gen: 1}

	var tr Tracker

	n := 0

	for b.Loop() {
		if Fresh(s, tr.Current(), 1) {
			n++
		}
	}

	_ = n
}

func BenchmarkAdvance(b *testing.B) {
	var tr Tracker

	i := int64(0)
	for b.Loop() {
		i += int64(time.Millisecond)
		tr.Advance(i)
	}
}

func TestStampForExtremes(t *testing.T) {
	// A TTL near MaxInt64 must not overflow ttl+period-1 into a tiny or
	// wrapped stamp.
	s := StampFor(10, time.Duration(math.MaxInt64), DefaultPeriod, 0)
	if want := uint64(10 + math.MaxInt64/int64(DefaultPeriod) + 2); s.Expires != want {
		t.Fatalf("huge ttl: Expires = %d, want %d", s.Expires, want)
	}

	if s := StampFor(5, -time.Second, DefaultPeriod, 0); Fresh(s, 5, 0) {
		t.Fatalf("negative ttl must not be fresh: %+v", s)
	}

	if s := StampFor(Never-3, MinTTL, DefaultPeriod, 0); s.Expires != Never {
		t.Fatalf("saturation: Expires = %d", s.Expires)
	}
}

func TestAdvanceSaturates(t *testing.T) {
	var tr Tracker

	tr.Anchor(Never-1, time.Nanosecond, 0)

	if got := tr.Advance(1 << 40); got != Never {
		t.Fatalf("Advance = %d, want saturation at %d", got, uint64(Never))
	}
}

// TestPeriodConcurrent is meaningful under -race: Period is read on the
// request path while the tick goroutine anchors.
func TestPeriodConcurrent(t *testing.T) {
	var (
		tr Tracker
		wg sync.WaitGroup
	)

	wg.Go(func() {
		for i := range 1000 {
			tr.Anchor(uint64(i), time.Duration(1+i%3)*time.Second, int64(i))
		}
	})

	for range 1000 {
		if p := tr.Period(); p <= 0 {
			t.Fatalf("Period = %v", p)
		}
	}

	wg.Wait()
}

// TestStaleAnchorsDoNotStretchEpochs is a regression test: each anchor used
// to reset the phase to the time it was applied, so a heartbeat replayed
// late (a catch-up delta, a snapshot, a new leader whose own phase was
// late) delayed every later epoch boundary by up to a period. Repeated
// across leader changes this stretched stamp lifetimes without bound (found
// by the DST data-plane workload).
func TestStaleAnchorsDoNotStretchEpochs(t *testing.T) {
	const p = 15 * time.Second

	var tr Tracker

	tr.Anchor(0, p, 0)

	// Every period a heartbeat for the epoch that just began arrives 14s
	// late. The phase must stay at the first anchor's.
	for e := uint64(1); e <= 40; e++ {
		at := int64(e)*int64(p) + int64(14*time.Second)
		if got := tr.Anchor(e, p, at); got != e {
			t.Fatalf("epoch %d: got %d", e, got)
		}
	}

	if got := tr.Advance(41 * int64(p)); got != 41 {
		t.Fatalf("after 41 periods: %d, want 41", got)
	}

	// A member that is behind still catches up at once.
	var lag Tracker

	lag.Anchor(0, p, 0)

	if got := lag.Anchor(10, p, int64(20*time.Second)); got != 10 {
		t.Fatalf("catch up: %d", got)
	}
}
