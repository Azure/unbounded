// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package leader is the cachemesh leader logic: a pure, deterministic delta
// planner run by the elected leader.
//
// # Design
//
// The leader batches its inputs (joins, leaves, suspicions, invalidations)
// in bounded in-memory queues and turns them into at most one list of
// view.Op per Tick. Everything else is derived from the view it is given:
// member states, weights, cells, and the partition table. A newly elected
// leader therefore continues where the previous one stopped; inputs that
// were queued at the old leader are lost and retried by their senders.
//
// Member timing (quarantine, warmup steps, drain) is measured on the
// leader's monotonic clock from the moment it set the member's current
// state. For a member whose state it did not set itself (after a failover
// or a restart) it estimates that moment from Member.Since, the epoch of the
// last state change: at = now - (epochNow - Since) * EpochPeriod. The
// estimate never overstates elapsed time by more than one epoch period and
// is usually conservative.
//
// The ops of one delta are ordered:
//
//  1. NewTerm (only when Tick is asked to start a term).
//  2. Bootstrap: Config and AddMember(self) when the view has no config.
//  3. Admissions, at most MaxJoinsPerTick. A member with the same ID and
//     another incarnation is removed first, in the same delta. New members
//     are Joining with weight 0 in the least occupied cell.
//  4. Promotions: Joining members become Active after QuarantineFor;
//     Active members step through WarmupWeights every WarmupStep.
//  5. Drains (Leave requests) and removal of members Draining for DrainFor.
//  6. Suspicion removals: a member reported by observers (per
//     view.Observers) in at least ObserverCells distinct cells within
//     SuspicionWindow, clamped to the distinct cells of its observers that
//     are not themselves suspected (so failed members that observe each
//     other are still removed).
//     At most max(1, N/1000) per tick, or max(1, N/10000) when more than
//     DampingFraction of the members are suspected.
//  7. Cell balancing (SetCell, MaxCellMovesPerTick) or, when the shape
//     violates its bounds (view.NeedsReshape), one Reshape.
//  8. Partition moves (Assign) from control.Planner: mandatory moves for
//     removed and weight-0 owners, then optional moves up to MaxMoves.
//  9. Epoch, when the leader's epoch is ahead of the view.
//  10. Invalidations.
//
// Cells are settled before partitions so the planner prefers the final
// cell-mates of removed owners.
//
// The leader never removes itself. A removal always comes with the moves
// of the removed member's partitions in the same delta, so the view never
// has a partition owned by a non-member.
//
// # Contract
//
// A Leader is single-threaded and sans-I/O. It reads no clock, uses only the
// injected random source, starts no goroutines, and no output depends on
// map iteration order. The ops returned by Tick are valid for the given
// view: they apply with view.Apply (after the caller wraps them in a delta)
// and the result passes view.Validate. The returned slice is owned by the
// caller.
package leader

import (
	"math/rand/v2"
	"slices"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/control"
	"github.com/Azure/unbounded/internal/cachemesh/internal/route"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Defaults.
const (
	DefaultMaxJoinsPerTick     = 256
	DefaultQuarantineFor       = 10 * time.Second
	DefaultWarmupStep          = 20 * time.Second
	DefaultDrainFor            = 30 * time.Second
	DefaultSuspicionWindow     = 30 * time.Second
	DefaultObserverCells       = 3
	DefaultDampingPercent      = 10
	DefaultMaxMoves            = control.DefaultMaxMoves
	DefaultMaxCellMovesPerTick = 32
	DefaultCellSize            = 4
	DefaultMaxNeighbors        = 256
	DefaultPartitionBits       = view.DefaultPartitionBits
	DefaultEpochPeriod         = route.DefaultEpochPeriod
	DefaultSlotReuseEpochs     = 16
	DefaultHandoffEpochs       = 8
	DefaultMaxPending          = 4096
)

// DefaultWarmupWeights are the Active weight steps.
var DefaultWarmupWeights = []uint32{250, 500, view.MaxWeight}

// Member identifies a member to admit, including the leader itself.
type Member struct {
	ID          string
	Incarnation uint64
	Addr, Zone  string
}

// Config tunes a Leader. Zero values select defaults.
type Config struct {
	// Self is the leader's own identity. It is admitted when the leader is
	// not a member of the view it leads (bootstrap or a lottery winner).
	Self Member

	MaxJoinsPerTick int
	QuarantineFor   time.Duration
	WarmupStep      time.Duration
	// WarmupWeights are the weights an Active member steps through, one
	// per WarmupStep; the last is the steady-state weight.
	WarmupWeights   []uint32
	DrainFor        time.Duration
	SuspicionWindow time.Duration
	ObserverCells   int
	// DampingPercent is the share of suspected members above which the
	// removal cap drops to max(1, N/10000) per tick. Default 10.
	DampingPercent int
	// MaxMoves caps optional partition moves per tick. Negative disables
	// optional moves.
	MaxMoves            int
	MaxCellMovesPerTick int
	CellSize            int
	MaxNeighbors        int
	// PartitionBits, SlotReuseEpochs, and HandoffEpochs go into the
	// bootstrap Config op.
	PartitionBits   uint8
	SlotReuseEpochs uint32
	HandoffEpochs   uint32
	EpochPeriod     time.Duration
	// MaxPending bounds each input queue (joins, leaves, suspected
	// targets, invalidations). Default 4096.
	MaxPending int
}

// WithDefaults returns c with zero fields replaced by defaults.
func (c Config) WithDefaults() Config {
	def := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}

	def(&c.MaxJoinsPerTick, DefaultMaxJoinsPerTick)
	dur(&c.QuarantineFor, DefaultQuarantineFor)
	dur(&c.WarmupStep, DefaultWarmupStep)
	dur(&c.DrainFor, DefaultDrainFor)
	dur(&c.SuspicionWindow, DefaultSuspicionWindow)
	dur(&c.EpochPeriod, DefaultEpochPeriod)
	def(&c.ObserverCells, DefaultObserverCells)
	def(&c.DampingPercent, DefaultDampingPercent)
	def(&c.MaxCellMovesPerTick, DefaultMaxCellMovesPerTick)
	def(&c.CellSize, DefaultCellSize)
	def(&c.MaxNeighbors, DefaultMaxNeighbors)
	def(&c.MaxPending, DefaultMaxPending)

	if len(c.WarmupWeights) == 0 {
		c.WarmupWeights = DefaultWarmupWeights
	}

	if c.PartitionBits == 0 {
		c.PartitionBits = DefaultPartitionBits
	}

	if c.SlotReuseEpochs == 0 {
		c.SlotReuseEpochs = DefaultSlotReuseEpochs
	}

	if c.HandoffEpochs == 0 {
		c.HandoffEpochs = DefaultHandoffEpochs
	}

	return c
}

// RemoveReason explains a removal.
type RemoveReason uint8

// Removal reasons.
const (
	RemovedReplaced  RemoveReason = iota + 1 // same ID rejoined with a new incarnation
	RemovedDrained                           // left gracefully
	RemovedSuspected                         // failure detection
)

// Hooks observe the Leader for metrics and coverage. Nil fields are skipped.
type Hooks struct {
	OnAdmit     func(slot view.Slot, id string)
	OnRemove    func(slot view.Slot, reason RemoveReason)
	OnPromote   func(slot view.Slot, state view.MemberState, weight uint32)
	OnDamped    func(suspected, members int)
	OnReshape   func(from, to view.Shape)
	OnCellMove  func(n int)
	OnPlan      func(mandatory, optional int)
	OnDrop      func(queue string) // an input was dropped because its queue is full
	OnBootstrap func()
}

type report struct {
	observer view.Slot
	at       int64
}

type suspicion struct {
	key     uint64
	reports []report
}

type stamp struct {
	key   uint64
	state view.MemberState
	at    int64
}

// Leader plans deltas. Create one with New.
type Leader struct {
	cfg   Config
	rng   *rand.Rand
	hooks Hooks

	joins     []Member
	joinIdx   map[string]int
	leaves    map[string]uint64
	suspects  map[view.Slot]*suspicion
	invals    []uint64
	stamps    []stamp // indexed by slot
	planner   *control.Planner
	settledAt *view.Table
	settledH  uint64

	// scratch
	pm      []control.Member
	occBuf  []uint32
	obs     []view.Slot
	targets []view.Slot
}

// New returns a Leader. rng is used only for tie breaking that does not
// need to agree across members; it may be nil.
func New(cfg Config, rng *rand.Rand) *Leader {
	cfg = cfg.WithDefaults()

	return &Leader{
		cfg:      cfg,
		rng:      rng,
		joinIdx:  map[string]int{},
		leaves:   map[string]uint64{},
		suspects: map[view.Slot]*suspicion{},
		planner:  control.NewPlanner(control.Config{MaxMoves: cfg.MaxMoves}),
	}
}

// SetHooks installs hooks.
func (l *Leader) SetHooks(h Hooks) { l.hooks = h }

// Config returns the effective configuration.
func (l *Leader) Config() Config { return l.cfg }

// Reset drops all queued inputs and timing state. Call it when gaining
// leadership.
func (l *Leader) Reset() {
	l.joins = l.joins[:0]
	clear(l.joinIdx)
	clear(l.leaves)
	clear(l.suspects)
	l.invals = l.invals[:0]
	l.stamps = l.stamps[:0]
	l.settledAt = nil
}

// Pending returns the queued joins, leaves, suspected targets, and
// invalidations.
func (l *Leader) Pending() (joins, leaves, suspects, invals int) {
	return len(l.joins), len(l.leaves), len(l.suspects), len(l.invals)
}

func (l *Leader) drop(q string) {
	if l.hooks.OnDrop != nil {
		l.hooks.OnDrop(q)
	}
}

// Join queues an admission request. A later request with the same ID
// replaces an earlier one.
func (l *Leader) Join(_ int64, j Member) {
	if j.ID == "" || len(j.ID) > view.MaxIDLen || len(j.Addr) > view.MaxAddrLen || len(j.Zone) > view.MaxZoneLen {
		return
	}

	if i, ok := l.joinIdx[j.ID]; ok {
		l.joins[i] = j

		return
	}

	if len(l.joins) >= l.cfg.MaxPending {
		l.drop("join")

		return
	}

	l.joinIdx[j.ID] = len(l.joins)
	l.joins = append(l.joins, j)
}

// Leave queues a graceful departure of the member with this ID and
// incarnation.
func (l *Leader) Leave(_ int64, id string, inc uint64) {
	if _, ok := l.leaves[id]; !ok && len(l.leaves) >= l.cfg.MaxPending {
		l.drop("leave")

		return
	}

	l.leaves[id] = inc
}

// Suspect records that observer could not reach target, whose member key
// is targetKey, at local time now.
func (l *Leader) Suspect(now int64, target view.Slot, targetKey uint64, observer view.Slot) {
	s, ok := l.suspects[target]
	if !ok || s.key != targetKey {
		if !ok && len(l.suspects) >= l.cfg.MaxPending {
			l.drop("suspect")

			return
		}

		s = &suspicion{key: targetKey}
		l.suspects[target] = s
	}

	for i := range s.reports {
		if s.reports[i].observer == observer {
			s.reports[i].at = now

			return
		}
	}

	const maxReports = 64
	if len(s.reports) >= maxReports {
		// Replace the oldest report.
		oldest := 0
		for i := range s.reports {
			if s.reports[i].at < s.reports[oldest].at {
				oldest = i
			}
		}

		s.reports[oldest] = report{observer: observer, at: now}

		return
	}

	s.reports = append(s.reports, report{observer: observer, at: now})
}

// Invalidate queues a generation bump for the keyspace seed.
func (l *Leader) Invalidate(seed uint64) {
	if slices.Contains(l.invals, seed) {
		return
	}

	if len(l.invals) >= l.cfg.MaxPending {
		l.drop("invalidate")

		return
	}

	l.invals = append(l.invals, seed)
}

// state is the per-tick working copy of what the delta has done so far.
type state struct {
	v        *view.View
	ops      []view.Op
	removed  map[view.Slot]bool
	setState map[view.Slot]view.Op // final state and weight per slot
	added    []view.Slot
	addedMem []view.Member
	slotCap  uint32
	n        int
	self     view.Slot
	changed  bool                 // membership, weights, or cells changed
	occ      []uint32             // per-cell occupancy, nil until built
	moved    map[view.Slot]uint32 // SetCell targets
	reshaped []uint32             // Reshape cells by slot, nil without a reshape
}

// maxMemberOps bounds the per-member ops of one delta, leaving room under
// view.MaxDeltaOps for cell moves, Assign, Epoch, and invalidations. Work
// beyond it waits for the next tick.
const maxMemberOps = view.MaxDeltaOps - 4096

// maxInvalsPerTick bounds Invalidate ops per delta.
const maxInvalsPerTick = 1024

func (st *state) budget() bool { return len(st.ops) < maxMemberOps }

func (st *state) memberState(s view.Slot) (view.MemberState, uint32) {
	if op, ok := st.setState[s]; ok {
		return op.State, op.Weight
	}

	m, _ := st.v.Member(s)

	return m.State, m.Weight
}

// Tick plans one delta at local monotonic time now for view v. epochNow is
// the leader's current epoch. When newTerm is nonzero the delta starts that
// term: its first op is NewTerm(newTerm, own slot), and the leader admits
// itself if it is not a member. Tick returns nil when there is nothing to
// do (never when newTerm is nonzero).
func (l *Leader) Tick(now int64, epochNow uint64, v *view.View, newTerm uint64) []view.Op {
	st := &state{v: v, removed: map[view.Slot]bool{}, setState: map[view.Slot]view.Op{}, slotCap: v.SlotCap(), n: v.Len()}

	if newTerm != 0 {
		st.ops = append(st.ops, view.OpNewTerm(newTerm, view.NoSlot))
	}

	_, hasConfig := v.Config()
	if !hasConfig {
		if newTerm == 0 {
			// Only the first delta of a term may carry the bootstrap, so a
			// leader without a config has nothing to do outside it.
			return nil
		}

		l.bootstrap(st, epochNow)
		st.ops[0].Slot = st.self

		return st.ops
	}

	l.selfSlot(st, epochNow, newTerm != 0)

	if st.self == view.NoSlot {
		// Not a member and not starting a term: nothing can be published
		// (the leader must be a member).
		return nil
	}

	if newTerm != 0 {
		st.ops[0].Slot = st.self
	}

	l.ensureStamps(v)
	l.admit(st, now, epochNow)
	l.promote(st, now, epochNow)
	l.drains(st, now, epochNow)
	l.suspicions(st, now)
	l.cells(st)
	l.place(st)

	if epochNow > v.Epoch() {
		st.ops = append(st.ops, view.OpEpoch(epochNow, l.cfg.EpochPeriod))
	}

	k := min(len(l.invals), maxInvalsPerTick)
	for _, seed := range l.invals[:k] {
		st.ops = append(st.ops, view.OpInvalidate(seed, v.Gen(seed)+1))
	}

	l.invals = append(l.invals[:0], l.invals[k:]...)

	if len(st.ops) == 0 {
		return nil
	}

	return st.ops
}

func (l *Leader) selfMember(epochNow uint64, state view.MemberState, w, cell uint32) view.Member {
	_ = epochNow
	s := l.cfg.Self

	return view.Member{
		ID: s.ID, Incarnation: s.Incarnation, Key: view.MemberKey(s.ID, s.Incarnation),
		Addr: s.Addr, Zone: s.Zone, State: state, Weight: w, Cell: cell,
	}
}

func (l *Leader) bootstrap(st *state, epochNow uint64) {
	self := st.v.NextSlot()
	st.self = self

	st.ops = append(st.ops,
		view.OpConfig(view.Config{
			PartitionBits: l.cfg.PartitionBits, InitialOwner: self,
			SlotReuseEpochs: l.cfg.SlotReuseEpochs, HandoffEpochs: l.cfg.HandoffEpochs,
		}),
		view.OpAddMember(self, l.selfMember(epochNow, view.Active, view.MaxWeight, 0)),
	)

	if epochNow > st.v.Epoch() || st.v.Period() != l.cfg.EpochPeriod {
		st.ops = append(st.ops, view.OpEpoch(max(epochNow, st.v.Epoch()), l.cfg.EpochPeriod))
	}

	if l.hooks.OnBootstrap != nil {
		l.hooks.OnBootstrap()
	}
}

// selfSlot finds the leader's slot. When starting a term and not a member,
// the leader admits itself first (removing a stale incarnation).
func (l *Leader) selfSlot(st *state, epochNow uint64, starting bool) {
	v := st.v
	key := view.MemberKey(l.cfg.Self.ID, l.cfg.Self.Incarnation)

	if s, ok := v.Lookup(l.cfg.Self.ID); ok {
		if v.Key(s) == key {
			st.self = s

			return
		}

		if !starting {
			st.self = view.NoSlot

			return
		}

		l.remove(st, s, RemovedReplaced)
	} else if !starting {
		st.self = view.NoSlot

		return
	}

	_ = epochNow
	occ := l.occupancy(st)
	cell := view.JoinCell(occ, key)
	s := l.nextSlots(st, 1)[0]
	st.self = s
	st.ops = append(st.ops, view.OpAddMember(s, l.selfMember(epochNow, view.Joining, 0, cell)))
	st.added = append(st.added, s)
	st.addedMem = append(st.addedMem, l.selfMember(epochNow, view.Joining, 0, cell))
	st.n++
	st.changed = true
	occ[cell]++
}

func (l *Leader) remove(st *state, s view.Slot, reason RemoveReason) {
	st.ops = append(st.ops, view.OpRemoveMember(s))
	st.removed[s] = true
	st.n--
	st.changed = true

	if l.hooks.OnRemove != nil {
		l.hooks.OnRemove(s, reason)
	}

	if st.occ != nil && st.v.IsMember(s) {
		if c := st.v.Cell(s); int(c) < len(st.occ) && st.occ[c] > 0 {
			st.occ[c]--
		}
	}
}

// occupancy returns the per-cell occupancy for the working state, building
// it on first use in a tick.
func (l *Leader) occupancy(st *state) []uint32 {
	if st.occ != nil {
		return st.occ
	}

	nc := st.v.Shape().Cells()
	occ := slices.Grow(l.occBuf[:0], int(nc))[:nc]

	for c := range nc {
		occ[c] = uint32(st.v.Occupancy(c))
	}

	for s := range st.removed {
		if c := st.v.Cell(s); c < nc {
			occ[c]--
		}
	}

	l.occBuf = occ
	st.occ = occ

	return occ
}

// nextSlots returns k slots for new members: reusable free slots of the
// base view in ascending order, then fresh slots. It accounts for slots
// already taken in this tick.
func (l *Leader) nextSlots(st *state, k int) []view.Slot {
	out := make([]view.Slot, 0, k)
	v := st.v

	if first := v.NextSlot(); first < v.SlotCap() {
		for _, f := range v.FreeSlots() {
			if len(out) == k {
				break
			}

			if f.ReusableAt <= v.Epoch() && !slices.Contains(st.added, f.Slot) {
				out = append(out, f.Slot)
			}
		}
	}

	for len(out) < k {
		out = append(out, st.slotCap)
		st.slotCap++
	}

	return out
}

func (l *Leader) admit(st *state, _ int64, epochNow uint64) {
	if len(l.joins) == 0 {
		return
	}

	v := st.v
	k := min(len(l.joins), l.cfg.MaxJoinsPerTick, max(0, (maxMemberOps-len(st.ops))/2))

	batch := make([]Member, 0, k)
	for _, j := range l.joins[:k] {
		key := view.MemberKey(j.ID, j.Incarnation)
		if s, ok := v.Lookup(j.ID); ok {
			if v.Key(s) == key || s == st.self || st.removed[s] {
				continue // already admitted, or the leader's own ID
			}

			l.remove(st, s, RemovedReplaced)
		}

		if j.ID == l.cfg.Self.ID {
			continue
		}

		batch = append(batch, j)
	}

	for _, j := range l.joins[:k] {
		delete(l.joinIdx, j.ID)
	}

	l.joins = append(l.joins[:0], l.joins[k:]...)
	for i, j := range l.joins {
		l.joinIdx[j.ID] = i
	}

	if len(batch) == 0 {
		return
	}

	occ := l.occupancy(st)
	slots := l.nextSlots(st, len(batch))

	for i, j := range batch {
		key := view.MemberKey(j.ID, j.Incarnation)
		cell := view.JoinCell(occ, key)
		occ[cell]++

		m := view.Member{
			ID: j.ID, Incarnation: j.Incarnation, Key: key, Addr: j.Addr, Zone: j.Zone,
			State: view.Joining, Cell: cell,
		}
		st.ops = append(st.ops, view.OpAddMember(slots[i], m))
		st.added = append(st.added, slots[i])
		st.addedMem = append(st.addedMem, m)
		st.n++
		st.changed = true

		if l.hooks.OnAdmit != nil {
			l.hooks.OnAdmit(slots[i], j.ID)
		}
	}

	_ = epochNow
}

// ensureStamps sizes the stamp table for v.
func (l *Leader) ensureStamps(v *view.View) {
	if n := int(v.SlotCap()); len(l.stamps) < n {
		l.stamps = append(l.stamps, make([]stamp, n-len(l.stamps))...)
	}
}

// since returns the local time at which member s entered its current state.
func (l *Leader) since(s view.Slot, m *view.Member, now int64, epochNow uint64) int64 {
	st := &l.stamps[s]
	if st.key == m.Key && st.state == m.State {
		return st.at
	}

	var elapsed int64
	if epochNow > m.Since {
		elapsed = int64(epochNow-m.Since) * int64(l.cfg.EpochPeriod)
	}

	*st = stamp{key: m.Key, state: m.State, at: now - elapsed}

	return st.at
}

func (l *Leader) mark(s view.Slot, key uint64, state view.MemberState, now int64) {
	if int(s) < len(l.stamps) {
		l.stamps[s] = stamp{key: key, state: state, at: now}
	}
}

func (l *Leader) setState(st *state, s view.Slot, m *view.Member, state view.MemberState, w uint32, now int64) {
	op := view.OpSetState(s, state, w)
	st.ops = append(st.ops, op)
	st.setState[s] = op
	st.changed = true

	if state != m.State {
		l.mark(s, m.Key, state, now)
	}

	if l.hooks.OnPromote != nil {
		l.hooks.OnPromote(s, state, w)
	}
}

func (l *Leader) promote(st *state, now int64, epochNow uint64) {
	ww := l.cfg.WarmupWeights
	final := ww[len(ww)-1]

	st.v.ForEachMember(func(s view.Slot, m *view.Member) bool {
		if st.removed[s] {
			return true
		}

		at := l.since(s, m, now, epochNow)

		switch m.State {
		case view.Joining:
			if _, leaving := l.leaves[m.ID]; leaving {
				return true
			}

			if now-at >= int64(l.cfg.QuarantineFor) && st.budget() {
				l.setState(st, s, m, view.Active, ww[0], now)
			}
		case view.Active:
			if m.Weight == final {
				return true
			}

			step := int((now - at) / int64(l.cfg.WarmupStep))
			w := ww[min(step, len(ww)-1)]
			// A bootstrap leader starts at full weight; never lower one.
			if w > m.Weight && st.budget() {
				l.setState(st, s, m, view.Active, w, now)
			}
		}

		return true
	})
}

func (l *Leader) drains(st *state, now int64, epochNow uint64) {
	v := st.v

	if len(l.leaves) > 0 {
		ids := make([]string, 0, len(l.leaves))
		for id := range l.leaves {
			ids = append(ids, id)
		}

		slices.Sort(ids)

		for _, id := range ids {
			inc := l.leaves[id]
			delete(l.leaves, id)

			s, ok := v.Lookup(id)
			if !ok || st.removed[s] {
				continue
			}

			m, _ := v.Member(s)
			if m.Incarnation != inc || m.State == view.Draining {
				continue
			}

			if !st.budget() {
				l.leaves[id] = inc // retry next tick

				continue
			}

			l.setState(st, s, m, view.Draining, 0, now)
		}
	}

	v.ForEachMember(func(s view.Slot, m *view.Member) bool {
		if m.State != view.Draining || s == st.self || st.removed[s] {
			return true
		}

		if now-l.since(s, m, now, epochNow) >= int64(l.cfg.DrainFor) && st.budget() {
			l.remove(st, s, RemovedDrained)
		}

		return true
	})
}

func (l *Leader) suspicions(st *state, now int64) {
	if len(l.suspects) == 0 {
		return
	}

	v := st.v
	window := int64(l.cfg.SuspicionWindow)

	l.targets = l.targets[:0]
	for t := range l.suspects {
		l.targets = append(l.targets, t)
	}

	slices.Sort(l.targets)

	// Pass 1: expire reports and collect the reporting observer cells of
	// every suspected target.
	type cand struct {
		slot  view.Slot
		cells []uint32
	}

	var cands []cand

	for _, t := range l.targets {
		s := l.suspects[t]

		s.reports = slices.DeleteFunc(s.reports, func(r report) bool { return now-r.at > window })
		if len(s.reports) == 0 || !v.IsMember(t) || v.Key(t) != s.key || st.removed[t] {
			delete(l.suspects, t)

			continue
		}

		l.obs = view.Observers(v, t, l.obs[:0])

		var cells []uint32

		for _, r := range s.reports {
			if !slices.Contains(l.obs, r.observer) {
				continue
			}

			if c := v.Cell(r.observer); !slices.Contains(cells, c) {
				cells = append(cells, c)
			}
		}

		if len(cells) > 0 {
			cands = append(cands, cand{slot: t, cells: cells})
		}
	}

	isSuspected := func(s view.Slot) bool {
		_, ok := slices.BinarySearchFunc(cands, s, func(c cand, x view.Slot) int { return int(c.slot) - int(x) })

		return ok
	}

	// Pass 2: a target is due when observers in enough distinct cells
	// reported it. Observers that are themselves suspected do not count
	// towards the cells required, so two failed members that observe each
	// other can still be removed.
	var due []view.Slot

	for _, c := range cands {
		if c.slot == st.self {
			continue
		}

		l.obs = view.Observers(v, c.slot, l.obs[:0])

		var healthy []uint32

		for _, o := range l.obs {
			if cell := v.Cell(o); !isSuspected(o) && !slices.Contains(healthy, cell) {
				healthy = append(healthy, cell)
			}
		}

		need := max(1, min(l.cfg.ObserverCells, len(healthy)))
		if len(c.cells) >= need {
			due = append(due, c.slot)
		}
	}

	suspected := len(cands)

	n := v.Len()
	limit := max(1, n/1000)

	if suspected*100 > n*l.cfg.DampingPercent {
		limit = max(1, n/10000)

		if l.hooks.OnDamped != nil {
			l.hooks.OnDamped(suspected, n)
		}
	}

	for _, t := range due[:min(len(due), limit, max(0, maxMemberOps-len(st.ops)))] {
		delete(l.suspects, t)
		l.remove(st, t, RemovedSuspected)
	}
}

// cellOf returns the cell of slot s in the working state.
func (st *state) finalCell(s view.Slot, cur uint32) uint32 {
	if st.reshaped != nil {
		return st.reshaped[s]
	}

	if c, ok := st.moved[s]; ok {
		return c
	}

	return cur
}

func (l *Leader) cellOf(st *state, s view.Slot) uint32 {
	if i := slices.Index(st.added, s); i >= 0 {
		return st.addedMem[i].Cell
	}

	return st.v.Cell(s)
}

func (l *Leader) cells(st *state) {
	v := st.v
	occ := l.occupancy(st)
	sh := *v.Shape()

	maxOcc, minOcc := uint32(0), ^uint32(0)
	for _, o := range occ {
		maxOcc = max(maxOcc, o)
		minOcc = min(minOcc, o)
	}

	if view.NeedsReshape(sh, st.n, int(maxOcc), l.cfg.CellSize, l.cfg.MaxNeighbors) {
		l.reshape(st, sh)

		return
	}

	if len(occ) < 2 || maxOcc <= minOcc+1 {
		return
	}

	cands := make([]view.Candidate, 0, st.n)

	l.forEachLive(st, func(s view.Slot, key uint64, cell uint32, state view.MemberState) {
		cands = append(cands, view.Candidate{Member: s, Key: key, Cell: cell, Joining: state == view.Joining})
	})

	moves := view.BalanceMoves(occ, cands, l.cfg.MaxCellMovesPerTick)
	for _, mv := range moves {
		st.ops = append(st.ops, view.OpSetCell(mv.Member, mv.To))
		st.changed = true

		from := l.cellOf(st, mv.Member)
		occ[from]--
		occ[mv.To]++

		if i := slices.Index(st.added, mv.Member); i >= 0 {
			st.addedMem[i].Cell = mv.To
		} else {
			if st.moved == nil {
				st.moved = map[view.Slot]uint32{}
			}

			st.moved[mv.Member] = mv.To
		}
	}

	if len(moves) > 0 && l.hooks.OnCellMove != nil {
		l.hooks.OnCellMove(len(moves))
	}
}

// forEachLive calls fn for every member that will exist after the ops so
// far, in slot order, with its cell and state.
func (l *Leader) forEachLive(st *state, fn func(s view.Slot, key uint64, cell uint32, state view.MemberState)) {
	v := st.v
	v.ForEachMember(func(s view.Slot, m *view.Member) bool {
		if !st.removed[s] {
			state, _ := st.memberState(s)
			fn(s, m.Key, st.finalCell(s, m.Cell), state)
		}

		return true
	})

	for i, s := range st.added {
		m := &st.addedMem[i]
		fn(s, m.Key, st.finalCell(s, m.Cell), m.State)
	}
}

func (l *Leader) reshape(st *state, from view.Shape) {
	to := view.Choose(st.n, l.cfg.CellSize, l.cfg.MaxNeighbors)

	var (
		slots []view.Slot
		keys  []uint64
	)

	l.forEachLive(st, func(s view.Slot, key uint64, _ uint32, _ view.MemberState) {
		slots = append(slots, s)
		keys = append(keys, key)
	})

	assigned := view.ReshapeCells(to, keys)

	cells := make([]uint32, st.slotCap)
	for i := range cells {
		cells[i] = view.NoCell
	}

	for i, s := range slots {
		cells[s] = assigned[i]
	}

	for i, s := range st.added {
		st.addedMem[i].Cell = cells[s]
	}

	st.ops = append(st.ops, view.OpReshape(to, cells))
	st.changed = true
	st.reshaped = cells

	if l.hooks.OnReshape != nil {
		l.hooks.OnReshape(from, to)
	}
}

func (l *Leader) place(st *state) {
	v := st.v
	pm := l.pm[:0]

	l.forEachLive(st, func(s view.Slot, key uint64, cell uint32, _ view.MemberState) {
		w := uint32(0)
		if i := slices.Index(st.added, s); i < 0 {
			_, w = st.memberState(s)
		}

		pm = append(pm, control.Member{Slot: s, Key: key, Weight: w, Cell: cell})
	})

	removed := make([]view.Slot, 0, len(st.removed))
	for s := range st.removed {
		removed = append(removed, s)
	}

	slices.Sort(removed)

	for _, s := range removed {
		pm = append(pm, control.Member{Slot: s, Key: v.Key(s), Cell: v.Cell(s), Gone: true})
	}

	l.pm = pm

	h := uint64(len(pm))
	for _, m := range pm {
		h = view.Mix64(h ^ uint64(m.Slot)<<32 ^ uint64(m.Weight)<<1 ^ uint64(m.Cell)<<40 ^ m.Key)
		if m.Gone {
			h = view.Mix64(h + 1)
		}
	}

	if l.settledAt == v.Table() && l.settledH == h {
		return
	}

	plan, err := l.planner.Plan(v.Table(), pm)
	if err != nil {
		// Unreachable for a valid view: slots are unique and never NoMember.
		panic("leader: " + err.Error())
	}

	if l.hooks.OnPlan != nil && len(plan.Parts) > 0 {
		l.hooks.OnPlan(plan.Mandatory, len(plan.Parts)-plan.Mandatory)
	}

	if len(plan.Parts) == 0 {
		l.settledAt, l.settledH = v.Table(), h

		return
	}

	l.settledAt = nil

	st.ops = append(st.ops, view.OpAssign(slices.Clone(plan.Parts), slices.Clone(plan.Owners)))
}
