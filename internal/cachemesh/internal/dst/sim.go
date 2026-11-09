// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package dst is the deterministic simulation substrate for cachemesh.
//
// It provides the generic pieces the simulation suite plugs the library
// into, and the simulated cluster built from them:
//
//   - sim.go: a discrete-event scheduler with a simulated nanosecond clock,
//     per-member clocks with offset and drift, and an event trace hash for
//     run-twice determinism checks.
//   - net.go: a network model with latency, jitter, loss, duplication,
//     partitions and paused members, a request/response RPC layer, an
//     in-memory Lease server with compare-and-swap semantics, and a member
//     process table.
//   - scenario.go: the swarm scenario generator and workload key generator.
//   - cluster.go: the simulated cluster, the control-plane invariants and
//     the state coverage registry.
//   - dataplane.go: the data-plane workload and its invariants.
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
	"encoding/binary"
	"math/bits"
	"math/rand/v2"
	"time"

	"github.com/zeebo/xxh3"

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
	steps   uint64
	streams map[string]*rand.Rand
}

// New returns a scheduler at simulated time 0 whose randomness derives from
// seed.
func New(seed uint64) *Sim {
	return &Sim{seed: seed, streams: make(map[string]*rand.Rand)}
}

// Seed returns the run seed.
func (s *Sim) Seed() uint64 { return s.seed }

// Now returns the simulated time in nanoseconds.
func (s *Sim) Now() int64 { return s.now }

// Steps returns how many events have run.
func (s *Sim) Steps() uint64 { return s.steps }

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

// Epoch is the wall time at simulated time 0.
var Epoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// MaxDriftPPB bounds the magnitude of a clock's drift (50%).
const MaxDriftPPB = 500_000_000

const nsPerSec = 1_000_000_000

// Clock is one member's view of time. Its rate differs from simulated time
// by DriftPPB parts per billion, its monotonic reading starts at an
// arbitrary base, and its wall reading is offset from the true simulated
// wall time. The library under test must only ever see its own member's
// Clock.
//
// Changing the drift re-bases the clock at the current simulated time, so
// Mono never decreases. Changing the offset steps the wall clock only.
type Clock struct {
	anchor   int64 // simulated time of the last re-base
	monoBase int64 // Mono at anchor
	wallBase int64 // wall nanoseconds since Epoch at anchor
	offset   int64
	ppb      int64
}

// NewClock returns a clock whose Mono reads monoBase at simulated time 0,
// with no offset and no drift.
func NewClock(monoBase int64) *Clock {
	return &Clock{monoBase: monoBase}
}

// elapsed scales a non-negative simulated duration by the drift without
// overflowing for any int64 duration.
func (c *Clock) elapsed(dt int64) int64 {
	q, r := dt/nsPerSec, dt%nsPerSec

	return dt + q*c.ppb + r*c.ppb/nsPerSec
}

func (c *Clock) since(now int64) int64 {
	if now < c.anchor {
		now = c.anchor
	}

	return c.elapsed(now - c.anchor)
}

// Mono returns the member's monotonic reading at simulated time now. It
// never decreases as now increases.
func (c *Clock) Mono(now int64) int64 { return c.monoBase + c.since(now) }

// Wall returns the member's wall time at simulated time now.
func (c *Clock) Wall(now int64) time.Time {
	return Epoch.Add(time.Duration(c.wallBase + c.since(now)))
}

// DriftPPB returns the current drift.
func (c *Clock) DriftPPB() int64 { return c.ppb }

// Offset returns the current wall offset.
func (c *Clock) Offset() time.Duration { return time.Duration(c.offset) }

func (c *Clock) rebase(now int64) {
	if now < c.anchor {
		now = c.anchor
	}

	e := c.since(now)
	c.monoBase += e
	c.wallBase += e
	c.anchor = now
}

// SetDrift changes the drift from simulated time now on. ppb is clamped to
// [-MaxDriftPPB, MaxDriftPPB].
func (c *Clock) SetDrift(now, ppb int64) {
	c.rebase(now)
	c.ppb = max(-MaxDriftPPB, min(MaxDriftPPB, ppb))
}

// SetOffset steps the wall clock so it reads off ahead of a drift-free
// clock re-based at now. Mono is unaffected.
func (c *Clock) SetOffset(now int64, off time.Duration) {
	c.rebase(now)
	c.wallBase += int64(off) - c.offset
	c.offset = int64(off)
}

// SimTime returns the earliest simulated time at or after the last re-base
// at which Mono reaches mono, under the current drift. Use it to schedule a
// member's timer; if the drift changes before the timer fires, the harness
// must convert again. Deadlines already reached map to the re-base time,
// which the scheduler clamps to Now.
func (c *Clock) SimTime(mono int64) int64 {
	d := mono - c.monoBase
	if d <= 0 {
		return c.anchor
	}

	hi, lo := bits.Mul64(uint64(d), nsPerSec)
	q, _ := bits.Div64(hi, lo, uint64(nsPerSec+c.ppb))
	dt := int64(q)

	for c.elapsed(dt) < d {
		dt++
	}

	for dt > 0 && c.elapsed(dt-1) >= d {
		dt--
	}

	return c.anchor + dt
}

// Trace is a running hash of recorded events. Two runs of the same seed must
// produce the same Sum; a mismatch means something nondeterministic leaked
// into the simulation.
type Trace struct {
	h   *xxh3.Hasher
	buf [26]byte
	n   uint64
}

// NewTrace returns an empty trace.
func NewTrace() *Trace { return &Trace{h: xxh3.New()} }

// Add records an event of the given kind with three arguments. It does not
// allocate.
func (t *Trace) Add(kind uint16, a, b, c uint64) {
	binary.LittleEndian.PutUint16(t.buf[0:], kind)
	binary.LittleEndian.PutUint64(t.buf[2:], a)
	binary.LittleEndian.PutUint64(t.buf[10:], b)
	binary.LittleEndian.PutUint64(t.buf[18:], c)
	t.write(t.buf[:])
	t.n++
}

// write feeds p to the hash. xxh3.Hasher.Write never fails.
func (t *Trace) write(p []byte) {
	if _, err := t.h.Write(p); err != nil {
		panic(err)
	}
}

// Len returns the number of records added.
func (t *Trace) Len() uint64 { return t.n }

// Sum returns the 64-bit hash of everything recorded so far.
func (t *Trace) Sum() uint64 { return t.h.Sum64() }
