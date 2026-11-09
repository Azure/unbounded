// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Neighbor health. This file tracks the health of a member's data-plane neighbors:
// consecutive-failure breakers with a half-open probing state, Envoy-style
// outlier detection, peak-EWMA latency, overload ("busy") marking, ejection
// limits, a panic threshold, and a hedge budget.
//
// # Hot and control paths
//
// Observe, Usable, HedgeAfter, NoteRouted, and TryHedge are the
// hot path. They are safe for concurrent use, take no locks, never read the
// clock, and never allocate. Track, Untrack, and Tick are the control path
// and must be called from a single goroutine (the runtime tick, or a
// simulated node). HealthHooks are invoked only from Tick and Track/Untrack.
//
// Peers are identified by a dense slot number plus a member key. Slots can
// reach about 1M, but a HealthTracker follows only a few hundred neighbors and the
// simulator runs up to 100k Trackers in one process, so per-tracker memory is
// O(tracked), never O(max slot): a small open-addressing table (slot to
// *Peer) is rebuilt copy-on-write on Track/Untrack and published through an
// atomic pointer. Re-tracking a slot with a different key resets its state;
// re-tracking with the same key only updates its cell.
//
// Track and Untrack rebuild the table, O(tracked) each. Use Sync (exact
// set) or TrackMany (additive) to change many peers with one rebuild; a
// member bootstrapping hundreds of neighbors one Track at a time pays
// O(n^2).
//
// Untracked slots are reported Healthy by Usable, and Observe on them is a
// cheap no-op: health only filters among neighbors this member tracks.
//
// Observe keeps contended writes low because every member may report on
// the same peer from many goroutines: a Success writes one shared counter
// (the interval bucket) plus, for every success up to 64 per interval and
// one in 8 after that, the latency mean the EWMA decays toward. The peak
// EWMA and the consecutive-failure counter are written only when they
// change. AllHealthy lets routing skip per-peer Usable lookups when no
// tracked peer is filtered.
//
// # Time
//
// The hot path never reads the clock. Tick(now) stores a coarse clock that
// Observe uses for busy deadlines. Windows (outlier detection intervals,
// hedge budgets) are delimited by Tick: per-peer counters live in two
// buckets selected by an atomic index that Tick swaps every Interval.
// Breaker ejections triggered inline by Observe are timed from the next
// Tick (at most one runtime tick later), which also draws the jitter, so the
// random source is only used on the control path.
//
// # States
//
// The breaker state of a peer is Healthy, Ejected, or Probing. Busy and
// Degraded are overlays on a Healthy peer that make Usable return Degraded.
// When an ejection ends the peer becomes Probing: Usable still reports
// Unusable, so routing sends it no requests. Probing peers are probed by the
// control plane (neighbor pings and broadcast replies), whose results reach
// Observe like any other: any Success makes the peer Healthy; a Failure or
// Timeout ejects it again with a longer ejection.
//
// # Limits
//
// At most MaxEjectionPercent of tracked peers (but at least one) are in
// Ejected or Probing at once, and the last Healthy-state member of a cell is
// never ejected. These limits are enforced with atomic counters that are
// adjusted optimistically and rolled back on failure; under concurrent
// ejections they err on the side of not ejecting. Tick recomputes the
// counters from peer states each call, which repairs any drift caused by
// races with Track/Untrack.
//
// If more than PanicThreshold percent of tracked peers are unusable, Panic
// is set and Usable reports every peer Healthy until it clears.
//
// # Determinism
//
// Outlier detection and EWMA decay use floating point. Their results are
// local to this member (never compared across members) and are computed in a
// fixed order (by slot), so given the same inputs and seed the outputs are
// identical. Concurrent Observe calls are inherently unordered; simulations
// that need determinism call Observe from one goroutine.

package route

import (
	"math"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"
	"unsafe"
)

// Result is the outcome of a request to a peer.
type Result uint8

// Results.
const (
	Success Result = iota
	Failure
	Timeout
	// Overload means the peer shed load. It never trips the breaker.
	Overload
)

// Usability ranks a peer for routing. Higher is better.
type Usability uint8

// Usabilities.
const (
	Unusable Usability = iota
	Degraded
	Healthy
)

// PeerHealth is a peer state for reporting and coverage.
type PeerHealth uint8

// States.
const (
	StateHealthy PeerHealth = iota
	StateDegraded
	StateBusy
	StateEjected
	StateProbing
)

func (s PeerHealth) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDegraded:
		return "degraded"
	case StateBusy:
		return "busy"
	case StateEjected:
		return "ejected"
	case StateProbing:
		return "probing"
	}

	return "unknown"
}

// Reason says why a peer was ejected.
type Reason uint8

// Reasons.
const (
	ReasonConsecutiveFailures Reason = iota
	ReasonSuccessRate
	ReasonFailurePercentage
	ReasonProbeFailed
)

func (r Reason) String() string {
	switch r {
	case ReasonConsecutiveFailures:
		return "consecutive-failures"
	case ReasonSuccessRate:
		return "success-rate"
	case ReasonFailurePercentage:
		return "failure-percentage"
	case ReasonProbeFailed:
		return "probe-failed"
	}

	return "unknown"
}

// HealthConfig tunes a HealthTracker. Zero values select the documented defaults;
// a feature cannot be disabled by setting zero.
type HealthConfig struct {
	// ConsecutiveFailures ejects a peer after this many Failure or Timeout
	// results in a row. Default 5.
	ConsecutiveFailures uint32
	// BaseEjection is multiplied by the ejection count. Default 30s.
	BaseEjection time.Duration
	// MaxEjection caps the ejection time before jitter. Default 300s.
	MaxEjection time.Duration
	// EjectionJitter is the maximum added jitter as a percent of the
	// ejection time. Default 10.
	EjectionJitter uint32
	// Interval is the outlier detection, ejection-decay, and hedge budget
	// window. Default 10s.
	Interval time.Duration
	// SuccessRateStdevFactor ejects peers whose success rate is below
	// mean - factor*stddev. Default 1.9.
	SuccessRateStdevFactor float64
	// SuccessRateMinHosts is the number of peers with enough volume
	// required for success-rate detection. Default 5.
	SuccessRateMinHosts int
	// SuccessRateRequestVolume is the per-interval volume a peer needs to be
	// included in success-rate detection. Default 100.
	SuccessRateRequestVolume uint64
	// FailurePercentageThreshold ejects a peer whose failure percentage is
	// at least this. Default 85.
	FailurePercentageThreshold uint64
	// FailurePercentageRequestVolume is the per-interval volume a peer needs
	// for failure-percentage detection. Default 50.
	FailurePercentageRequestVolume uint64
	// EWMADecay is the peak-EWMA time constant. Default 10s.
	EWMADecay time.Duration
	// DegradedFactor marks a peer Degraded when its EWMA exceeds this many
	// times the median EWMA of tracked peers. Default 3.
	DegradedFactor int64
	// DegradedFloor is the minimum EWMA for a peer to be Degraded.
	// Default 10ms.
	DegradedFloor time.Duration
	// BusyFor is how long an Overload result marks a peer busy. Default 1s.
	BusyFor time.Duration
	// MaxEjectionPercent caps ejected peers as a percent of tracked peers.
	// At least one peer may always be ejected. Default 10.
	MaxEjectionPercent int
	// PanicThreshold disables health filtering when more than this percent
	// of tracked peers are unusable. Default 50.
	PanicThreshold int
	// HedgeRatio is the hedge budget as a fraction of routed requests per
	// interval. Default 0.05.
	HedgeRatio float64
	// HedgeMin and HedgeMax clamp HedgeAfter. Defaults 5ms and 1s.
	HedgeMin, HedgeMax time.Duration
	// HedgeFactor multiplies the EWMA for HedgeAfter. Default 3.
	HedgeFactor int64
	// Hooks receive state change notifications.
	Hooks HealthHooks
}

// HealthHooks are optional callbacks, invoked only from the control path (Tick,
// Track, Untrack). Nil fields are skipped.
type HealthHooks struct {
	// OnEject is called when a peer's ejection is timed (at the first Tick
	// after it was ejected).
	OnEject func(slot uint32, reason Reason, ejections int, d time.Duration)
	// OnProbe is called when an ejection ends and the peer starts probing.
	OnProbe func(slot uint32)
	// OnUneject is called when a probing peer has become Healthy.
	OnUneject func(slot uint32)
	// OnDegraded is called when a peer's Degraded flag changes.
	OnDegraded func(slot uint32, degraded bool)
	// OnPanic is called when the panic state changes.
	OnPanic func(panic bool)
}

func (c *HealthConfig) setDefaults() {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.BaseEjection, 30*time.Second)
	def(&c.MaxEjection, 300*time.Second)
	def(&c.Interval, 10*time.Second)
	def(&c.EWMADecay, 10*time.Second)
	def(&c.DegradedFloor, 10*time.Millisecond)
	def(&c.BusyFor, time.Second)
	def(&c.HedgeMin, 5*time.Millisecond)
	def(&c.HedgeMax, time.Second)

	if c.ConsecutiveFailures == 0 {
		c.ConsecutiveFailures = 5
	}

	if c.EjectionJitter == 0 {
		c.EjectionJitter = 10
	}

	if c.SuccessRateStdevFactor <= 0 {
		c.SuccessRateStdevFactor = 1.9
	}

	if c.SuccessRateMinHosts <= 0 {
		c.SuccessRateMinHosts = 5
	}

	if c.SuccessRateRequestVolume == 0 {
		c.SuccessRateRequestVolume = 100
	}

	if c.FailurePercentageThreshold == 0 {
		c.FailurePercentageThreshold = 85
	}

	if c.FailurePercentageRequestVolume == 0 {
		c.FailurePercentageRequestVolume = 50
	}

	if c.DegradedFactor <= 0 {
		c.DegradedFactor = 3
	}

	if c.MaxEjectionPercent <= 0 {
		c.MaxEjectionPercent = 10
	}

	if c.PanicThreshold <= 0 {
		c.PanicThreshold = 50
	}

	if c.HedgeRatio <= 0 {
		c.HedgeRatio = 0.05
	}

	if c.HedgeFactor <= 0 {
		c.HedgeFactor = 3
	}

	if c.HedgeMax < c.HedgeMin {
		c.HedgeMax = c.HedgeMin
	}
}

// Breaker states stored in Peer.state.
const (
	bHealthy uint32 = iota
	bEjected
	bProbing
	// bRetired marks a peer dropped from the table (untracked or replaced
	// by a new key). It never changes again, so a caller still holding an
	// old table cannot eject it, and it is not counted in HealthTracker.bad.
	bRetired
)

type bucket struct {
	succ, fail atomic.Uint64
}

type cellCounter struct {
	cell   uint32
	usable atomic.Int32 // peers in bHealthy
	n      int          // control-only: tracked peers in this cell
	tmp    int32        // control-only scratch
}

// healthPeer is the health state of one tracked neighbor.
//
// The fields are grouped by access pattern on three 64-byte lines (a healthPeer
// is 192 bytes, a size class whose objects are 64-byte aligned): fields
// read by every routing decision and rarely written, the counters every
// Observe writes, and fields used only on ejection and by the control path.
// Keeping the counters off the first line means a peer hammered by
// concurrent Observe calls does not slow down Usable for it.
type healthPeer struct {
	// Read-mostly.
	slot      uint32
	state     atomic.Uint32
	key       uint64
	cc        atomic.Pointer[cellCounter]
	busyUntil atomic.Int64
	ewma      atomic.Int64 // ns
	consec    atomic.Uint32
	degraded  atomic.Bool
	_         [64 - 48]byte

	// Written by every Observe.
	latSum  atomic.Int64
	latN    atomic.Int64
	buckets [2]bucket
	_       [64 - 48]byte

	// Ejection and control.
	until     atomic.Int64 // ejection end; 0 means not yet timed
	ejections atomic.Int32
	reason    atomic.Uint32
	recent    atomic.Bool // ejected during the current interval

	// Control-only.
	reported uint32 // breaker state last reported through hooks
	live     bool   // scratch for rebuild: in the new table
	_        [64 - 32]byte
}

type entry struct {
	slot uint32
	p    *healthPeer
}

// table is an immutable slot index. peers is sorted by slot.
type table struct {
	entries []entry // power-of-two open addressing; p == nil is empty
	shift   uint32
	peers   []*healthPeer
	cells   []*cellCounter // sorted by cell
}

func (t *table) lookup(slot uint32) *healthPeer {
	mask := uint32(len(t.entries) - 1)

	for i := (slot * 0x9E3779B1) >> t.shift; ; i = (i + 1) & mask {
		e := &t.entries[i]
		if e.p == nil || e.slot == slot {
			return e.p
		}
	}
}

const hedgeShards = 16

// Latency mean sampling of successes; see Observe.
const (
	latSampleAll   = 64
	latSampleEvery = 8
)

// maxLatency caps an observed latency so that latSum cannot overflow and
// the EWMA math stays well inside int64 and float64 precision.
const maxLatency = int64(time.Hour)

type hedgeShard struct {
	routed atomic.Uint64
	_      [128 - 8]byte // avoid false sharing (adjacent-line prefetch)
}

// HealthTracker tracks the health of a set of peers.
type HealthTracker struct {
	cfg HealthConfig
	rng *rand.Rand

	tab        atomic.Pointer[table]
	now        atomic.Int64 // coarse clock set by Tick
	active     atomic.Uint32
	panic      atomic.Bool
	ejected    atomic.Int32 // peers in bEjected or bProbing
	maxEjected atomic.Int32
	hedgePM    uint64 // hedge budget in parts per million

	// AllHealthy summary, exact under single-goroutine use and
	// conservative otherwise (see AllHealthy).
	bad       atomic.Int32 // peers in bEjected or bProbing
	nDegraded atomic.Int32 // peers with degraded set at the last Tick
	busyMax   atomic.Int64 // latest busy deadline ever set

	// The padding keeps the hedge counters, written on every routed
	// request, off the cache lines of the read-mostly fields above.
	_      [128]byte
	hedge  [hedgeShards]hedgeShard
	hedged atomic.Uint64
	_      [128 - 8]byte

	// Control-only.
	cells        map[uint32]*cellCounter
	started      bool
	lastTick     int64
	lastInterval int64
	scratch      []int64
	median       int64 // median EWMA at the last degrade pass
	vols         []vol
}

type vol struct {
	p          *healthPeer
	succ, fail uint64
}

// NewHealthTracker returns a HealthTracker. rng is used only on the control path, for
// ejection jitter; nil means a fixed-seed PCG source.
func NewHealthTracker(cfg HealthConfig, rng *rand.Rand) *HealthTracker {
	cfg.setDefaults()

	if rng == nil {
		rng = rand.New(rand.NewPCG(0, 0))
	}

	t := &HealthTracker{
		cfg:     cfg,
		rng:     rng,
		hedgePM: uint64(math.Round(cfg.HedgeRatio * 1e6)),
		cells:   map[uint32]*cellCounter{},
	}
	t.tab.Store(&table{entries: make([]entry, 1), shift: 32})
	t.maxEjected.Store(1)

	return t
}

// Track starts tracking slot with member key and cell, or updates the cell
// of an already tracked slot with the same key. A different key resets the
// slot's state. Control path.
func (t *HealthTracker) Track(slot uint32, key uint64, cell uint32) {
	old := t.tab.Load()
	if p := old.lookup(slot); p != nil && p.key == key {
		if p.cc.Load().cell != cell {
			p.cc.Store(t.cellFor(cell))
			t.rebuild(old.peers)
		}

		return
	}

	p := &healthPeer{slot: slot, key: key}
	p.cc.Store(t.cellFor(cell))

	peers := make([]*healthPeer, 0, len(old.peers)+1)
	for _, q := range old.peers {
		if q.slot != slot {
			peers = append(peers, q)
		}
	}

	i, _ := slices.BinarySearchFunc(peers, slot, func(q *healthPeer, s uint32) int {
		return cmpU32(q.slot, s)
	})
	peers = slices.Insert(peers, i, p)
	t.rebuild(peers)
}

// Untrack stops tracking slot. Control path.
func (t *HealthTracker) Untrack(slot uint32) {
	old := t.tab.Load()
	if old.lookup(slot) == nil {
		return
	}

	peers := make([]*healthPeer, 0, len(old.peers)-1)
	for _, q := range old.peers {
		if q.slot != slot {
			peers = append(peers, q)
		}
	}

	t.rebuild(peers)
}

// TrackRef names a peer for TrackMany and Sync.
type TrackRef struct {
	Slot uint32
	Key  uint64
	Cell uint32
}

// TrackMany is Track for every ref with a single table rebuild, so tracking
// n peers costs O(n log n + tracked) instead of O(n * tracked). Refs are
// applied in order; a later ref for the same slot wins. Control path.
func (t *HealthTracker) TrackMany(refs []TrackRef) {
	t.merge(refs, false)
}

// Sync makes the tracked set exactly refs: slots not in refs are
// untracked, and refs are applied as by TrackMany. Peers whose slot and key
// are unchanged keep their state. It rebuilds the table at most once.
// Control path.
func (t *HealthTracker) Sync(refs []TrackRef) {
	t.merge(refs, true)
}

func (t *HealthTracker) merge(refs []TrackRef, exact bool) {
	old := t.tab.Load()

	// Sort a copy by slot, last ref per slot wins (stable sort keeps
	// input order among equal slots).
	rs := slices.Clone(refs)
	slices.SortStableFunc(rs, func(a, b TrackRef) int { return cmpU32(a.Slot, b.Slot) })

	w := 0

	for i := range rs {
		if i+1 < len(rs) && rs[i+1].Slot == rs[i].Slot {
			continue
		}

		rs[w] = rs[i]
		w++
	}

	rs = rs[:w]

	peers := make([]*healthPeer, 0, max(len(rs), len(old.peers)))
	changed := false
	i, j := 0, 0

	for i < len(old.peers) || j < len(rs) {
		switch {
		case j == len(rs) || (i < len(old.peers) && old.peers[i].slot < rs[j].Slot):
			if exact {
				changed = true
			} else {
				peers = append(peers, old.peers[i])
			}

			i++
		case i == len(old.peers) || rs[j].Slot < old.peers[i].slot:
			peers = append(peers, t.newPeer(rs[j]))
			changed = true
			j++
		default:
			q, r := old.peers[i], rs[j]
			if q.key != r.Key {
				q = t.newPeer(r)
				changed = true
			} else if q.cc.Load().cell != r.Cell {
				q.cc.Store(t.cellFor(r.Cell))

				changed = true
			}

			peers = append(peers, q)
			i++
			j++
		}
	}

	if changed {
		t.rebuild(peers)
	}
}

func (t *HealthTracker) newPeer(r TrackRef) *healthPeer {
	p := &healthPeer{slot: r.Slot, key: r.Key}
	p.cc.Store(t.cellFor(r.Cell))

	return p
}

// Tracked returns the number of tracked peers.
func (t *HealthTracker) Tracked() int { return len(t.tab.Load().peers) }

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

func (t *HealthTracker) cellFor(cell uint32) *cellCounter {
	c := t.cells[cell]
	if c == nil {
		c = &cellCounter{cell: cell}
		t.cells[cell] = c
	}

	return c
}

func (t *HealthTracker) rebuild(peers []*healthPeer) {
	size := 8
	for size < 2*len(peers) {
		size *= 2
	}

	shift := uint32(32)
	for s := size; s > 1; s >>= 1 {
		shift--
	}

	nt := &table{entries: make([]entry, size), shift: shift, peers: peers}
	mask := uint32(size - 1)

	t.retireDropped(peers)

	for _, c := range t.cells {
		c.n = 0
	}

	for _, p := range peers {
		i := (p.slot * 0x9E3779B1) >> shift
		for nt.entries[i].p != nil {
			i = (i + 1) & mask
		}

		nt.entries[i] = entry{slot: p.slot, p: p}
		p.cc.Load().n++
	}

	for k, c := range t.cells {
		if c.n == 0 {
			delete(t.cells, k)
			continue
		}

		nt.cells = append(nt.cells, c)
	}

	slices.SortFunc(nt.cells, func(a, b *cellCounter) int { return cmpU32(a.cell, b.cell) })

	t.maxEjected.Store(int32(max(1, len(peers)*t.cfg.MaxEjectionPercent/100)))
	t.tab.Store(nt)
	t.reconcile(nt)
	t.updatePanic(nt)
}

// retireDropped moves every peer of the current table that is not in peers
// to bRetired, keeping HealthTracker.bad equal to the number of tracked-or-held
// peers in bEjected or bProbing.
func (t *HealthTracker) retireDropped(peers []*healthPeer) {
	old := t.tab.Load()

	for _, p := range old.peers {
		p.live = false
	}

	for _, p := range peers {
		p.live = true
	}

	for _, p := range old.peers {
		if p.live {
			continue
		}

		for {
			st := p.state.Load()
			if st == bRetired || p.state.CompareAndSwap(st, bRetired) {
				if st == bEjected || st == bProbing {
					t.bad.Add(-1)
				}

				break
			}
		}
	}
}

// reconcile recomputes the ejection and per-cell usable counters from peer
// states.
func (t *HealthTracker) reconcile(nt *table) {
	for _, c := range nt.cells {
		c.tmp = 0
	}

	var ej int32

	for _, p := range nt.peers {
		if p.state.Load() == bHealthy {
			p.cc.Load().tmp++
		} else {
			ej++
		}
	}

	for _, c := range nt.cells {
		c.usable.Store(c.tmp)
	}

	t.ejected.Store(ej)
}

func (t *HealthTracker) updatePanic(nt *table) {
	n := len(nt.peers)
	p := n > 0 && int(t.ejected.Load())*100 > n*t.cfg.PanicThreshold

	if t.panic.Swap(p) != p && t.cfg.Hooks.OnPanic != nil {
		t.cfg.Hooks.OnPanic(p)
	}
}

// tryEject moves p from state from to bEjected, respecting limits when
// from is bHealthy. Safe for concurrent use.
func (t *HealthTracker) tryEject(p *healthPeer, from uint32, reason Reason) bool {
	// p.until is already 0: it is cleared when an ejection ends (see
	// transitions), so a concurrent loser of the CAS below never clobbers
	// the timing of an ejection another caller just made.
	if from == bProbing {
		if !p.state.CompareAndSwap(bProbing, bEjected) {
			return false
		}

		p.reason.Store(uint32(reason))
		p.ejections.Add(1)
		p.recent.Store(true)

		return true
	}

	if t.ejected.Add(1) > t.maxEjected.Load() {
		t.ejected.Add(-1)
		return false
	}

	cc := p.cc.Load()
	if cc.usable.Add(-1) < 1 {
		cc.usable.Add(1)
		t.ejected.Add(-1)

		return false
	}

	if !p.state.CompareAndSwap(bHealthy, bEjected) {
		cc.usable.Add(1)
		t.ejected.Add(-1)

		return false
	}

	t.bad.Add(1)

	p.reason.Store(uint32(reason))
	p.ejections.Add(1)
	p.recent.Store(true)
	p.consec.Store(0)

	return true
}

// Observe records the result and latency of a request to slot. Hot path.
func (t *HealthTracker) Observe(slot uint32, r Result, latency time.Duration) {
	p := t.tab.Load().lookup(slot)
	if p == nil {
		return
	}

	if r == Overload {
		until := t.now.Load() + int64(t.cfg.BusyFor)
		p.busyUntil.Store(until)

		for {
			m := t.busyMax.Load()
			if m >= until || t.busyMax.CompareAndSwap(m, until) {
				break
			}
		}

		return
	}

	lat := min(int64(latency), maxLatency)
	if lat > 0 {
		// Peak: any observation above the EWMA raises it immediately.
		for {
			e := p.ewma.Load()
			if lat <= e || p.ewma.CompareAndSwap(e, lat) {
				break
			}
		}
	}

	b := &p.buckets[t.active.Load()&1]
	st := p.state.Load()

	if r == Success {
		// The latency mean that Tick decays the EWMA toward is sampled:
		// every success while the interval's success count is at most
		// latSampleAll, then one in latSampleEvery. The count is the
		// bucket counter this call increments anyway, so sampling is
		// deterministic and costs no extra shared write.
		if n := b.succ.Add(1); lat > 0 && (n <= latSampleAll || n%latSampleEvery == 0) {
			p.latSum.Add(lat)
			p.latN.Add(1)
		}

		if p.consec.Load() != 0 {
			p.consec.Store(0)
		}

		if st == bProbing && p.state.CompareAndSwap(bProbing, bHealthy) {
			p.cc.Load().usable.Add(1)
			t.ejected.Add(-1)
			t.bad.Add(-1)
		}

		return
	}

	b.fail.Add(1)

	if lat > 0 {
		p.latSum.Add(lat)
		p.latN.Add(1)
	}

	switch st {
	case bHealthy:
		if p.consec.Add(1) >= t.cfg.ConsecutiveFailures {
			t.tryEject(p, bHealthy, ReasonConsecutiveFailures)
		}
	case bProbing:
		t.tryEject(p, bProbing, ReasonProbeFailed)
	}
}

// Usable reports how usable slot is for routing. Untracked slots are
// Healthy. Hot path.
func (t *HealthTracker) Usable(slot uint32) Usability {
	p := t.tab.Load().lookup(slot)
	if p == nil || t.panic.Load() {
		return Healthy
	}

	if p.state.Load() != bHealthy {
		return Unusable
	}

	if p.degraded.Load() || p.busyUntil.Load() > t.now.Load() {
		return Degraded
	}

	return Healthy
}

// AllHealthy reports whether Usable returns Healthy for every slot, so a
// caller ranking many peers can skip per-peer lookups. It is true when
// filtering is in panic, or when no peer is Ejected or Probing, none was
// Degraded at the last Tick, and no busy deadline is still pending. A peer
// that changes state concurrently may be reported either way, as with
// Usable; after a peer is dropped from the tracked set it may keep
// AllHealthy false until the next Tick. Hot path.
func (t *HealthTracker) AllHealthy() bool {
	if t.bad.Load() == 0 && t.nDegraded.Load() == 0 && t.busyMax.Load() <= t.now.Load() {
		return true
	}

	return t.panic.Load()
}

// Panic reports whether health filtering is disabled.
func (t *HealthTracker) Panic() bool { return t.panic.Load() }

// HedgeAfter returns clamp(HedgeFactor*EWMA, HedgeMin, HedgeMax). A peer
// without latency data, or an untracked slot, returns HedgeMax so that
// hedging is conservative until there is data. Hot path.
func (t *HealthTracker) HedgeAfter(slot uint32) time.Duration {
	p := t.tab.Load().lookup(slot)
	if p == nil {
		return t.cfg.HedgeMax
	}

	e := p.ewma.Load()
	if e <= 0 {
		return t.cfg.HedgeMax
	}

	d := t.cfg.HedgeMax
	if e < int64(t.cfg.HedgeMax)/t.cfg.HedgeFactor {
		d = time.Duration(e * t.cfg.HedgeFactor)
	}

	return max(d, t.cfg.HedgeMin)
}

// NoteRouted counts a routed request toward the hedge budget. Hot path.
//
// The count is spread over shards chosen by the calling goroutine's stack
// address, so concurrent routers mostly write different cache lines even
// when they route the same partition; TryHedge sums the shards, so which
// shard a call lands in never affects a result.
func (t *HealthTracker) NoteRouted() {
	t.hedge[stackShard()].routed.Add(1)
}

// stackShard returns a shard index derived from the address of a local
// variable: stable for a goroutine (until its stack moves) and different
// across goroutines, which run on different stacks of at least 8 KiB.
func stackShard() uint64 {
	var b byte

	// Stacks are usually carved consecutively, so the low bits of the
	// 8 KiB block number spread goroutines started together.
	return uint64(uintptr(unsafe.Pointer(&b))>>13) & (hedgeShards - 1)
}

// TryHedge admits a hedge if hedges in this interval stay within HedgeRatio
// of the requests routed in this interval. The check and increment are not
// atomic together, so concurrent callers may overshoot slightly. Hot path, but called only for requests that are hedge candidates,
// so it reads every shard.
func (t *HealthTracker) TryHedge() bool {
	// Sum the shards only until the budget covers one more hedge.
	need := (t.hedged.Load() + 1) * 1e6

	var routed uint64

	for i := range t.hedge {
		routed += t.hedge[i].routed.Load()
		if routed*t.hedgePM >= need {
			t.hedged.Add(1)

			return true
		}
	}

	return false
}

// Tick advances the control clock to now (node-local monotonic ns). Call it
// regularly (for example every 100ms) from the control goroutine.
func (t *HealthTracker) Tick(now int64) {
	t.now.Store(now)

	if !t.started {
		t.started = true
		t.lastTick = now
		t.lastInterval = now
	}

	nt := t.tab.Load()

	t.decayEWMA(nt, now-t.lastTick)
	t.lastTick = now

	if now-t.lastInterval >= int64(t.cfg.Interval) {
		t.lastInterval = now
		t.interval(nt)
	}

	t.transitions(nt, now)
	t.reconcile(nt)
	t.degrade(nt)
	t.updatePanic(nt)
}

func (t *HealthTracker) decayEWMA(nt *table, dt int64) {
	if dt <= 0 {
		return
	}

	w := math.Exp(-float64(dt) / float64(t.cfg.EWMADecay))

	for _, p := range nt.peers {
		n := p.latN.Swap(0)
		sum := p.latSum.Swap(0)

		// A peer with no observations decays toward the median of the
		// last degrade pass, never upward. Otherwise a Degraded peer,
		// which routing avoids, would get no traffic and keep its peak
		// EWMA (and stay Degraded) forever. Once it falls below the
		// limit traffic returns, and a still-slow peer is marked
		// Degraded again after its next peak observation. Decaying
		// toward the median rather than zero keeps idle peers from
		// dragging the median down.
		mean := float64(t.median)
		if n > 0 {
			mean = float64(sum) / float64(n)
		}

		for {
			e := p.ewma.Load()
			if n == 0 && e <= t.median {
				break
			}

			ne := int64(mean + (float64(e)-mean)*w)
			if p.ewma.CompareAndSwap(e, ne) {
				break
			}
		}
	}
}

func (t *HealthTracker) interval(nt *table) {
	old := t.active.Load() & 1
	t.active.Store(old ^ 1)

	cfg := &t.cfg

	// Gather counts in slot order.
	var (
		sum, sumSq float64
		hosts      int
	)

	vols := t.vols[:0]
	defer func() { clear(vols); t.vols = vols[:0] }()

	for _, p := range nt.peers {
		b := &p.buckets[old]
		v := vol{p: p, succ: b.succ.Swap(0), fail: b.fail.Swap(0)}
		vols = append(vols, v)

		if total := v.succ + v.fail; total >= cfg.SuccessRateRequestVolume {
			r := float64(v.succ) / float64(total)
			sum += r
			sumSq += r * r
			hosts++
		}
	}

	if hosts >= cfg.SuccessRateMinHosts {
		mean := sum / float64(hosts)
		stdev := math.Sqrt(max(0, sumSq/float64(hosts)-mean*mean))
		threshold := mean - cfg.SuccessRateStdevFactor*stdev

		for _, v := range vols {
			if total := v.succ + v.fail; total >= cfg.SuccessRateRequestVolume &&
				float64(v.succ)/float64(total) < threshold {
				t.tryEject(v.p, bHealthy, ReasonSuccessRate)
			}
		}
	}

	for _, v := range vols {
		if total := v.succ + v.fail; total >= cfg.FailurePercentageRequestVolume &&
			v.fail*100 >= cfg.FailurePercentageThreshold*total {
			t.tryEject(v.p, bHealthy, ReasonFailurePercentage)
		}
	}

	for _, p := range nt.peers {
		recent := p.recent.Swap(false)
		if !recent && p.state.Load() == bHealthy && p.ejections.Load() > 0 {
			p.ejections.Add(-1)
		}
	}

	for i := range t.hedge {
		t.hedge[i].routed.Store(0)
	}

	t.hedged.Store(0)
}

// ejectionFor returns the ejection time for count n, with jitter.
func (t *HealthTracker) ejectionFor(n int32) time.Duration {
	d := t.cfg.MaxEjection
	if n > 0 && int64(t.cfg.BaseEjection) <= int64(t.cfg.MaxEjection)/int64(n) {
		d = t.cfg.BaseEjection * time.Duration(n)
	}

	j := int64(d) * int64(t.cfg.EjectionJitter) / 100

	return d + time.Duration(t.rng.Int64N(j+1))
}

func (t *HealthTracker) transitions(nt *table, now int64) {
	h := &t.cfg.Hooks

	for _, p := range nt.peers {
		switch p.state.Load() {
		case bEjected:
			u := p.until.Load()
			if u == 0 {
				n := p.ejections.Load()
				d := t.ejectionFor(n)
				p.until.Store(now + int64(d))

				if h.OnEject != nil {
					h.OnEject(p.slot, Reason(p.reason.Load()), int(n), d)
				}

				p.reported = bEjected

				continue
			}

			if now >= u {
				if p.state.CompareAndSwap(bEjected, bProbing) {
					// Only Tick times ejections, and it ignores until
					// outside bEjected, so clearing it here means every
					// future ejection starts untimed.
					p.until.Store(0)
					p.reported = bProbing

					if h.OnProbe != nil {
						h.OnProbe(p.slot)
					}
				}
			}
		case bHealthy:
			if p.reported != bHealthy {
				p.reported = bHealthy

				if h.OnUneject != nil {
					h.OnUneject(p.slot)
				}
			}
		}
	}
}

func (t *HealthTracker) degrade(nt *table) {
	t.scratch = t.scratch[:0]

	for _, p := range nt.peers {
		if e := p.ewma.Load(); e > 0 {
			t.scratch = append(t.scratch, e)
		}
	}

	var limit int64 = math.MaxInt64

	t.median = 0

	if len(t.scratch) > 0 {
		slices.Sort(t.scratch)

		median := t.scratch[len(t.scratch)/2]

		t.median = median
		if median <= math.MaxInt64/t.cfg.DegradedFactor {
			limit = max(median*t.cfg.DegradedFactor, int64(t.cfg.DegradedFloor))
		}
	}

	var nd int32

	for _, p := range nt.peers {
		d := p.ewma.Load() > limit
		if d {
			nd++
		}

		if p.degraded.Swap(d) != d && t.cfg.Hooks.OnDegraded != nil {
			t.cfg.Hooks.OnDegraded(p.slot, d)
		}
	}

	t.nDegraded.Store(nd)
}

// PeerState reports the state, ejection count, and EWMA latency of slot.
// Untracked slots report StateHealthy with zero values.
func (t *HealthTracker) PeerState(slot uint32) (PeerHealth, int, time.Duration) {
	p := t.tab.Load().lookup(slot)
	if p == nil {
		return StateHealthy, 0, 0
	}

	return t.peerState(p), int(p.ejections.Load()), time.Duration(p.ewma.Load())
}

func (t *HealthTracker) peerState(p *healthPeer) PeerHealth {
	switch {
	case p.state.Load() == bEjected:
		return StateEjected
	case p.state.Load() == bProbing:
		return StateProbing
	case p.busyUntil.Load() > t.now.Load():
		return StateBusy
	case p.degraded.Load():
		return StateDegraded
	}

	return StateHealthy
}

// Counts summarizes tracked peers by state.
type Counts struct {
	Tracked, Healthy, Degraded, Busy, Ejected, Probing int
}

// Counts returns a summary of tracked peers by state.
func (t *HealthTracker) Counts() Counts {
	nt := t.tab.Load()
	c := Counts{Tracked: len(nt.peers)}

	for _, p := range nt.peers {
		switch t.peerState(p) {
		case StateHealthy:
			c.Healthy++
		case StateDegraded:
			c.Degraded++
		case StateBusy:
			c.Busy++
		case StateEjected:
			c.Ejected++
		case StateProbing:
			c.Probing++
		}
	}

	return c
}
