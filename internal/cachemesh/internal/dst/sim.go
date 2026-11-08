// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package dst is the deterministic simulation substrate for cachemesh.
//
// It provides the generic pieces the simulation suite plugs the library
// into: a discrete-event scheduler with a simulated nanosecond clock, an
// event trace hash for run-twice determinism checks, per-member clocks with
// offset and drift, a network model with latency, jitter, loss,
// duplication, partitions and paused members, a request/response RPC layer,
// an in-memory Lease server with compare-and-swap semantics and QPS
// accounting, a member process table, a swarm scenario generator, a state
// coverage registry, latency histograms, workload key generators, and
// helpers for selecting seeds from the environment.
//
// # Determinism
//
// Everything in this package is a pure function of the seed passed to New
// and the calls made by the harness. Nothing reads the wall or monotonic
// clock, nothing uses the global math/rand source, no goroutines, timers or
// channels are used, and no output depends on Go map iteration order. Events
// scheduled for the same simulated time run in the order they were
// scheduled.
//
// Randomness comes from named sub-streams (Sim.Stream). Each stream is an
// independent PCG generator seeded from the run seed and the stream name, so
// extra draws in one component (for example the network) do not perturb the
// draws seen by another (for example the scenario or the workload). Adding a
// new consumer therefore does not change the behavior of existing ones for a
// given seed.
//
// # Allocation
//
// The scheduler is a 4-ary min-heap of small value entries plus a slab of
// event payloads with a free list, so scheduling does not allocate in steady
// state. Besides closures (At, After) events can carry a Target interface
// and two uint64 arguments (AtTarget, AfterTarget), which avoids a closure
// allocation on hot paths such as message delivery. Cancellation is O(1)
// and lazy: the heap entry stays until it reaches the top and is skipped.
package dst

import (
	"math/rand/v2"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
)

// Target is an allocation-free event receiver. Fire is called with the two
// arguments given when the event was scheduled.
type Target interface {
	Fire(a, b uint64)
}

// Handle identifies a scheduled event so it can be canceled. The zero
// Handle refers to no event.
type Handle struct {
	slot uint32
	seq  uint64
}

// Valid reports whether h refers to a scheduled event (it may since have
// run or been canceled).
func (h Handle) Valid() bool { return h.seq != 0 }

type heapEntry struct {
	t    int64
	seq  uint64
	slot uint32
}

type slabEntry struct {
	seq uint64 // 0 when the slot is free or canceled
	fn  func()
	tgt Target
	a   uint64
	b   uint64
}

// Sim is a discrete-event scheduler with a simulated clock in nanoseconds.
type Sim struct {
	seed    uint64
	now     int64
	seq     uint64
	heap    []heapEntry
	slab    []slabEntry
	free    []uint32
	live    int
	steps   uint64
	rng     *rand.Rand
	streams map[string]*rand.Rand
}

// New returns a scheduler at simulated time 0 whose randomness derives from
// seed.
func New(seed uint64) *Sim {
	s := &Sim{seed: seed, streams: make(map[string]*rand.Rand)}
	s.rng = s.Stream("sim")

	return s
}

// Seed returns the run seed.
func (s *Sim) Seed() uint64 { return s.seed }

// Now returns the simulated time in nanoseconds.
func (s *Sim) Now() int64 { return s.now }

// Steps returns how many events have run.
func (s *Sim) Steps() uint64 { return s.steps }

// Rand returns the scheduler's default random stream ("sim").
func (s *Sim) Rand() *rand.Rand { return s.rng }

// Stream returns the named random stream, creating it on first use. A
// stream's sequence depends only on the run seed and its name, never on
// draws made from other streams. Repeated calls with the same name return
// the same generator.
func (s *Sim) Stream(name string) *rand.Rand {
	if r, ok := s.streams[name]; ok {
		return r
	}

	r := NewRand(s.seed, name)
	s.streams[name] = r

	return r
}

// NewRand returns a PCG generator derived from seed and name, independent
// of any Sim.
func NewRand(seed uint64, name string) *rand.Rand {
	return rand.New(rand.NewPCG(hashing.Mix64(seed), hashing.Seed(name)))
}

// Pending returns the number of scheduled, not canceled events.
func (s *Sim) Pending() int { return s.live }

// At schedules fn at simulated time t. Times in the past run at Now.
func (s *Sim) At(t int64, fn func()) Handle {
	return s.push(t, slabEntry{fn: fn})
}

// After schedules fn d nanoseconds from now.
func (s *Sim) After(d int64, fn func()) Handle {
	return s.At(s.now+d, fn)
}

// AtTarget schedules tgt.Fire(a, b) at simulated time t without allocating.
func (s *Sim) AtTarget(t int64, tgt Target, a, b uint64) Handle {
	return s.push(t, slabEntry{tgt: tgt, a: a, b: b})
}

// AfterTarget schedules tgt.Fire(a, b) d nanoseconds from now.
func (s *Sim) AfterTarget(d int64, tgt Target, a, b uint64) Handle {
	return s.AtTarget(s.now+d, tgt, a, b)
}

// Cancel cancels the event identified by h. It reports whether the event
// was still pending.
func (s *Sim) Cancel(h Handle) bool {
	if h.seq == 0 || int(h.slot) >= len(s.slab) {
		return false
	}

	e := &s.slab[h.slot]
	if e.seq != h.seq {
		return false
	}

	*e = slabEntry{}
	s.live--

	return true
}

func (s *Sim) push(t int64, e slabEntry) Handle {
	if t < s.now {
		t = s.now
	}

	s.seq++
	e.seq = s.seq

	var slot uint32

	if n := len(s.free); n > 0 {
		slot = s.free[n-1]
		s.free = s.free[:n-1]
		s.slab[slot] = e
	} else {
		slot = uint32(len(s.slab))
		s.slab = append(s.slab, e)
	}

	s.heap = append(s.heap, heapEntry{t: t, seq: e.seq, slot: slot})
	s.up(len(s.heap) - 1)
	s.live++

	return Handle{slot: slot, seq: e.seq}
}

func less(a, b *heapEntry) bool {
	if a.t != b.t {
		return a.t < b.t
	}

	return a.seq < b.seq
}

func (s *Sim) up(i int) {
	h := s.heap
	x := h[i]

	for i > 0 {
		p := (i - 1) / 4
		if !less(&x, &h[p]) {
			break
		}

		h[i] = h[p]
		i = p
	}

	h[i] = x
}

func (s *Sim) down(i int) {
	h := s.heap
	n := len(h)
	x := h[i]

	for {
		c := 4*i + 1
		if c >= n {
			break
		}

		m := c

		end := min(c+4, n)
		for j := c + 1; j < end; j++ {
			if less(&h[j], &h[m]) {
				m = j
			}
		}

		if !less(&h[m], &x) {
			break
		}

		h[i] = h[m]
		i = m
	}

	h[i] = x
}

func (s *Sim) pop() heapEntry {
	h := s.heap
	top := h[0]
	last := len(h) - 1
	h[0] = h[last]
	s.heap = h[:last]

	if last > 0 {
		s.down(0)
	}

	return top
}

// skipDead removes canceled entries from the top of the heap.
func (s *Sim) skipDead() {
	for len(s.heap) > 0 {
		top := s.heap[0]
		if s.slab[top.slot].seq == top.seq {
			return
		}

		s.pop()

		if s.slab[top.slot].seq == 0 {
			s.free = append(s.free, top.slot)
		}
	}
}

// Next returns the time of the next pending event and whether one exists.
func (s *Sim) Next() (int64, bool) {
	s.skipDead()

	if len(s.heap) == 0 {
		return 0, false
	}

	return s.heap[0].t, true
}

// Step runs the next pending event, advancing the clock to its time. It
// reports whether an event ran.
func (s *Sim) Step() bool {
	s.skipDead()

	if len(s.heap) == 0 {
		return false
	}

	top := s.pop()
	e := s.slab[top.slot]
	s.slab[top.slot] = slabEntry{}
	s.free = append(s.free, top.slot)
	s.live--
	s.now = top.t
	s.steps++

	if e.fn != nil {
		e.fn()
	} else {
		e.tgt.Fire(e.a, e.b)
	}

	return true
}

// RunUntil runs every event scheduled at or before t, then sets the clock to
// t (if it is later than Now).
func (s *Sim) RunUntil(t int64) {
	for {
		next, ok := s.Next()
		if !ok || next > t {
			break
		}

		s.Step()
	}

	if t > s.now {
		s.now = t
	}
}

// RunFor runs events for d nanoseconds of simulated time.
func (s *Sim) RunFor(d int64) { s.RunUntil(s.now + d) }

// Run runs until no events are pending or limit events have run (limit <= 0
// means no limit). It returns the number of events run.
func (s *Sim) Run(limit int) int {
	n := 0
	for (limit <= 0 || n < limit) && s.Step() {
		n++
	}

	return n
}
