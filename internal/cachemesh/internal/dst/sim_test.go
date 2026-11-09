// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"testing"
	"time"
)

type recTarget struct{ got []uint64 }

func (r *recTarget) Fire(a, b uint64) { r.got = append(r.got, a*100+b) }

// drain runs every pending event.
func drain(s *Sim) {
	for s.Step() {
	}
}

func TestSimOrdering(t *testing.T) {
	s := New(1)

	var got []int

	s.At(20, func() { got = append(got, 3) })
	s.At(10, func() { got = append(got, 1) })
	s.At(10, func() { got = append(got, 2) })
	s.At(30, func() {
		got = append(got, 4)

		s.After(0, func() { got = append(got, 6) })
		s.At(5, func() { got = append(got, 7) }) // past: clamps to now, after 6
	})
	s.At(30, func() { got = append(got, 5) })

	drain(s)

	want := []int{1, 2, 3, 4, 5, 6, 7}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}

	if s.Now() != 30 {
		t.Fatalf("now = %d", s.Now())
	}
}

func TestSimStabilityManySameTime(t *testing.T) {
	s := New(2)
	tg := &recTarget{}

	for i := range 1000 {
		s.AtTarget(int64(i%3), tg, uint64(i), 0)
	}

	drain(s)

	var want []uint64

	for tm := range 3 {
		for i := range 1000 {
			if i%3 == tm {
				want = append(want, uint64(i)*100)
			}
		}
	}

	for i := range want {
		if tg.got[i] != want[i] {
			t.Fatalf("event %d: got %d want %d", i, tg.got[i], want[i])
		}
	}
}

func TestSimCancel(t *testing.T) {
	s := New(3)

	var ran []int

	h1 := s.After(10, func() { ran = append(ran, 1) })
	h2 := s.After(20, func() { ran = append(ran, 2) })
	s.After(30, func() { ran = append(ran, 3) })

	if !s.Cancel(h2) || s.Cancel(h2) {
		t.Fatal("cancel should succeed once")
	}

	s.RunUntil(15)

	if s.Cancel(h1) {
		t.Fatal("cancel of run event")
	}

	// Reuse of the canceled slot must not let the stale handle cancel it.
	h4 := s.After(1, func() { ran = append(ran, 4) })
	if s.Cancel(h2) {
		t.Fatal("stale handle canceled a new event")
	}

	_ = h4

	drain(s)

	if len(ran) != 3 || ran[0] != 1 || ran[1] != 4 || ran[2] != 3 {
		t.Fatalf("ran %v", ran)
	}

	if s.Cancel(Handle{}) {
		t.Fatal("zero handle")
	}
}

func TestSimRunUntilAdvancesClock(t *testing.T) {
	s := New(4)
	s.At(100, func() {})
	s.RunFor(50)

	if next, ok := s.Next(); s.Now() != 50 || !ok || next != 100 {
		t.Fatalf("now %d next %d %v", s.Now(), next, ok)
	}

	s.RunUntil(100)

	if s.Steps() != 1 {
		t.Fatal("event at boundary should run")
	}

	if _, ok := s.Next(); ok {
		t.Fatal("next on empty")
	}
}

func TestSimHeapRandom(t *testing.T) {
	s := New(5)
	r := s.Stream("test")

	var handles []Handle

	last := int64(-1)
	check := func() {
		if s.Now() < last {
			t.Fatal("time went backwards")
		}

		last = s.Now()
	}

	for i := range 20000 {
		at := r.Int64N(1_000_000)
		handles = append(handles, s.At(at, check))

		if i%3 == 0 {
			s.Cancel(handles[r.IntN(len(handles))])
		}
	}

	drain(s)

	if _, ok := s.Next(); ok {
		t.Fatal("pending after run")
	}
}

func TestStreamsIndependent(t *testing.T) {
	a, b := New(9), New(9)
	_ = a.Stream("net").Uint64()
	_ = a.Stream("net").Uint64()

	if a.Stream("workload").Uint64() != b.Stream("workload").Uint64() {
		t.Fatal("draws on one stream perturbed another")
	}

	if New(9).Stream("sim").Uint64() == New(10).Stream("sim").Uint64() {
		t.Fatal("different seeds gave the same draw")
	}
}

func TestTrace(t *testing.T) {
	run := func(seed uint64) uint64 {
		s := New(seed)
		tr := NewTrace()
		r := s.Stream("x")

		for i := range 100 {
			s.After(r.Int64N(1000), func() { tr.Add(1, uint64(i), uint64(s.Now()), 0) })
		}

		drain(s)

		return tr.Sum()
	}

	if first := run(1); first != run(1) {
		t.Fatal("same seed, different trace")
	}

	if run(1) == run(2) {
		t.Fatal("different seed, same trace")
	}

	tr := NewTrace()

	if n := testing.AllocsPerRun(1000, func() { tr.Add(1, 2, 3, 4) }); n != 0 {
		t.Fatalf("Add allocates %v", n)
	}
}

func TestClockDrift(t *testing.T) {
	c := NewClock(12345)
	if c.Mono(0) != 12345 {
		t.Fatal("base")
	}

	c.SetDrift(0, 1000) // +1ppm

	h := int64(time.Hour)
	if got := c.Mono(h) - 12345; got != h+h/1_000_000 {
		t.Fatalf("drift elapsed %d", got)
	}

	prev := c.Mono(h)

	for i, ppb := range []int64{-MaxDriftPPB, 0, MaxDriftPPB, -7, 2 * MaxDriftPPB} {
		now := h + int64(i+1)*int64(time.Minute)
		c.SetDrift(now, ppb)

		for dt := int64(0); dt < 3*nsPerSec; dt += 333_333_333 {
			m := c.Mono(now + dt)
			if m < prev {
				t.Fatalf("mono decreased: %d < %d", m, prev)
			}

			prev = m
		}
	}

	if c.DriftPPB() != MaxDriftPPB {
		t.Fatal("drift not clamped")
	}

	// Long runs do not overflow.
	c.SetDrift(0, MaxDriftPPB)

	if c.Mono(1000*h) < c.Mono(999*h) {
		t.Fatal("overflow")
	}
}

func TestClockMonoStepwise(t *testing.T) {
	c := NewClock(0)
	c.SetDrift(0, -MaxDriftPPB+1)

	prev := c.Mono(0)
	for now := int64(0); now < 3*nsPerSec; now += 7_777_777 {
		if m := c.Mono(now); m < prev {
			t.Fatal("decreased")
		} else {
			prev = m
		}
	}
}

func TestClockWallAndSimTime(t *testing.T) {
	c := NewClock(-5)

	if !c.Wall(0).Equal(Epoch) {
		t.Fatal("epoch")
	}

	c.SetOffset(int64(time.Second), 3*time.Second)

	if got := c.Wall(int64(2 * time.Second)); !got.Equal(Epoch.Add(5 * time.Second)) {
		t.Fatalf("wall %v", got)
	}

	before := c.Mono(int64(2 * time.Second))
	c.SetOffset(int64(2*time.Second), -time.Second)

	if c.Mono(int64(2*time.Second)) != before {
		t.Fatal("offset moved mono")
	}

	if c.Offset() != -time.Second {
		t.Fatal("offset")
	}

	for _, ppb := range []int64{0, 1, 999, -1_234_567, MaxDriftPPB, -MaxDriftPPB} {
		c.SetDrift(int64(10*time.Second), ppb)

		for _, d := range []int64{1, 999, nsPerSec, 12345678901, 3600 * nsPerSec} {
			target := c.Mono(int64(10*time.Second)) + d
			at := c.SimTime(target)

			if c.Mono(at) < target || c.Mono(at-1) >= target {
				t.Fatalf("ppb %d d %d: SimTime %d not earliest", ppb, d, at)
			}
		}

		if c.SimTime(0) != int64(10*time.Second) {
			t.Fatal("past deadline")
		}
	}
}

func TestHistogram(t *testing.T) {
	var h Histogram

	for i := int64(1); i <= 100_000; i++ {
		h.Record(i * 1000)
	}

	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		want := q * 100_000 * 1000
		got := float64(h.Quantile(q))

		if got < want*0.98 || got > want*1.02 {
			t.Fatalf("q%v = %v want %v", q, got, want)
		}
	}

	var o Histogram
	o.Record(-5)
	o.Record(1 << 62)
	h.Merge(&o)

	if h.Count() != 100_002 || h.Min() != 0 || h.Max() != 1<<62 || h.Quantile(1) != 1<<62 {
		t.Fatalf("merge: %s", h.String())
	}

	if (&Histogram{}).Quantile(0.5) != 0 {
		t.Fatal("empty")
	}

	for v := range uint64(1 << 20) {
		lo, hi := histBounds(histIndex(v))
		if v < lo || v > hi {
			t.Fatalf("bounds %d: %d..%d", v, lo, hi)
		}
	}
}

func TestCounters(t *testing.T) {
	var a, b Counters

	*a.Counter("x") += 2

	b.Add("x", 3)
	b.Add("a", 1)
	a.Merge(&b)

	if a.Get("x") != 5 || a.Get("a") != 1 || a.Get("none") != 0 {
		t.Fatal(a.String())
	}

	if a.String() != "a=1\nx=5\n" {
		t.Fatal(a.String())
	}
}

func TestCoverage(t *testing.T) {
	c := NewCoverage()
	c.Register("b", "a", "c")
	h := c.Handle("a")
	h.Hit()
	h.Hit()
	c.Hit("extra")

	if got := c.Missing(); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("missing %v", got)
	}

	o := NewCoverage()
	o.Register("d")
	o.Hit("b")
	c.Merge(o)

	if got := c.Missing(); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("missing %v", got)
	}

	if h.Count() != 2 || c.Count("b") != 1 {
		t.Fatal("counts")
	}

	if c.Report() == "" {
		t.Fatal("report")
	}

	if n := testing.AllocsPerRun(100, h.Hit); n != 0 {
		t.Fatal("Hit allocates")
	}
}
