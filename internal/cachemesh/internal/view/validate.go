// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"fmt"
	"slices"

	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
)

// Validate checks every structural invariant of v in O(members +
// partitions + cells): member records (keys match IDs and incarnations,
// unique IDs, states, weights, cells in range), the ID index, the cell index
// (matches members, sorted by key then slot), free slots, per-slot loads
// against the table, that owners are members while the view has members,
// that the table matches the config, the shape, the leader, and the sorted
// keyspace generations. Errors wrap ErrInvalid.
func Validate(v *View) error {
	if err := validate(v); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	return nil
}

func validate(v *View) error {
	for i := 1; i < len(v.gens); i++ {
		if v.gens[i-1].Seed >= v.gens[i].Seed {
			return fmt.Errorf("generations not sorted at %d", i)
		}
	}

	for _, g := range v.gens {
		if g.Gen == 0 {
			return fmt.Errorf("zero generation for seed %x", g.Seed)
		}
	}

	if len(v.gens) > MaxGens {
		return fmt.Errorf("%d generations", len(v.gens))
	}

	if v.period < 0 {
		return fmt.Errorf("negative period")
	}

	if !v.hasConfig {
		if v.n != 0 || v.slotCap != 0 || v.table != nil || v.config != (Config{}) || v.shape != (topology.Shape{}) {
			return fmt.Errorf("members, table, or shape without config")
		}

		if v.version.Term > 0 {
			return fmt.Errorf("term %d without a leader member", v.version.Term)
		}

		return nil
	}

	if err := validateLayout(v); err != nil {
		return err
	}

	if err := validateMembers(v); err != nil {
		return err
	}

	if err := validateCells(v); err != nil {
		return err
	}

	if err := validateLoads(v); err != nil {
		return err
	}

	if v.version.Term > 0 && !v.IsMember(v.leader) {
		return fmt.Errorf("leader slot %d is not a member", v.leader)
	}

	return nil
}

func validateLayout(v *View) error {
	c := v.config
	if !placement.ValidBits(c.PartitionBits) {
		return fmt.Errorf("partition bits %d", c.PartitionBits)
	}

	if c.InitialOwner != NoSlot && c.InitialOwner >= MaxSlots {
		return fmt.Errorf("initial owner %d", c.InitialOwner)
	}

	if v.table == nil || v.table.Bits() != c.PartitionBits {
		return fmt.Errorf("table does not match %d partition bits", c.PartitionBits)
	}

	if err := v.shape.Validate(); err != nil {
		return err
	}

	if v.shape.Cells() > MaxCells {
		return fmt.Errorf("%d cells", v.shape.Cells())
	}

	if want := (v.shape.Cells() + cellChunk - 1) / cellChunk; len(v.cells) != int(want) {
		return fmt.Errorf("cell index has %d chunks, want %d", len(v.cells), want)
	}

	if v.slotCap > MaxSlots {
		return fmt.Errorf("slot cap %d", v.slotCap)
	}

	if want := (int(v.slotCap) + memChunk - 1) / memChunk; len(v.mem) != want || len(v.minReuse) != want {
		return fmt.Errorf("member storage has %d chunks, want %d", len(v.mem), want)
	}

	if len(v.ids) != 0 && v.idSize() < idxChunk || v.idSize()&(v.idSize()-1) != 0 {
		return fmt.Errorf("ID index size %d", v.idSize())
	}

	if uint64(v.n)*2 > v.idSize() && v.n > 0 {
		return fmt.Errorf("ID index too small")
	}

	return nil
}

func validateMembers(v *View) error {
	n := 0
	ids := make(map[string]Slot, v.n)

	for ci, c := range v.mem {
		minR := uint64(noReuse)

		for j := range memChunk {
			s := Slot(ci<<memShift + j)
			m := &c.mem[j]

			if s >= v.slotCap {
				if *m != (Member{}) || c.reuse[j] != 0 {
					return fmt.Errorf("slot %d beyond cap is not empty", s)
				}

				continue
			}

			if m.State == 0 {
				if *m != (Member{}) {
					return fmt.Errorf("free slot %d has member data", s)
				}

				minR = min(minR, c.reuse[j])

				continue
			}

			if c.reuse[j] != 0 {
				return fmt.Errorf("slot %d: member with reuse epoch", s)
			}

			if err := checkMember(m); err != nil {
				return fmt.Errorf("slot %d: %w", s, err)
			}

			if m.Cell >= v.shape.Cells() {
				return fmt.Errorf("slot %d: cell %d out of range", s, m.Cell)
			}

			if m.Joined > m.Since {
				return fmt.Errorf("slot %d: joined %d after since %d", s, m.Joined, m.Since)
			}

			if prev, dup := ids[m.ID]; dup {
				return fmt.Errorf("ID %q at slots %d and %d", m.ID, prev, s)
			}

			ids[m.ID] = s

			if got := v.lookupID(m.ID); got != s {
				return fmt.Errorf("ID index maps %q to %d, want %d", m.ID, got, s)
			}

			n++
		}

		if v.minReuse[ci] != minR {
			return fmt.Errorf("chunk %d: min reuse %d, want %d", ci, v.minReuse[ci], minR)
		}
	}

	if n != v.n || v.idCount != n {
		return fmt.Errorf("member count %d, recorded %d, indexed %d", n, v.n, v.idCount)
	}

	return nil
}

func validateCells(v *View) error {
	total := 0

	for ci, ch := range v.cells {
		if ch == nil {
			continue
		}

		if len(ch) != cellChunk {
			return fmt.Errorf("cell chunk %d has %d cells", ci, len(ch))
		}

		for j, cs := range ch {
			c := uint32(ci<<cellShift + j)
			if c >= v.shape.Cells() && len(cs) > 0 {
				return fmt.Errorf("members in cell %d beyond shape", c)
			}

			for i, s := range cs {
				if v.Cell(s) != c {
					return fmt.Errorf("cell %d lists slot %d in cell %d", c, s, v.Cell(s))
				}

				if i > 0 {
					p := cs[i-1]
					if kp, ks := v.Key(p), v.Key(s); kp > ks || kp == ks && p >= s {
						return fmt.Errorf("cell %d not sorted at %d", c, i)
					}
				}
			}

			total += len(cs)
		}
	}

	if total != v.n {
		return fmt.Errorf("cell index holds %d slots, want %d", total, v.n)
	}

	return nil
}

func validateLoads(v *View) error {
	loads := v.table.Loads(nil)
	orphans := uint64(0)

	for s, l := range loads {
		if l > 0 && !v.IsMember(Slot(s)) {
			orphans += uint64(l)
		}
	}

	for s := range max(len(loads), len(v.loads)<<loadShift) {
		want := uint32(0)
		if s < len(loads) {
			want = loads[s]
		}

		if got := v.Load(Slot(s)); got != want {
			return fmt.Errorf("slot %d: load %d, table has %d", s, got, want)
		}
	}

	if orphans != v.orphans {
		return fmt.Errorf("orphaned partitions %d, recorded %d", orphans, v.orphans)
	}

	if v.n > 0 && orphans > 0 {
		return fmt.Errorf("%d partitions owned by non-members", orphans)
	}

	return nil
}

// Equal reports whether a and b have identical content, including version
// and digest. Storage sharing is irrelevant.
func Equal(a, b *View) bool {
	if a == b {
		return true
	}

	if a.version != b.version || a.digest != b.digest || a.leader != b.leader ||
		a.config != b.config || a.hasConfig != b.hasConfig || a.shape != b.shape ||
		a.epoch != b.epoch || a.period != b.period || a.n != b.n || a.slotCap != b.slotCap ||
		!slices.Equal(a.gens, b.gens) {
		return false
	}

	if (a.table == nil) != (b.table == nil) || a.table != nil && !placement.Equal(a.table, b.table) {
		return false
	}

	for ci := range a.mem {
		if a.mem[ci] != b.mem[ci] && *a.mem[ci] != *b.mem[ci] {
			return false
		}
	}

	return true
}
