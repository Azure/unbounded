// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"slices"
)

// Choose returns the grid shape for n members.
//
// It targets cells = max(1, floor(n/cellSize)) cells and, for D = 1..MaxDims
// in order, factors them into D near-equal sizes with Factor. It returns the
// first (smallest D) shape whose EstimatedNeighbors(ceil(n/Cells())) is at
// most maxNeighbors. If no D satisfies the bound it returns the MaxDims shape,
// which has the fewest neighbors Choose can offer.
//
// cellSize < 1 is treated as 1 and n < 0 as 0.
func Choose(n, cellSize, maxNeighbors int) Shape {
	if cellSize < 1 {
		cellSize = 1
	}

	if n < 0 {
		n = 0
	}

	// Factor may round the product up to just under twice cells, so cap
	// cells at MaxCells/2 to keep every candidate shape valid.
	cells := max(1, n/cellSize)
	cells = min(cells, MaxGridCells/2)

	var (
		s   Shape
		buf [MaxDims]uint32
	)

	for d := 1; d <= MaxDims; d++ {
		s = mustShape(Factor(buf[:0], uint32(cells), d))

		occ := ceilDiv(n, int(s.cells))
		if s.EstimatedNeighbors(occ) <= maxNeighbors {
			return s
		}
	}

	return s
}

// Factor appends d sizes whose product is the smallest such product that is
// at least cells, with sizes differing by at most one: every size starts at
// r = floor(cells^(1/d)) and sizes are incremented to r+1 from dimension 0
// upward until the product reaches cells. Larger sizes therefore come first.
// Rounding the product up (never down) keeps average occupancy at or below
// the requested cell size while staying within a factor (r+1)/r of it.
func Factor(dst []uint32, cells uint32, d int) []uint32 {
	cells = max(cells, 1)
	r := intRoot(uint64(cells), d)

	start := len(dst)
	for range d {
		dst = append(dst, uint32(r))
	}

	prod := pow(r, d)
	for i := 0; i < d && prod < uint64(cells); i++ {
		prod = prod / r * (r + 1)
		dst[start+i]++
	}

	return dst
}

// intRoot returns floor(x^(1/d)) for x >= 1.
func intRoot(x uint64, d int) uint64 {
	lo, hi := uint64(1), x
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if pow(mid, d) <= x {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	return lo
}

// pow returns b^e, saturating at 2^62.
func pow(b uint64, e int) uint64 {
	const limit = 1 << 62

	p := uint64(1)
	for range e {
		if b != 0 && p > limit/b {
			return limit
		}

		p *= b
	}

	return p
}

func ceilDiv(a, b int) int {
	return (a + b - 1) / b
}

// NeedsReshape reports whether the leader must reshape the grid. The current
// shape violates a hard bound when
//
//   - EstimatedNeighbors(maxOcc) > maxNeighbors,
//   - average occupancy n/Cells() is below max(1, cellSize/2) with more than
//     one cell, or
//   - average occupancy is above 4*cellSize.
//
// Averages are compared with integer cross-multiplication. A violation only
// triggers a reshape when Choose would return a different shape; otherwise
// (for example when even the MaxDims shape exceeds maxNeighbors) reshaping
// would not help and would only churn every member. An invalid shape always
// needs a reshape.
func NeedsReshape(s Shape, n, maxOcc, cellSize, maxNeighbors int) bool {
	if s.Validate() != nil {
		return true
	}

	if cellSize < 1 {
		cellSize = 1
	}

	cells := int(s.cells)
	low := max(1, cellSize/2)
	violated := s.EstimatedNeighbors(maxOcc) > maxNeighbors ||
		(cells > 1 && n < low*cells) ||
		n > 4*cellSize*cells

	return violated && Choose(n, cellSize, maxNeighbors) != s
}

// JoinCell returns the cell for a joining member: the least occupied cell,
// with ties going to the cell that ranks best for the member, that is the
// largest Score(memberKey, cell) (Better semantics; distinct
// cells give distinct scores, so the result is unique). Spreading ties by
// score keeps a burst of joins from all landing in the lowest cell index. It
// returns NoCell for an empty occupancy slice.
func JoinCell(occupancy []uint32, memberKey uint64) uint32 {
	best := NoCell

	var bestOcc uint32

	var bestScore uint64

	for c, o := range occupancy {
		if best != NoCell && o > bestOcc {
			continue
		}

		sc := Score(memberKey, uint64(c))
		if best == NoCell || o < bestOcc || sc > bestScore {
			best, bestOcc, bestScore = uint32(c), o, sc
		}
	}

	return best
}

// ReshapeCells assigns cells for a new shape: members sorted by key
// ascending (ties by input index) get cells 0, 1, ..., Cells()-1, 0, ...
// round-robin. The result is indexed like keys.
func ReshapeCells(s Shape, keys []uint64) []uint32 {
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}

	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case keys[a] < keys[b]:
			return -1
		case keys[a] > keys[b]:
			return 1
		}

		return 0
	})

	out := make([]uint32, len(keys))

	cells := max(s.cells, 1)
	c := uint32(0)

	for _, i := range order {
		out[i] = c

		c++
		if c == cells {
			c = 0
		}
	}

	return out
}

// Candidate is a member that BalanceMoves may move.
type Candidate struct {
	Member  uint32 // opaque member identifier, usually a slot
	Key     uint64 // Member.Key
	Cell    uint32 // current cell
	Joining bool   // preferred for moves
}

// Move moves Member to cell To.
type Move struct {
	Member uint32
	To     uint32
}

// BalanceMoves plans up to maxMoves cell moves that bring the occupancy
// spread (max - min) toward at most one. Each step takes the fullest cell
// that still has an unmoved candidate (ties: lowest cell index) and the
// emptiest cell (ties: lowest cell index); it stops when their occupancies
// differ by at most one. From the source cell it moves the candidate that
// ranks first by: Joining before not Joining, then ascending Key, then input
// order. Each candidate moves at most once. occupancy is not modified.
// Candidates whose Cell is out of range are ignored.
//
// Cost is O(len(candidates) + len(occupancy) * (maxMoves + 1)) plus sorting
// the candidates of each source cell.
func BalanceMoves(occupancy []uint32, candidates []Candidate, maxMoves int) []Move {
	if len(occupancy) < 2 || maxMoves <= 0 {
		return nil
	}

	occ := slices.Clone(occupancy)

	// Bucket candidates by cell (counting sort, stable in input order);
	// each bucket is sorted only when its cell first becomes a source.
	next := make([]int32, len(occ)+1)

	for _, c := range candidates {
		if int(c.Cell) < len(occ) {
			next[c.Cell+1]++
		}
	}

	for c := range occ {
		next[c+1] += next[c]
	}

	end := slices.Clone(next[1:])
	order := make([]int32, next[len(occ)])
	fill := slices.Clone(next[:len(occ)])

	for i, c := range candidates {
		if int(c.Cell) < len(occ) {
			order[fill[c.Cell]] = int32(i)
			fill[c.Cell]++
		}
	}

	sorted := make([]bool, len(occ))

	var moves []Move

	for len(moves) < maxMoves {
		src, dst := -1, 0

		for c, o := range occ {
			if o < occ[dst] {
				dst = c
			}

			if next[c] < end[c] && (src < 0 || o > occ[src]) {
				src = c
			}
		}

		if src < 0 || occ[src] <= occ[dst]+1 {
			break
		}

		if !sorted[src] {
			sorted[src] = true
			sortCandidates(candidates, order[next[src]:end[src]])
		}

		cand := candidates[order[next[src]]]
		next[src]++
		occ[src]--
		occ[dst]++

		moves = append(moves, Move{Member: cand.Member, To: uint32(dst)})
	}

	return moves
}

// sortCandidates orders idx by Joining first, then ascending Key, then
// input index.
func sortCandidates(candidates []Candidate, idx []int32) {
	slices.SortFunc(idx, func(a, b int32) int {
		ca, cb := &candidates[a], &candidates[b]
		if ca.Joining != cb.Joining {
			if ca.Joining {
				return -1
			}

			return 1
		}

		if c := cmpU64(ca.Key, cb.Key); c != 0 {
			return c
		}

		return int(a) - int(b)
	})
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}
