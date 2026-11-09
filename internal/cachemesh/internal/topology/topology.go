// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package topology is the arithmetic of the cachemesh grid.
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
package topology

import (
	"errors"
	"fmt"
	"math/bits"
)

// MaxDims is the largest number of grid dimensions.
const MaxDims = 8

// NoCell marks the absence of a cell.
const NoCell = ^uint32(0)

// MaxCells bounds the number of cells in a shape: it keeps cell arithmetic
// within uint32. Views apply the stricter view.MaxCells (no more cells than
// slots), so this bound only matters to Shape and Choose on their own.
const MaxCells = 1 << 30

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
var ErrInvalidShape = errors.New("topology: invalid shape")

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
		if prod > MaxCells {
			return Shape{}, fmt.Errorf("%w: more than %d cells", ErrInvalidShape, MaxCells)
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
