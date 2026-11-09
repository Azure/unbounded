// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestNewShapeErrors(t *testing.T) {
	cases := [][]uint32{
		nil,
		{1, 2, 3, 4, 5, 6, 7, 8, 9},
		{3, 0},
		{1 << 16, 1 << 16},
	}
	for _, c := range cases {
		if _, err := NewShape(c...); !errors.Is(err, ErrInvalidShape) {
			t.Errorf("NewShape(%v) err = %v, want ErrInvalidShape", c, err)
		}
	}

	var zero Shape
	if zero.Validate() == nil {
		t.Error("zero shape must be invalid")
	}

	s, err := NewShape(3, 4, 5)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}

	if s.Cells() != 60 || s.Degree() != 9 || !slices.Equal(s.Sizes(), []uint32{3, 4, 5}) {
		t.Fatalf("shape = %+v", s)
	}

	bad := s
	bad.Size[0] = 2

	if bad.Validate() == nil {
		t.Error("tampered shape must be invalid")
	}

	s2, _ := NewShape(3, 4, 5)
	if s != s2 {
		t.Error("equal sizes must give equal shapes")
	}
}

func randShape(r *rand.Rand) Shape {
	d := r.IntN(MaxDims) + 1

	sz := make([]uint32, d)
	for i := range sz {
		sz[i] = uint32(r.IntN(9) + 1)
	}

	return mustShape(sz)
}

func TestCoordinates(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 300 {
		s := randShape(r)

		var buf [MaxDims]uint32

		for range 50 {
			c := uint32(r.IntN(int(s.Cells())))
			co := s.Coords(&buf, c)

			if got := s.CellOf(co); got != c {
				t.Fatalf("round trip %v: %d -> %v -> %d", s.Sizes(), c, co, got)
			}

			for i := range int(s.Dims) {
				if s.Coord(c, i) != co[i] {
					t.Fatalf("Coord mismatch")
				}

				x := uint32(r.IntN(int(s.Size[i])))
				rc := s.Replace(c, i, x)

				if s.Coord(rc, i) != x || !s.Adjacent(c, rc) || s.DiffMask(c, rc)&^(1<<i) != 0 {
					t.Fatalf("Replace %d dim %d -> %d", c, i, rc)
				}

				up := s.Step(c, i, 1)
				if s.Step(up, i, -1) != c || !s.Adjacent(c, up) {
					t.Fatalf("Step not inverse")
				}

				if s.Size[i] > 1 && up == c {
					t.Fatalf("Step did not move")
				}

				if want := (co[i] + 1) % s.Size[i]; s.Coord(up, i) != want {
					t.Fatalf("Step wrap: %d want %d", s.Coord(up, i), want)
				}
			}
		}

		if s.CellOf(make([]uint32, s.Dims+1)) != NoCell {
			t.Fatal("wrong length must give NoCell")
		}

		bad := make([]uint32, s.Dims)
		bad[0] = s.Size[0]

		if s.CellOf(bad) != NoCell {
			t.Fatal("out of range must give NoCell")
		}
	}
}

func TestFastDivExhaustiveEdges(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	divs := []uint32{1, 2, 3, 7, 1 << 15, 1<<30 - 1, 1 << 30, 1<<31 + 1, ^uint32(0)}

	for range 200 {
		divs = append(divs, r.Uint32()|1, uint32(r.IntN(1000)+1))
	}

	nums := []uint32{0, 1, ^uint32(0), ^uint32(0) - 1, 1 << 31}
	for range 200 {
		nums = append(nums, r.Uint32())
	}

	for _, d := range divs {
		m := magic(d)
		for _, a := range nums {
			if fastDiv(a, m) != a/d {
				t.Fatalf("fastDiv(%d,%d)=%d", a, d, fastDiv(a, m))
			}
		}
	}
}

func TestDiffMaskAndNextHop(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for range 300 {
		s := randShape(r)
		for range 50 {
			a := uint32(r.IntN(int(s.Cells())))
			b := uint32(r.IntN(int(s.Cells())))

			m := s.DiffMask(a, b)
			for i := range int(s.Dims) {
				if (m>>i&1 == 1) != (s.Coord(a, i) != s.Coord(b, i)) {
					t.Fatalf("DiffMask bit %d wrong", i)
				}
			}

			if (a == b) != (m == 0) {
				t.Fatal("DiffMask zero iff equal")
			}

			// Next-hop progression: fix the lowest differing dimension.
			cur, steps := a, 0
			for cur != b {
				mm := s.DiffMask(cur, b)
				i := FirstDiff(mm)
				nxt := s.Replace(cur, i, s.Coord(b, i))

				if !s.Adjacent(cur, nxt) || nxt == cur {
					t.Fatal("hop not adjacent")
				}

				cur = nxt
				steps++
			}

			if steps > int(s.Dims) || steps != popcount(m) {
				t.Fatalf("hops %d, dims %d, diff %d", steps, s.Dims, popcount(m))
			}

			// Walking the differing dimensions in order visits each once.
			var seen uint8
			for mm := m; mm != 0; mm &= mm - 1 {
				seen |= 1 << FirstDiff(mm)
			}

			if seen != m {
				t.Fatal("FirstDiff walk")
			}
		}
	}

	if FirstDiff(0) != -1 {
		t.Fatal("FirstDiff(0)")
	}
}

func popcount(m uint8) int {
	n := 0
	for ; m != 0; m &= m - 1 {
		n++
	}

	return n
}

func TestAppendNeighborCells(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10))
	for range 100 {
		s := randShape(r)
		c := uint32(r.IntN(int(s.Cells())))
		nb := s.AppendNeighborCells(nil, c)

		if len(nb) != 1+s.Degree() || nb[0] != c {
			t.Fatalf("neighbors %v of %d in %v", nb, c, s.Sizes())
		}

		seen := map[uint32]bool{}
		for _, x := range nb {
			if seen[x] || !s.Adjacent(c, x) {
				t.Fatalf("bad neighbor %d", x)
			}

			seen[x] = true
		}

		count := 0

		for x := range s.Cells() {
			if s.Adjacent(c, x) {
				count++
			}
		}

		if count != len(nb) {
			t.Fatalf("missing neighbors: %d vs %d", count, len(nb))
		}
	}
}

var gridSink uint32

func BenchmarkCoord(b *testing.B) {
	s := Choose(100_000, 4, 256)

	var x uint32
	for i := range b.N {
		x += s.Coord(uint32(i)%s.Cells(), i&3)
	}

	gridSink = x
}

func BenchmarkDiffMask(b *testing.B) {
	s := Choose(100_000, 4, 256)
	n := s.Cells()

	var x uint32
	for i := range b.N {
		x += uint32(s.DiffMask(uint32(i)%n, uint32(i*7919)%n))
	}

	gridSink = x
}

func BenchmarkNextHop(b *testing.B) {
	s := Choose(100_000, 4, 256)
	n := s.Cells()

	var x uint32

	for i := range b.N {
		a, t := uint32(i)%n, uint32(i*7919)%n
		m := s.DiffMask(a, t)

		if m&(m-1) != 0 {
			d := FirstDiff(m)
			x += s.Replace(a, d, s.Coord(t, d))
		}
	}

	gridSink = x
}

func BenchmarkStep(b *testing.B) {
	s := Choose(100_000, 4, 256)

	var x uint32
	for i := range b.N {
		x += s.Step(uint32(i)%s.Cells(), i&3, 1)
	}

	gridSink = x
}

func BenchmarkAppendNeighborCells(b *testing.B) {
	s := Choose(100_000, 4, 256)
	buf := make([]uint32, 0, 64)

	b.ReportAllocs()

	for i := range b.N {
		buf = s.AppendNeighborCells(buf[:0], uint32(i)%s.Cells())
	}

	gridSink = uint32(len(buf))
}

func FuzzFastDiv(f *testing.F) {
	f.Add(uint32(0), uint32(1))
	f.Add(^uint32(0), uint32(3))
	f.Add(^uint32(0), ^uint32(0))
	f.Fuzz(func(t *testing.T, a, d uint32) {
		if d == 0 {
			return
		}

		if got := fastDiv(a, magic(d)); got != a/d {
			t.Fatalf("fastDiv(%d, %d) = %d, want %d", a, d, got, a/d)
		}
	})
}
