// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"crypto/sha256"
	"math"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
)

const (
	memShift  = 8
	memChunk  = 1 << memShift
	memMask   = memChunk - 1
	loadShift = 10
	loadChunk = 1 << loadShift
	loadMask  = loadChunk - 1
	cellShift = 8
	cellChunk = 1 << cellShift
	cellMask  = cellChunk - 1
	idxShift  = 10
	idxChunk  = 1 << idxShift

	noReuse = math.MaxUint64
	idSeed  = 0x6361636865696473 // "cacheids"
)

// memberChunk holds memChunk slots. A slot without a member has State 0 and
// keeps its ReusableAt in reuse.
type memberChunk struct {
	mem   [memChunk]Member
	reuse [memChunk]uint64
}

// View is an immutable membership view. All methods are safe for concurrent
// use. Slices and pointers returned by a View are shared and must not be
// modified. Views share unchanged storage chunks with the views they were
// derived from.
type View struct {
	version Version
	digest  Digest
	leader  Slot

	config    Config
	hasConfig bool
	shape     topology.Shape
	table     *placement.Table

	epoch  uint64
	period time.Duration
	gens   []KeyspaceGen

	n       int    // members
	slotCap uint32 // slots in use or free; members are below slotCap
	orphans uint64 // partitions owned by non-member slots

	mem      []*memberChunk
	minReuse []uint64   // per member chunk: min ReusableAt of its free slots
	loads    [][]uint32 // per slot partition counts; nil chunk is zero
	cells    [][][]Slot // per cell sorted slots; nil chunk is all empty
	ids      [][]Slot   // open-addressing ID index chunks, NoSlot empty
	idCount  int        // entries in ids
}

var genesisDigest = Digest(sha256.Sum256([]byte("cachemesh/view/v1/genesis")))

var genesis = &View{digest: genesisDigest, leader: NoSlot}

// GenesisDigest is the digest of the genesis view.
func GenesisDigest() Digest { return genesisDigest }

// Genesis returns the empty view at the zero Version: no config, no members,
// no leader. Every cluster starts from it.
func Genesis() *View { return genesis }

// Version returns the view version.
func (v *View) Version() Version { return v.version }

// Digest returns the content digest.
func (v *View) Digest() Digest { return v.digest }

// Term returns the leader term, Version().Term.
func (v *View) Term() uint64 { return v.version.Term }

// Leader returns the slot of the term's leader, or NoSlot at term 0.
func (v *View) Leader() Slot { return v.leader }

// Config returns the cluster constants and whether they have been set.
func (v *View) Config() (Config, bool) { return v.config, v.hasConfig }

// PartitionBits returns log2 of the partition count, or 0 without config.
func (v *View) PartitionBits() uint8 { return v.config.PartitionBits }

// Shape returns the grid shape. It is the zero (invalid) shape without
// config.
func (v *View) Shape() *topology.Shape { return &v.shape }

// Table returns the partition table, or nil without config.
func (v *View) Table() *placement.Table { return v.table }

// Epoch returns the epoch anchor.
func (v *View) Epoch() uint64 { return v.epoch }

// Period returns the epoch period, or 0 before the first Epoch op.
func (v *View) Period() time.Duration { return v.period }

// Gen returns the generation of keyspace seed.
func (v *View) Gen(seed uint64) uint32 {
	i, ok := findGen(v.gens, seed)
	if !ok {
		return 0
	}

	return v.gens[i].Gen
}

// Gens returns every non-zero keyspace generation sorted by seed.
func (v *View) Gens() []KeyspaceGen { return v.gens[:len(v.gens):len(v.gens)] }

func findGen(g []KeyspaceGen, seed uint64) (int, bool) {
	lo, hi := 0, len(g)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if g[m].Seed < seed {
			lo = m + 1
		} else {
			hi = m
		}
	}

	return lo, lo < len(g) && g[lo].Seed == seed
}

// Len returns the number of members.
func (v *View) Len() int { return v.n }

// SlotCap returns one past the highest slot ever used. Every slot below it
// is a member or a free slot.
func (v *View) SlotCap() uint32 { return v.slotCap }

// Member returns the member at slot. The pointer is shared and must not be
// modified.
func (v *View) Member(s Slot) (*Member, bool) {
	if s >= v.slotCap {
		return nil, false
	}

	m := &v.mem[s>>memShift].mem[s&memMask]

	return m, m.State != 0
}

// Key returns the key of the member at slot, or 0.
func (v *View) Key(s Slot) uint64 {
	if s >= v.slotCap {
		return 0
	}

	return v.mem[s>>memShift].mem[s&memMask].Key
}

// Cell returns the cell of the member at slot, or topology.NoCell.
func (v *View) Cell(s Slot) uint32 {
	if s >= v.slotCap {
		return topology.NoCell
	}

	m := &v.mem[s>>memShift].mem[s&memMask]
	if m.State == 0 {
		return topology.NoCell
	}

	return m.Cell
}

// State returns the state of the member at slot, or 0 if there is none.
func (v *View) State(s Slot) MemberState {
	if s >= v.slotCap {
		return 0
	}

	return v.mem[s>>memShift].mem[s&memMask].State
}

// IsMember reports whether slot holds a member.
func (v *View) IsMember(s Slot) bool { return v.State(s) != 0 }

// CellMembers returns the members of cell sorted by Key ascending, ties by
// slot. A member's index in it is its rank. The slice is shared.
func (v *View) CellMembers(c uint32) []Slot {
	ci := c >> cellShift
	if int(ci) >= len(v.cells) || v.cells[ci] == nil {
		return nil
	}

	s := v.cells[ci][c&cellMask]

	return s[:len(s):len(s)]
}

// Occupancy returns the number of members in cell.
func (v *View) Occupancy(c uint32) int { return len(v.CellMembers(c)) }

// Load returns the number of partitions owned by slot.
func (v *View) Load(s Slot) uint32 {
	ci := s >> loadShift
	if int(ci) >= len(v.loads) || v.loads[ci] == nil {
		return 0
	}

	return v.loads[ci][s&loadMask]
}

// ForEachMember calls fn for every member in slot order until fn returns
// false.
func (v *View) ForEachMember(fn func(Slot, *Member) bool) {
	for s := range v.slotCap {
		m := &v.mem[s>>memShift].mem[s&memMask]
		if m.State != 0 && !fn(s, m) {
			return
		}
	}
}

// FreeSlots returns every free slot in slot order.
func (v *View) FreeSlots() []FreeSlot {
	var out []FreeSlot

	for s := range v.slotCap {
		c := v.mem[s>>memShift]
		if c.mem[s&memMask].State == 0 {
			out = append(out, FreeSlot{Slot: s, ReusableAt: c.reuse[s&memMask]})
		}
	}

	return out
}

// NextSlot returns the lowest free slot reusable at the view epoch, or
// SlotCap if there is none.
func (v *View) NextSlot() Slot {
	for ci, m := range v.minReuse {
		if m > v.epoch {
			continue
		}

		c := v.mem[ci]
		for j := range memChunk {
			s := Slot(ci<<memShift + j)
			if s >= v.slotCap {
				break
			}

			if c.mem[j].State == 0 && c.reuse[j] <= v.epoch {
				return s
			}
		}
	}

	return v.slotCap
}

// Lookup returns the slot of the member with the given ID.
func (v *View) Lookup(id string) (Slot, bool) {
	s := v.lookupID(id)

	return s, s != NoSlot
}

func idHash(id string) uint64 { return hashing.KeyHashString(idSeed, id) }

func (v *View) idSize() uint64 { return uint64(len(v.ids)) << idxShift }

func (v *View) idAt(i uint64) Slot { return v.ids[i>>idxShift][i&(idxChunk-1)] }

func (v *View) lookupID(id string) Slot {
	if len(v.ids) == 0 {
		return NoSlot
	}

	mask := v.idSize() - 1
	for i := idHash(id) & mask; ; i = (i + 1) & mask {
		s := v.idAt(i)
		if s == NoSlot {
			return NoSlot
		}

		if v.mem[s>>memShift].mem[s&memMask].ID == id {
			return s
		}
	}
}
