// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package negcache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func mustDigest(t *testing.T, s string) digest.Digest {
	t.Helper()

	d, err := digest.Parse(s)
	if err != nil {
		t.Fatalf("parse digest %q: %v", s, err)
	}

	return d
}

const (
	dA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeClock returns a deterministic Now func + setter.
func fakeClock(start time.Time) (now func() time.Time, set func(time.Time)) {
	var mu sync.Mutex

	cur := start
	now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		return cur
	}
	set = func(t time.Time) {
		mu.Lock()
		defer mu.Unlock()

		cur = t
	}

	return now, set
}

func TestCooldownLadder_DefaultMultiplier3(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	nowFn, _ := fakeClock(start)
	c := New(Options{
		Initial:    10 * time.Second,
		Max:        10 * time.Minute,
		Multiplier: 3,
		Now:        nowFn,
	})
	d := mustDigest(t, dA)

	// 1st: 10s
	e := c.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 10*time.Second; got != want {
		t.Fatalf("1st cooldown = %v, want %v", got, want)
	}
	// 2nd: 30s
	e = c.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 30*time.Second; got != want {
		t.Fatalf("2nd cooldown = %v, want %v", got, want)
	}
	// 3rd: 90s (still below 10-min cap)
	e = c.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 90*time.Second; got != want {
		t.Fatalf("3rd cooldown = %v, want %v", got, want)
	}
	// 4th: 270s (still below cap)
	e = c.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 270*time.Second; got != want {
		t.Fatalf("4th cooldown = %v, want %v", got, want)
	}
	// 5th: 810s -> capped at 600s (10min).
	e = c.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 10*time.Minute; got != want {
		t.Fatalf("5th cooldown = %v, want %v", got, want)
	}
	// 6th: still capped at 10 min.
	e = c.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 10*time.Minute; got != want {
		t.Fatalf("6th cooldown = %v, want %v", got, want)
	}
}

func TestLookup_ExpiredEntryEvicted(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	nowFn, setNow := fakeClock(start)
	c := New(Options{
		Initial: 10 * time.Second,
		Max:     time.Minute,
		Now:     nowFn,
	})
	d := mustDigest(t, dA)
	c.RecordFailure(d, ifaces.FailureRateLimited)

	if e, ok := c.Lookup(d); !ok || e.Class != ifaces.FailureRateLimited {
		t.Fatalf("Lookup pre-expiry: ok=%v entry=%+v", ok, e)
	}
	// Advance past cooldown.
	setNow(start.Add(11 * time.Second))

	if _, ok := c.Lookup(d); ok {
		t.Fatalf("Lookup post-expiry: want eviction, got hit")
	}

	if got := c.Len(); got != 0 {
		t.Fatalf("Len after eviction = %d, want 0", got)
	}
}

func TestRecordSuccess_ClearsEntry(t *testing.T) {
	c := New(Options{Initial: 10 * time.Second})
	d := mustDigest(t, dA)
	c.RecordFailure(d, ifaces.FailureAuth)

	if _, ok := c.Lookup(d); !ok {
		t.Fatal("entry missing after RecordFailure")
	}

	c.RecordSuccess(d)

	if _, ok := c.Lookup(d); ok {
		t.Fatal("entry still present after RecordSuccess")
	}
	// Subsequent failure starts the ladder from scratch.
	start := time.Unix(1_700_000_000, 0).UTC()
	nowFn, _ := fakeClock(start)
	c2 := New(Options{Initial: 10 * time.Second, Max: time.Minute, Multiplier: 3, Now: nowFn})
	c2.RecordFailure(d, ifaces.FailureTransient)
	c2.RecordSuccess(d)

	e := c2.RecordFailure(d, ifaces.FailureTransient)
	if got, want := e.CooldownUntil.Sub(start), 10*time.Second; got != want {
		t.Fatalf("post-success ladder reset: 1st cooldown = %v, want %v", got, want)
	}
}

func TestRecordSuccess_NoEntry_NoOp(t *testing.T) {
	var sizeCalls int32

	c := New(Options{
		Initial: 10 * time.Second,
		OnSize:  func(int) { atomic.AddInt32(&sizeCalls, 1) },
	})
	d := mustDigest(t, dA)
	c.RecordSuccess(d) // no-op

	if got := atomic.LoadInt32(&sizeCalls); got != 0 {
		t.Fatalf("OnSize called %d times on empty success, want 0", got)
	}
}

func TestCallbacks_FiredOnEnterAndHit(t *testing.T) {
	var enters, hits int32

	c := New(Options{
		Initial: 10 * time.Second,
		OnEnter: func(c ifaces.FailureClass) {
			if c != ifaces.FailureAuth {
				t.Errorf("OnEnter class = %v, want FailureAuth", c)
			}

			atomic.AddInt32(&enters, 1)
		},
		OnHit: func(c ifaces.FailureClass) {
			if c != ifaces.FailureAuth {
				t.Errorf("OnHit class = %v, want FailureAuth", c)
			}

			atomic.AddInt32(&hits, 1)
		},
	})
	d := mustDigest(t, dA)
	c.RecordFailure(d, ifaces.FailureAuth)

	if _, ok := c.Lookup(d); !ok {
		t.Fatal("lookup missed")
	}

	if _, ok := c.Lookup(d); !ok {
		t.Fatal("lookup missed")
	}

	if got := atomic.LoadInt32(&enters); got != 1 {
		t.Fatalf("enters = %d, want 1", got)
	}

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("hits = %d, want 2", got)
	}
}

func TestLen_TracksDistinctDigests(t *testing.T) {
	c := New(Options{Initial: 10 * time.Second})
	dx := mustDigest(t, dA)
	dy := mustDigest(t, dB)

	c.RecordFailure(dx, ifaces.FailureTransient)
	c.RecordFailure(dy, ifaces.FailureTransient)
	c.RecordFailure(dx, ifaces.FailureTransient) // same digest -> no new entry

	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(Options{Initial: 100 * time.Millisecond, Max: time.Second})
	d := mustDigest(t, dA)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)

		go func() {
			defer wg.Done()

			c.RecordFailure(d, ifaces.FailureTransient)
		}()
		go func() {
			defer wg.Done()

			_, _ = c.Lookup(d) //nolint:errcheck // best-effort
		}()
	}

	wg.Wait()
}

func TestDefaults_AppliedWhenZero(t *testing.T) {
	c := New(Options{})
	d := mustDigest(t, dA)

	e := c.RecordFailure(d, ifaces.FailureTransient)
	if d := e.CooldownUntil.Sub(e.LastFailure); d != 10*time.Second {
		t.Fatalf("default Initial = %v, want 10s", d)
	}
}
