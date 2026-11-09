// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func TestChoose(t *testing.T) {
	const cellSize, maxN = 4, 256

	for _, n := range []int{0, 1, 2, 3, 4, 5, 8, 100, 257, 1000, 10_000, 100_000, 1_000_000} {
		s := Choose(n, cellSize, maxN)
		if err := s.Validate(); err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}

		occ := ceilDiv(n, int(s.Cells()))
		est := s.EstimatedNeighbors(occ)

		if est > maxN && s.Dims != MaxDims {
			t.Errorf("n=%d: est %d > %d", n, est, maxN)
		}

		want := uint32(max(1, n/cellSize))
		if s.Cells() < want {
			t.Errorf("n=%d: cells %d < %d", n, s.Cells(), want)
		}

		for i := 1; i < int(s.Dims); i++ {
			if s.Size[i-1] < s.Size[i] || s.Size[i-1]-s.Size[i] > 1 {
				t.Errorf("n=%d: uneven sizes %v", n, s.Sizes())
			}
		}

		// Minimality: every smaller D violates the bound.
		for d := 1; d < int(s.Dims); d++ {
			sd := mustShape(Factor(nil, want, d))
			if sd.EstimatedNeighbors(ceilDiv(n, int(sd.Cells()))) <= maxN {
				t.Errorf("n=%d: D=%d also fits, Choose gave %d", n, d, s.Dims)
			}
		}

		if NeedsReshape(s, n, occ, cellSize, maxN) {
			t.Errorf("n=%d: fresh shape %v needs reshape", n, s.Sizes())
		}

		t.Logf("n=%d sizes=%v cells=%d neighbors=%d", n, s.Sizes(), s.Cells(), est)
	}

	s := Choose(100_000, 4, 256)
	if s.Dims != 4 {
		t.Fatalf("100k: D = %d, want 4", s.Dims)
	}

	if est := s.EstimatedNeighbors(4); est < 180 || est > 200 {
		t.Fatalf("100k: neighbors %d, want ~190", est)
	}

	if Choose(5, 4, 256).Dims != 1 {
		t.Fatal("small clusters must be D=1")
	}

	// Impossible bound falls back to MaxDims.
	if s := Choose(1_000_000, 4, 3); s.Dims != MaxDims {
		t.Fatalf("impossible bound: D = %d", s.Dims)
	}

	if NeedsReshape(Choose(1_000_000, 4, 3), 1_000_000, 4, 4, 3) {
		t.Fatal("reshape that cannot help must not be requested")
	}
}

func TestFactor(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		cells := uint32(r.IntN(1_000_000) + 1)
		d := r.IntN(MaxDims) + 1
		sz := Factor(nil, cells, d)

		prod := uint64(1)
		for _, x := range sz {
			prod *= uint64(x)
		}

		if prod < uint64(cells) {
			t.Fatalf("Factor(%d,%d)=%v product too small", cells, d, sz)
		}

		if sz[0]-sz[d-1] > 1 {
			t.Fatalf("Factor(%d,%d)=%v uneven", cells, d, sz)
		}

		// Smallest: dropping the last increment falls below cells.
		if sz[0] != sz[d-1] || sz[0] > 1 {
			last := 0

			for i := range sz {
				if sz[i] == sz[0] {
					last = i
				}
			}

			if sz[0] != sz[d-1] && prod/uint64(sz[last])*uint64(sz[last]-1) >= uint64(cells) {
				t.Fatalf("Factor(%d,%d)=%v not smallest", cells, d, sz)
			}
		}
	}
}

func TestNeedsReshape(t *testing.T) {
	s, _ := NewShape(5, 5) // 25 cells, degree 8
	cases := []struct {
		n, maxOcc int
		want      bool
	}{
		{100, 4, false},
		{100, 40, true}, // neighbor bound
		{40, 2, true},   // average below 2
		{401, 17, true}, // average above 16 (and neighbors)
		{50, 2, false},  // exactly at the low bound
		{1, 1, true},    // tiny cluster on a big grid
		{100, 5, false}, // 5*9-1=44 fine
		{100, 29, true}, // 29*9-1=260 > 256
	}

	for _, c := range cases {
		if got := NeedsReshape(s, c.n, c.maxOcc, 4, 256); got != c.want {
			t.Errorf("NeedsReshape(n=%d,maxOcc=%d)=%v want %v", c.n, c.maxOcc, got, c.want)
		}
	}

	if !NeedsReshape(Shape{}, 1, 1, 4, 256) {
		t.Error("invalid shape needs reshape")
	}
}

func TestJoinCell(t *testing.T) {
	if JoinCell(nil, 1) != NoCell {
		t.Fatal("empty")
	}

	occ := []uint32{3, 1, 2, 1, 1}

	c := JoinCell(occ, 42)
	if occ[c] != 1 {
		t.Fatalf("not least occupied: %d", c)
	}

	best := NoCell
	for _, x := range []uint32{1, 3, 4} {
		if best == NoCell || Better(Score(42, uint64(x)), 42, Score(42, uint64(best)), 42) {
			best = x
		}
	}

	if c != best {
		t.Fatalf("tie break: %d want %d", c, best)
	}

	// Equal occupancy: different keys spread over cells.
	flat := make([]uint32, 64)
	hit := map[uint32]bool{}

	for k := range uint64(64) {
		hit[JoinCell(flat, Mix64(k))] = true
	}

	if len(hit) < 20 {
		t.Fatalf("ties do not spread: %d cells", len(hit))
	}
}

func TestReshapeCells(t *testing.T) {
	s, _ := NewShape(3)
	keys := []uint64{50, 10, 40, 20, 30}

	got := ReshapeCells(s, keys)
	// Sorted: 10(1) 20(3) 30(4) 40(2) 50(0) -> cells 0 1 2 0 1.
	want := []uint32{1, 0, 0, 1, 2}

	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}

	if len(ReshapeCells(s, nil)) != 0 {
		t.Fatal("empty")
	}
}

func TestBalanceMoves(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	for iter := range 200 {
		ncells := r.IntN(50) + 2

		var cands []Candidate

		occ := make([]uint32, ncells)
		n := r.IntN(400)

		for i := range n {
			c := uint32(r.IntN(ncells))
			if r.IntN(3) == 0 {
				c = 0 // skew
			}

			occ[c]++
			cands = append(cands, Candidate{Member: uint32(i), Key: r.Uint64(), Cell: c, Joining: r.IntN(4) == 0})
		}

		before := slices.Clone(occ)
		moves := BalanceMoves(occ, cands, 1<<20)

		if !slices.Equal(before, occ) {
			t.Fatal("occupancy modified")
		}

		after := slices.Clone(occ)
		moved := map[uint32]bool{}

		for _, m := range moves {
			if moved[m.Member] {
				t.Fatal("member moved twice")
			}

			moved[m.Member] = true
			after[cands[m.Member].Cell]--
			after[m.To]++
		}

		if spread(after) > 1 {
			t.Fatalf("iter %d: spread %d after %d moves", iter, spread(after), len(moves))
		}

		// Shuffling candidates must not change the result.
		sh := slices.Clone(cands)
		r.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })

		if !slices.Equal(moves, BalanceMoves(occ, sh, 1<<20)) && !hasDupKeys(cands) {
			t.Fatal("result depends on candidate order")
		}

		// Capped moves are a prefix.
		k := len(moves) / 2
		if !slices.Equal(moves[:k], BalanceMoves(occ, cands, k)) {
			t.Fatal("capped moves not a prefix")
		}
	}

	// Joining preferred.
	occ := []uint32{3, 0}
	cands := []Candidate{{Member: 1, Key: 1, Cell: 0}, {Member: 2, Key: 9, Cell: 0, Joining: true}, {Member: 3, Key: 2, Cell: 0}}

	m := BalanceMoves(occ, cands, 1)
	if len(m) != 1 || m[0] != (Move{Member: 2, To: 1}) {
		t.Fatalf("joining not preferred: %v", m)
	}

	if BalanceMoves([]uint32{5}, cands, 10) != nil || BalanceMoves(occ, cands, 0) != nil {
		t.Fatal("degenerate")
	}

	// No candidates in the full cell: nothing to do.
	if len(BalanceMoves([]uint32{5, 0}, []Candidate{{Cell: 1}, {Cell: 7}}, 4)) != 0 {
		t.Fatal("moved without candidates")
	}
}

func hasDupKeys(c []Candidate) bool {
	seen := map[uint64]bool{}
	for _, x := range c {
		if seen[x.Key] {
			return true
		}

		seen[x.Key] = true
	}

	return false
}

func spread(o []uint32) uint32 {
	return slices.Max(o) - slices.Min(o)
}

func BenchmarkJoinCell(b *testing.B) {
	occ := make([]uint32, 25_000)
	for i := range occ {
		occ[i] = 4
	}

	b.ReportAllocs()

	for i := range b.N {
		gridSink += JoinCell(occ, uint64(i))
	}
}

func BenchmarkBalanceMoves(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 1))
	occ := make([]uint32, 25_000)

	cands := make([]Candidate, 100_000)
	for i := range cands {
		c := uint32(r.IntN(len(occ)))
		occ[c]++
		cands[i] = Candidate{Member: uint32(i), Key: r.Uint64(), Cell: c}
	}

	b.ReportAllocs()

	for range b.N {
		gridSink += uint32(len(BalanceMoves(occ, cands, 32)))
	}
}

// TestChooseHuge: Factor rounds the cell count up, which used to push the
// product past MaxCells and panic in mustShape for very large n.
func TestChooseHuge(t *testing.T) {
	for _, n := range []int{MaxGridCells, MaxGridCells + 12345, 1 << 31, 1<<31 - 1, 1 << 40} {
		for _, maxN := range []int{1, 16, 256} {
			s := Choose(n, 1, maxN)
			if err := s.Validate(); err != nil {
				t.Fatalf("Choose(%d, 1, %d): %v", n, maxN, err)
			}
		}
	}
}
