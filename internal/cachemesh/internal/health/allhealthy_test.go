// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

func allUsableHealthy(tr *Tracker, n int) bool {
	for s := range uint32(n) {
		if tr.Usable(s) != Healthy {
			return false
		}
	}

	return true
}

func TestAllHealthyTransitions(t *testing.T) {
	tr := newT(Config{})
	trackN(tr, 20, 4)
	tr.Tick(0)

	if !tr.AllHealthy() {
		t.Fatal("fresh tracker must be all healthy")
	}

	// Ejection clears it; probing keeps it clear; a probe success sets it.
	fail(tr, 3, 5)

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
	fail(tr, 7, 5)

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
	tr := newT(Config{})
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
	tr := newT(Config{})
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
	tr := newT(Config{MaxEjectionPercent: 100, PanicThreshold: 10})
	trackN(tr, 10, 5)
	tr.Tick(0)
	fail(tr, 0, 5)
	fail(tr, 5, 5)
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
		tr := newT(Config{ConsecutiveFailures: 2, BaseEjection: time.Second, MaxEjection: 4 * time.Second})

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
	tr := newT(Config{})
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
	tr := newT(Config{})
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
