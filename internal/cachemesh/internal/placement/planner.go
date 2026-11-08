// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"fmt"
	"slices"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
)

// MaxWeight is the full weight of an active, warmed-up member.
const MaxWeight = 1000

// DefaultMaxMoves is the default cap on optional moves per plan.
const DefaultMaxMoves = 4096

// Member is a cluster member as seen by the rebalancer.
type Member struct {
	Slot   uint32 // owner value in the table; must be unique and not NoMember
	Key    uint64 // Member.Key, used for ranking and tie breaks
	Weight uint32 // 0..MaxWeight; 0 means the member must own nothing
	Cell   uint32 // grid cell; cell-mates are members with the same Cell
	// Gone marks a member that has been removed but is listed so its Cell
	// is known: its partitions are handed to its cell-mates first. A Gone
	// member is never assigned partitions. Owners not listed at all are
	// removed members of unknown cell.
	Gone bool
}

// Config tunes the Planner. The zero value is valid.
type Config struct {
	// MaxMoves caps optional moves per plan. Zero means DefaultMaxMoves;
	// negative disables optional moves.
	MaxMoves int
}

// Plan is a set of partition moves for Table.Assign. Parts and Owners are
// owned by the Planner and valid until its next Plan call.
type Plan struct {
	Parts  []uint32
	Owners []uint32
	// Mandatory is the number of leading moves that were mandatory.
	Mandatory int
}

// Planner computes rebalancing plans. It keeps scratch buffers between
// calls but no state that affects results: every plan is a pure function of
// the table contents and the member set. Each call scans the table twice,
// O(P + N log N + moves * log N); about 2^20 partitions and 10^5 members take
// a few milliseconds. A Planner is not safe for concurrent use.
//
// Rules, per tick:
//
//  1. Targets: with W the sum of assignable weights, member i gets
//     floor(P*w_i/W); the remainder goes one each to assignable members in
//     ascending Key order (ties by Slot). Assignable means listed, not Gone,
//     and weight > 0. If no listed member that is not Gone has weight > 0
//     (bootstrap), every such member is assignable with weight MaxWeight.
//  2. Mandatory: every partition whose owner is not assignable (NoMember,
//     unlisted, Gone, or weight 0) gets a new owner, in ascending partition
//     order. If the old owner is listed, its assignable cell-mate with the
//     best hashing.Score(Key, partition) (hashing.Better) whose load is below
//     its target wins. Otherwise the assignable member with the smallest
//     load - target wins (ties by Key, then Slot).
//  3. Optional: up to MaxMoves times, move one partition from the member with
//     the largest load - target (ties by Key, then Slot) to the member with
//     the smallest, while the first is over and the second under target. The
//     moved partition is the donor's lowest-numbered partition not yet moved.
//
// After enough ticks every assignable member's load equals its target.
type Planner struct {
	cfg Config

	slotIdx    []int32
	assign     []bool
	weight     []uint64
	target     []int64
	load       []int64
	order      []int32
	keyed      []keyed
	packed     []uint64
	rank       []int32
	cellsBuilt bool
	orphans    []uint32
	offs       []int32
	cursor     []int32
	donated    []uint32
	under      heap
	over       heap
	parts      []uint32
	owners     []uint32
	members    []Member
}

// NewPlanner returns a Planner.
func NewPlanner(cfg Config) *Planner {
	return &Planner{cfg: cfg}
}

func (pl *Planner) maxMoves() int {
	switch {
	case pl.cfg.MaxMoves == 0:
		return DefaultMaxMoves
	case pl.cfg.MaxMoves < 0:
		return 0
	}

	return pl.cfg.MaxMoves
}

// prepare indexes members and computes targets. It returns the number of
// assignable members.
func (pl *Planner) prepare(p int, members []Member) (int, error) {
	n := len(members)
	pl.members = members

	maxSlot := -1

	for _, m := range members {
		if m.Slot == NoMember {
			return 0, fmt.Errorf("%w: member slot NoMember", ErrInvalid)
		}

		maxSlot = max(maxSlot, int(m.Slot))
	}

	pl.slotIdx = resize(pl.slotIdx, maxSlot+1)
	for i := range pl.slotIdx {
		pl.slotIdx[i] = -1
	}

	for i, m := range members {
		if pl.slotIdx[m.Slot] >= 0 {
			return 0, fmt.Errorf("%w: duplicate member slot %d", ErrInvalid, m.Slot)
		}

		pl.slotIdx[m.Slot] = int32(i)
	}

	pl.assign = resize(pl.assign, n)
	pl.weight = resize(pl.weight, n)
	pl.target = resize(pl.target, n)
	pl.load = resize(pl.load, n)

	bootstrap := true

	for _, m := range members {
		if !m.Gone && m.Weight > 0 {
			bootstrap = false

			break
		}
	}

	var total uint64

	pl.keyed = pl.keyed[:0]

	for i, m := range members {
		w := uint64(m.Weight)
		if bootstrap {
			w = MaxWeight
		}

		pl.assign[i] = !m.Gone && w > 0
		pl.weight[i] = w
		pl.target[i] = 0
		pl.load[i] = 0

		if pl.assign[i] {
			total += w

			pl.keyed = append(pl.keyed, keyed{key: m.Key, slot: m.Slot, idx: int32(i)})
		}
	}

	slices.SortFunc(pl.keyed, func(a, b keyed) int {
		if a.key != b.key {
			if a.key < b.key {
				return -1
			}

			return 1
		}

		return cmp32(a.slot, b.slot)
	})

	pl.order = pl.order[:0]
	for _, k := range pl.keyed {
		pl.order = append(pl.order, k.idx)
	}

	pl.rank = resize(pl.rank, n)
	for r, i := range pl.order {
		pl.rank[i] = int32(r)
	}

	if total > 0 {
		rem := int64(p)

		for _, i := range pl.order {
			pl.target[i] = int64(uint64(p) * pl.weight[i] / total)
			rem -= pl.target[i]
		}

		for _, i := range pl.order[:rem] {
			pl.target[i]++
		}
	}

	return len(pl.order), nil
}

// keyed is a sort record for ordering members by Key, then Slot.
type keyed struct {
	key  uint64
	slot uint32
	idx  int32
}

// owner returns the member index of slot o, or -1.
func (pl *Planner) owner(o uint32) int32 {
	if int(o) < len(pl.slotIdx) {
		return pl.slotIdx[o]
	}

	return -1
}

// Plan computes the moves for one tick. It returns an error for duplicate or
// NoMember slots in members.
func (pl *Planner) Plan(t *Table, members []Member) (Plan, error) {
	nAssign, err := pl.prepare(t.Len(), members)
	if err != nil {
		return Plan{}, err
	}

	pl.parts, pl.owners = pl.parts[:0], pl.owners[:0]

	defer func() { pl.members = nil }()

	if nAssign == 0 {
		return Plan{}, nil
	}

	pl.scan(t)

	pl.cellsBuilt = false

	pl.under.init(pl, false)

	for _, p := range pl.orphans {
		to := pl.mandatoryOwner(t.Owner(p), p)
		pl.load[to]++
		pl.under.fix(to)
		pl.parts = append(pl.parts, p)
		pl.owners = append(pl.owners, members[to].Slot)
	}

	mandatory := len(pl.parts)

	pl.over.init(pl, true)

	for range pl.maxMoves() {
		d, r := pl.over.top(), pl.under.top()
		if d < 0 || r < 0 || pl.load[d] <= pl.target[d] || pl.load[r] >= pl.target[r] {
			break
		}

		p := pl.donated[pl.cursor[d]]
		pl.cursor[d]++
		pl.load[d]--
		pl.load[r]++
		pl.over.fix(d)
		pl.over.fix(r)
		pl.under.fix(d)
		pl.under.fix(r)
		pl.parts = append(pl.parts, p)
		pl.owners = append(pl.owners, members[r].Slot)
	}

	return Plan{Parts: pl.parts, Owners: pl.owners, Mandatory: mandatory}, nil
}

// scan computes loads, the orphan list, and each assignable member's
// partitions in ascending order (pl.donated[pl.offs[i]:pl.offs[i+1]]).
func (pl *Planner) scan(t *Table) {
	n := len(pl.members)
	pl.orphans = pl.orphans[:0]

	for ci := range t.NumChunks() {
		base := uint32(ci) << chunkShift
		owners, _, _ := t.Columns(ci)
		for j, o := range owners {
			i := pl.owner(o)
			if i >= 0 && pl.assign[i] {
				pl.load[i]++
			} else {
				pl.orphans = append(pl.orphans, base+uint32(j))
			}
		}
	}

	pl.offs = resize(pl.offs, n+1)
	pl.offs[0] = 0

	for i := range n {
		pl.offs[i+1] = pl.offs[i] + int32(pl.load[i])
	}

	pl.cursor = resize(pl.cursor, n)
	copy(pl.cursor, pl.offs[:n])
	pl.donated = resize(pl.donated, int(pl.offs[n]))

	for ci := range t.NumChunks() {
		base := uint32(ci) << chunkShift
		owners, _, _ := t.Columns(ci)
		for j, o := range owners {
			i := pl.owner(o)
			if i >= 0 && pl.assign[i] {
				pl.donated[pl.cursor[i]] = base + uint32(j)
				pl.cursor[i]++
			}
		}
	}

	copy(pl.cursor, pl.offs[:n])
}

// buildCells sorts assignable members by cell, then Key, then Slot, packed
// as (cell << 32 | key rank) so the sort is a plain integer sort.
func (pl *Planner) buildCells() {
	pl.cellsBuilt = true
	pl.packed = pl.packed[:0]

	for rank, i := range pl.order {
		pl.packed = append(pl.packed, uint64(pl.members[i].Cell)<<32|uint64(rank))
	}

	slices.Sort(pl.packed)
}

// cellMates returns the packed entries of assignable members in cell c.
func (pl *Planner) cellMates(c uint32) []uint64 {
	if !pl.cellsBuilt {
		pl.buildCells()
	}

	lo, _ := slices.BinarySearch(pl.packed, uint64(c)<<32)
	hi, _ := slices.BinarySearch(pl.packed, (uint64(c)+1)<<32)

	return pl.packed[lo:hi]
}

func cmp32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

func (pl *Planner) mandatoryOwner(old, p uint32) int32 {
	if i := pl.owner(old); i >= 0 {
		best := int32(-1)

		var bestScore uint64

		for _, pk := range pl.cellMates(pl.members[i].Cell) {
			j := pl.order[uint32(pk)]
			if j == i || pl.load[j] >= pl.target[j] {
				continue
			}

			k := pl.members[j].Key

			sc := hashing.Score(k, uint64(p))
			if best < 0 || hashing.Better(sc, k, bestScore, pl.members[best].Key) {
				best, bestScore = j, sc
			}
		}

		if best >= 0 {
			return best
		}
	}

	return pl.under.top()
}

// Settled reports whether every assignable member's load is within one of
// its target and no partition is owned by a non-assignable owner. It is
// trivially true when there is nothing to assign to.
func (pl *Planner) Settled(t *Table, members []Member) (bool, error) {
	n, err := pl.prepare(t.Len(), members)

	defer func() { pl.members = nil }()

	if err != nil || n == 0 {
		return n == 0 && err == nil, err
	}

	for ci := range t.NumChunks() {
		owners, _, _ := t.Columns(ci)
		for _, o := range owners {
			i := pl.owner(o)
			if i < 0 || !pl.assign[i] {
				return false, nil
			}

			pl.load[i]++
		}
	}

	for _, i := range pl.order {
		if d := pl.load[i] - pl.target[i]; d > 1 || d < -1 {
			return false, nil
		}
	}

	return true, nil
}

// Targets returns each member's target load (indexed like members) under
// the Planner rules; non-assignable members get 0.
func (pl *Planner) Targets(bits uint8, members []Member) ([]int64, error) {
	if _, err := pl.prepare(1<<bits, members); err != nil {
		return nil, err
	}

	pl.members = nil

	return slices.Clone(pl.target[:len(members)]), nil
}

func resize[T any](s []T, n int) []T {
	if cap(s) < n {
		return make([]T, n)
	}

	return s[:n]
}

// heap is an indexed binary heap of assignable member indices ordered by
// load - target (ascending, or descending when max), then Key, then Slot
// (the precomputed rank).
type heap struct {
	pl  *Planner
	max bool
	h   []int32
	pos []int32
}

func (h *heap) init(pl *Planner, isMax bool) {
	h.pl, h.max = pl, isMax
	h.pos = resize(h.pos, len(pl.members))

	for i := range h.pos {
		h.pos[i] = -1
	}

	// Only members strictly over (max) or under (min) target can ever be
	// chosen: mandatory and optional moves never push a member across its
	// target, so the heaps stay small in a settled cluster.
	h.h = h.h[:0]

	for _, i := range pl.order {
		d := pl.load[i] - pl.target[i]
		if (isMax && d > 0) || (!isMax && d < 0) {
			h.h = append(h.h, i)
		}
	}

	for i, m := range h.h {
		h.pos[m] = int32(i)
	}

	for i := len(h.h)/2 - 1; i >= 0; i-- {
		h.down(i)
	}
}

func (h *heap) less(a, b int32) bool {
	pl := h.pl
	da, db := pl.load[a]-pl.target[a], pl.load[b]-pl.target[b]

	if da != db {
		if h.max {
			return da > db
		}

		return da < db
	}

	return pl.rank[a] < pl.rank[b]
}

// top returns the first member, or -1 if the heap is empty.
func (h *heap) top() int32 {
	if len(h.h) == 0 {
		return -1
	}

	return h.h[0]
}

func (h *heap) fix(m int32) {
	i := int(h.pos[m])
	if i < 0 {
		return
	}

	if !h.down(i) {
		h.up(i)
	}
}

func (h *heap) swap(i, j int) {
	h.h[i], h.h[j] = h.h[j], h.h[i]
	h.pos[h.h[i]] = int32(i)
	h.pos[h.h[j]] = int32(j)
}

func (h *heap) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !h.less(h.h[i], h.h[p]) {
			return
		}

		h.swap(i, p)
		i = p
	}
}

func (h *heap) down(i0 int) bool {
	i, n := i0, len(h.h)
	for {
		l := 2*i + 1
		if l >= n {
			break
		}

		c := l
		if r := l + 1; r < n && h.less(h.h[r], h.h[l]) {
			c = r
		}

		if !h.less(h.h[c], h.h[i]) {
			break
		}

		h.swap(i, c)
		i = c
	}

	return i > i0
}
