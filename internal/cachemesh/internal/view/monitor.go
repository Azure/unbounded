// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"slices"

	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
)

// The monitoring relation (designs/cachemesh.md, "Monitoring and failure
// detection"). A member s in cell c with rank r (its index in
// CellMembers(c)) monitors:
//
//   - every other member of c, and
//   - for each dimension j with Size[j] > 1 and direction +1 and -1
//     (wrapping, counted once when both directions reach the same cell), the
//     member of the adjacent cell a with rank r mod Occupancy(a), if a is
//     not empty.
//
// Observers is the exact inverse of Subjects.

// rank returns the index of s in its cell, or -1.
func rank(cs []Slot, s Slot) int { return slices.Index(cs, s) }

// forEachAdjacent calls fn with each distinct cell adjacent to c along a
// dimension of size > 1, one per (dimension, direction) pair.
func forEachAdjacent(v *View, c uint32, fn func(a uint32)) {
	sh := &v.shape
	for j := range int(sh.Dims) {
		if sh.Size[j] < 2 {
			continue
		}

		up := sh.Step(c, j, 1)
		fn(up)

		if down := sh.Step(c, j, -1); down != up {
			fn(down)
		}
	}
}

// Subjects appends to dst the members slot monitors: its cell-mates in rank
// order, then one member per adjacent cell in dimension then direction (+1,
// -1) order. It appends nothing if slot is not a member.
func Subjects(v *View, slot Slot, dst []Slot) []Slot {
	c := v.Cell(slot)
	if c == topology.NoCell {
		return dst
	}

	cs := v.CellMembers(c)
	r := rank(cs, slot)

	for _, s := range cs {
		if s != slot {
			dst = append(dst, s)
		}
	}

	forEachAdjacent(v, c, func(a uint32) {
		if as := v.CellMembers(a); len(as) > 0 {
			dst = append(dst, as[r%len(as)])
		}
	})

	return dst
}

// Observers appends to dst the members that monitor slot, the exact inverse
// of Subjects: its cell-mates, then for each adjacent cell a (in the order
// Subjects uses) the members of a whose rank mod Occupancy(cell of slot)
// equals slot's rank.
func Observers(v *View, slot Slot, dst []Slot) []Slot {
	c := v.Cell(slot)
	if c == topology.NoCell {
		return dst
	}

	cs := v.CellMembers(c)
	r := rank(cs, slot)

	for _, s := range cs {
		if s != slot {
			dst = append(dst, s)
		}
	}

	occ := len(cs)

	// Members of a reach c in the opposite direction; the set of cells is
	// the same either way, and each is visited once.
	forEachAdjacent(v, c, func(a uint32) {
		as := v.CellMembers(a)
		for i := r; i < len(as); i += occ {
			dst = append(dst, as[i])
		}
	})

	return dst
}

// ObserverCells returns the number of distinct cells holding observers of
// slot, which bounds the ObserverCells suspicion quorum.
func ObserverCells(v *View, slot Slot) int {
	c := v.Cell(slot)
	if c == topology.NoCell {
		return 0
	}

	cs := v.CellMembers(c)
	r := rank(cs, slot)

	n := 0
	if len(cs) > 1 {
		n++
	}

	forEachAdjacent(v, c, func(a uint32) {
		if r < v.Occupancy(a) {
			n++
		}
	})

	return n
}

// Neighbors appends to dst every other member in slot's cell or a cell
// adjacent to it, the data-plane neighbor set, in slot order. It appends
// nothing if slot is not a member.
func Neighbors(v *View, slot Slot, dst []Slot) []Slot {
	c := v.Cell(slot)
	if c == topology.NoCell {
		return dst
	}

	start := len(dst)

	var buf [64]uint32

	for _, nc := range v.shape.AppendNeighborCells(buf[:0], c) {
		for _, s := range v.CellMembers(nc) {
			if s != slot {
				dst = append(dst, s)
			}
		}
	}

	slices.Sort(dst[start:])

	return dst
}
