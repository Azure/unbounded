// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Grid arithmetic.
//
// The leader places every member in one cell of a D-dimensional grid with
// dimension sizes Size[0..D-1] (mixed radix, dimension 0 least significant,
// so cell = sum(coord[i] * stride[i]) with stride[0] = 1). Two cells are
// adjacent when their coordinates differ in exactly one dimension. A member's
// neighbors are the other members of its own cell and of every adjacent cell,
// so a member keeps about occ * (1 + sum(Size[i] - 1)) - 1 connections and
// reaches any other member in at most D hops by fixing one differing
// coordinate per hop, lowest dimension first.
//
// Everything here is a pure function of plain values and slices: no clocks,
// no randomness, no maps, no floating point. Shape is a comparable value type
// that carries precomputed strides and Lemire fast-division constants so the
// routing hot path (Coord, Replace, Step, DiffMask, Adjacent) uses only
// multiplications and never a hardware division.
//
// The leader-side helpers (Choose, NeedsReshape, JoinCell, ReshapeCells,
// BalanceMoves) are deterministic: identical inputs give identical outputs on
// every member and architecture.
package view

import (
	"errors"
	"fmt"
	"math/bits"
	"slices"
)

// MaxDims is the largest number of grid dimensions.
const MaxDims = 8

// NoCell marks the absence of a cell.
const NoCell = ^uint32(0)

// MaxGridCells bounds the number of cells in a shape: it keeps cell arithmetic
// within uint32. Views apply the stricter view.MaxGridCells (no more cells than
// slots), so this bound only matters to Shape and Choose on their own.
const MaxGridCells = 1 << 30

// Shape is a grid shape. The zero value is invalid; build shapes with
// NewShape or Choose. Shapes are comparable with ==: two shapes with the same
// sizes are equal because every other field is derived from the sizes.
type Shape struct {
	// Dims is the number of dimensions, 1..MaxDims.
	Dims uint8
	// Size holds the dimension sizes; entries at and beyond Dims are zero.
	Size [MaxDims]uint32

	cells  uint32
	stride [MaxDims]uint32
	// mStride and mSize are Lemire fast-division constants
	// (2^64 / d rounded up) for stride and size; zero when the divisor is 1.
	mStride [MaxDims]uint64
	mSize   [MaxDims]uint64
}

// ErrInvalidShape reports a shape that violates the size bounds.
var ErrInvalidShape = errors.New("view: invalid grid shape")

// NewShape builds a shape from its dimension sizes.
func NewShape(sizes ...uint32) (Shape, error) {
	var s Shape

	if len(sizes) == 0 || len(sizes) > MaxDims {
		return s, fmt.Errorf("%w: %d dimensions, want 1..%d", ErrInvalidShape, len(sizes), MaxDims)
	}

	prod := uint64(1)

	for i, sz := range sizes {
		if sz == 0 {
			return Shape{}, fmt.Errorf("%w: dimension %d has size 0", ErrInvalidShape, i)
		}

		prod *= uint64(sz)
		if prod > MaxGridCells {
			return Shape{}, fmt.Errorf("%w: more than %d cells", ErrInvalidShape, MaxGridCells)
		}
	}

	s.Dims = uint8(len(sizes))

	stride := uint32(1)

	for i, sz := range sizes {
		s.Size[i] = sz
		s.stride[i] = stride
		s.mStride[i] = magic(stride)
		s.mSize[i] = magic(sz)
		stride *= sz
	}

	s.cells = stride

	return s, nil
}

func mustShape(sizes []uint32) Shape {
	s, err := NewShape(sizes...)
	if err != nil {
		panic(err)
	}

	return s
}

// magic returns the Lemire constant ceil(2^64 / d), or 0 for d == 1.
func magic(d uint32) uint64 {
	if d <= 1 {
		return 0
	}

	return ^uint64(0)/uint64(d) + 1
}

// fastDiv returns a / d given m = magic(d). It is exact for every uint32 a.
func fastDiv(a uint32, m uint64) uint32 {
	if m == 0 {
		return a
	}

	hi, _ := bits.Mul64(m, uint64(a))

	return uint32(hi)
}

// Validate reports whether s is a well-formed shape built by NewShape.
func (s Shape) Validate() error {
	if s.Dims == 0 || s.Dims > MaxDims {
		return fmt.Errorf("%w: %d dimensions", ErrInvalidShape, s.Dims)
	}

	for i := int(s.Dims); i < MaxDims; i++ {
		if s.Size[i] != 0 {
			return fmt.Errorf("%w: size set beyond Dims", ErrInvalidShape)
		}
	}

	want, err := NewShape(s.Sizes()...)
	if err != nil {
		return err
	}

	if want != s {
		return fmt.Errorf("%w: derived fields do not match sizes", ErrInvalidShape)
	}

	return nil
}

// Sizes returns a copy of the dimension sizes.
func (s Shape) Sizes() []uint32 {
	out := make([]uint32, s.Dims)
	copy(out, s.Size[:s.Dims])

	return out
}

// Cells returns the number of cells, the product of the sizes.
func (s Shape) Cells() uint32 { return s.cells }

// Degree returns sum(Size[i] - 1), the number of cells adjacent to any cell.
func (s Shape) Degree() int {
	d := 0
	for i := range int(s.Dims) {
		d += int(s.Size[i]) - 1
	}

	return d
}

// EstimatedNeighbors returns occ * (1 + Degree()) - 1, the neighbor count of
// a member when every cell holds occ members. It returns -1 for occ <= 0.
func (s Shape) EstimatedNeighbors(occ int) int {
	if occ <= 0 {
		return -1
	}

	return occ*(1+s.Degree()) - 1
}

// Coord returns the coordinate of cell in dimension dim.
func (s *Shape) Coord(cell uint32, dim int) uint32 {
	q := fastDiv(cell, s.mStride[dim])

	return q - fastDiv(q, s.mSize[dim])*s.Size[dim]
}

// Coords writes the coordinates of cell into dst[:Dims] and returns it.
func (s *Shape) Coords(dst *[MaxDims]uint32, cell uint32) []uint32 {
	for i := range int(s.Dims) {
		q := fastDiv(cell, s.mSize[i])
		dst[i] = cell - q*s.Size[i]
		cell = q
	}

	return dst[:s.Dims]
}

// CellOf returns the cell with the given coordinates, or NoCell when the
// number of coordinates or any coordinate is out of range.
func (s *Shape) CellOf(coords []uint32) uint32 {
	if len(coords) != int(s.Dims) {
		return NoCell
	}

	var c uint32

	for i, x := range coords {
		if x >= s.Size[i] {
			return NoCell
		}

		c += x * s.stride[i]
	}

	return c
}

// Stride returns the cell-number distance between neighbors in dimension
// dim: the product of the sizes of the lower dimensions.
func (s *Shape) Stride(dim int) uint32 { return s.stride[dim] }

// Replace returns cell with its coordinate in dim set to coord.
func (s *Shape) Replace(cell uint32, dim int, coord uint32) uint32 {
	return cell - s.Coord(cell, dim)*s.stride[dim] + coord*s.stride[dim]
}

// Step moves cell by one in dim, wrapping. dir > 0 steps up, otherwise down.
func (s *Shape) Step(cell uint32, dim, dir int) uint32 {
	c := s.Coord(cell, dim)
	n := c

	if dir > 0 {
		n++
		if n == s.Size[dim] {
			n = 0
		}
	} else {
		if n == 0 {
			n = s.Size[dim]
		}

		n--
	}

	return cell - c*s.stride[dim] + n*s.stride[dim]
}

// DiffMask returns a bit mask with bit i set when a and b differ in
// dimension i.
func (s *Shape) DiffMask(a, b uint32) uint8 {
	var m uint8

	for i := range int(s.Dims) {
		qa := fastDiv(a, s.mSize[i])
		qb := fastDiv(b, s.mSize[i])

		if a-qa*s.Size[i] != b-qb*s.Size[i] {
			m |= 1 << i
		}

		a, b = qa, qb
		if a == b {
			break
		}
	}

	return m
}

// FirstDiff returns the lowest dimension set in mask, or -1 if mask is zero.
// Walk differing dimensions in increasing order with
// for m := mask; m != 0; m &= m - 1 { dim := FirstDiff(m) }.
func FirstDiff(mask uint8) int {
	if mask == 0 {
		return -1
	}

	return bits.TrailingZeros8(mask)
}

// Adjacent reports whether a and b are the same cell or differ in exactly
// one dimension, that is whether members of a and b are neighbors.
func (s *Shape) Adjacent(a, b uint32) bool {
	m := s.DiffMask(a, b)

	return m&(m-1) == 0
}

// AppendNeighborCells appends cell itself and then every adjacent cell, in
// increasing dimension and, within a dimension, increasing coordinate order.
// It appends exactly 1 + Degree() cells.
func (s *Shape) AppendNeighborCells(dst []uint32, cell uint32) []uint32 {
	dst = append(dst, cell)

	for i := range int(s.Dims) {
		c := s.Coord(cell, i)
		base := cell - c*s.stride[i]

		for x := range s.Size[i] {
			if x != c {
				dst = append(dst, base+x*s.stride[i])
			}
		}
	}

	return dst
}

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
