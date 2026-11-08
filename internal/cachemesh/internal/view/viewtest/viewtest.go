// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package viewtest builds valid, large views and realistic deltas for tests
// and benchmarks of packages built on view. Everything is deterministic in
// its inputs.
package viewtest

import (
	"strconv"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
)

// Options tune Build. The zero value builds an all-Active cluster with the
// default partition bits, cell size 4, and 256 maximum neighbors.
type Options struct {
	// PartitionBits defaults to placement.DefaultBits.
	PartitionBits uint8
	// CellSize and MaxNeighbors feed topology.Choose; defaults 4 and 256.
	CellSize, MaxNeighbors int
	// Joining and Draining members are taken from the highest slots; the
	// rest are Active with weight placement.MaxWeight.
	Joining, Draining int
	// Seed varies member incarnations (and therefore keys).
	Seed uint64
	// Epoch is the view epoch; default 1. Period defaults to 15s.
	Epoch  uint64
	Period time.Duration
}

func (o *Options) defaults() {
	if o.PartitionBits == 0 {
		o.PartitionBits = placement.DefaultBits
	}

	if o.CellSize <= 0 {
		o.CellSize = 4
	}

	if o.MaxNeighbors <= 0 {
		o.MaxNeighbors = 256
	}

	if o.Epoch == 0 {
		o.Epoch = 1
	}

	if o.Period <= 0 {
		o.Period = 15 * time.Second
	}
}

// NewMember returns a valid member record (Joining, weight 0, cell 0).
func NewMember(id string, incarnation uint64) view.Member {
	return view.Member{
		ID:          id,
		Incarnation: incarnation,
		Key:         hashing.MemberKey(id, incarnation),
		Addr:        id + ".example:7946",
		Zone:        "zone-" + strconv.FormatUint(incarnation%3, 10),
		State:       view.Joining,
	}
}

// batch is the number of member ops per delta in Build.
const batch = 32768

// Build returns a view at term 1 with n members in slots 0..n-1, leader slot
// 0, a shape from topology.Choose with cells assigned by
// topology.ReshapeCells, and every partition owned by an Active member
// (round-robin by partition, so loads are within one of each other). If no
// member is Active, partitions go to all members. n == 0 gives a configured
// view at term 0 with no members. It fails tb on any error.
func Build(tb testing.TB, n int, opts Options) *view.View {
	tb.Helper()
	opts.defaults()

	v := view.Genesis()
	sh := topology.Choose(n, opts.CellSize, opts.MaxNeighbors)

	ops := []view.Op{}
	if n > 0 {
		ops = append(ops, view.OpNewTerm(1, 0))
	}

	ops = append(ops,
		view.OpConfig(view.Config{PartitionBits: opts.PartitionBits, InitialOwner: view.NoSlot, SlotReuseEpochs: 16, HandoffEpochs: 8}),
		view.OpEpoch(opts.Epoch, opts.Period),
		view.OpReshape(sh, nil),
	)

	members := make([]view.Member, n)
	keys := make([]uint64, n)

	for i := range members {
		members[i] = NewMember("m-"+strconv.Itoa(i), hashing.Mix64(opts.Seed+uint64(i))|1)
		keys[i] = members[i].Key
	}

	cells := topology.ReshapeCells(sh, keys)

	var active []uint32

	for i := range members {
		m := &members[i]
		m.Cell = cells[i]

		switch {
		case i >= n-opts.Draining:
			m.State = view.Draining
		case i >= n-opts.Draining-opts.Joining:
			m.State = view.Joining
		default:
			m.State, m.Weight = view.Active, placement.MaxWeight

			active = append(active, uint32(i))
		}

		ops = append(ops, view.OpAddMember(uint32(i), *m))
		if len(ops) >= batch {
			v = apply(tb, v, ops)
			ops = nil
		}
	}

	if len(active) == 0 {
		for i := range n {
			active = append(active, uint32(i))
		}
	}

	if len(active) > 0 {
		p := 1 << opts.PartitionBits
		parts := make([]uint32, p)
		owners := make([]uint32, p)

		for i := range parts {
			parts[i] = uint32(i)
			owners[i] = active[i%len(active)]
		}

		ops = append(ops, view.OpAssign(parts, owners))
	}

	return apply(tb, v, ops)
}

func apply(tb testing.TB, v *view.View, ops []view.Op) *view.View {
	tb.Helper()

	nv, err := view.Apply(v, view.NewDelta(v, ops...))
	if err != nil {
		tb.Fatalf("viewtest: %v", err)
	}

	return nv
}

// Churn describes a realistic leader delta.
type Churn struct {
	// Admit new Joining members at NextSlot-style slots in the least
	// occupied cells.
	Admit int
	// Remove members (never the leader), handing their partitions to other
	// members first.
	Remove int
	// Moves partitions between members.
	Moves int
	// Epoch adds an Epoch op advancing the epoch by one.
	Epoch bool
}

// Delta returns a sealed delta on v performing c. Removal scans the table,
// so building it costs O(partitions); applying it does not.
func Delta(tb testing.TB, v *view.View, c Churn) *view.Delta {
	tb.Helper()

	var ops []view.Op

	epoch := v.Epoch()
	if c.Epoch {
		epoch++
		ops = append(ops, view.OpEpoch(epoch, max(v.Period(), time.Second)))
	}

	ops = append(ops, admitOps(v, c.Admit, epoch)...)
	ops = append(ops, removeOps(v, c.Remove)...)

	if c.Moves > 0 {
		ops = append(ops, moveOps(v, c.Moves, removed(ops))...)
	}

	return view.NewDelta(v, ops...)
}

func removed(ops []view.Op) map[uint32]bool {
	out := map[uint32]bool{}

	for _, op := range ops {
		if op.Kind == view.KindRemoveMember {
			out[op.Slot] = true
		}
	}

	return out
}

func admitOps(v *view.View, k int, epoch uint64) []view.Op {
	if k <= 0 {
		return nil
	}

	var slots []uint32

	for _, f := range v.FreeSlots() {
		if len(slots) == k {
			break
		}

		if f.ReusableAt <= epoch {
			slots = append(slots, f.Slot)
		}
	}

	for s := v.SlotCap(); len(slots) < k; s++ {
		slots = append(slots, s)
	}

	occ := make([]uint32, v.Shape().Cells())
	for c := range occ {
		occ[c] = uint32(v.Occupancy(uint32(c)))
	}

	ops := make([]view.Op, 0, k)

	for i, s := range slots {
		id := "a-" + v.Version().String() + "-" + strconv.Itoa(i)
		m := NewMember(id, hashing.Mix64(v.Version().Seq<<20+uint64(i))|1)
		m.Cell = topology.JoinCell(occ, m.Key)
		occ[m.Cell]++
		ops = append(ops, view.OpAddMember(s, m))
	}

	return ops
}

// candidates returns members other than the leader in slot order.
func candidates(v *view.View) []uint32 {
	var out []uint32

	v.ForEachMember(func(s uint32, _ *view.Member) bool {
		if s != v.Leader() {
			out = append(out, s)
		}

		return true
	})

	return out
}

func removeOps(v *view.View, k int) []view.Op {
	cand := candidates(v)
	k = min(k, len(cand))

	if k <= 0 {
		return nil
	}

	gone := map[uint32]bool{}
	step := len(cand) / k
	off := int(v.Version().Seq) % step

	for i := range k {
		gone[cand[off+i*step]] = true
	}

	var keep []uint32

	v.ForEachMember(func(s uint32, _ *view.Member) bool {
		if !gone[s] {
			keep = append(keep, s)
		}

		return true
	})

	var parts, owners []uint32

	t := v.Table()
	for p := range uint32(t.Len()) {
		if gone[t.Owner(p)] {
			parts = append(parts, p)
			owners = append(owners, keep[len(parts)%len(keep)])
		}
	}

	var ops []view.Op
	if len(parts) > 0 {
		ops = append(ops, view.OpAssign(parts, owners))
	}

	for _, s := range cand {
		if gone[s] {
			ops = append(ops, view.OpRemoveMember(s))
		}
	}

	return ops
}

func moveOps(v *view.View, k int, gone map[uint32]bool) []view.Op {
	var members []uint32

	v.ForEachMember(func(s uint32, _ *view.Member) bool {
		if !gone[s] {
			members = append(members, s)
		}

		return true
	})

	t := v.Table()
	if len(members) == 0 || t == nil {
		return nil
	}

	k = min(k, t.Len())
	mask := uint64(t.Len() - 1)
	seen := make(map[uint32]bool, k)
	parts := make([]uint32, 0, k)
	owners := make([]uint32, 0, k)

	// Pseudo-random distinct partitions, like the rebalancer's scattered
	// moves.
	for i := uint64(0); len(seen) < k; i++ {
		h := hashing.Mix64(i ^ v.Version().Seq<<32)
		p := uint32(h & mask)

		if seen[p] {
			continue
		}

		seen[p] = true

		o := members[int((h>>32)%uint64(len(members)))]
		if o == t.Owner(p) {
			continue
		}

		parts = append(parts, p)
		owners = append(owners, o)
	}

	return []view.Op{view.OpAssign(parts, owners)}
}
