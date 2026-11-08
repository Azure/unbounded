// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
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
		if best == NoCell || hashing.Better(hashing.Score(42, uint64(x)), 42, hashing.Score(42, uint64(best)), 42) {
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
		hit[JoinCell(flat, hashing.Mix64(k))] = true
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

var sink uint32

func BenchmarkCoord(b *testing.B) {
	s := Choose(100_000, 4, 256)

	var x uint32
	for i := range b.N {
		x += s.Coord(uint32(i)%s.Cells(), i&3)
	}

	sink = x
}

func BenchmarkDiffMask(b *testing.B) {
	s := Choose(100_000, 4, 256)
	n := s.Cells()

	var x uint32
	for i := range b.N {
		x += uint32(s.DiffMask(uint32(i)%n, uint32(i*7919)%n))
	}

	sink = x
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

	sink = x
}

func BenchmarkStep(b *testing.B) {
	s := Choose(100_000, 4, 256)

	var x uint32
	for i := range b.N {
		x += s.Step(uint32(i)%s.Cells(), i&3, 1)
	}

	sink = x
}

func BenchmarkAppendNeighborCells(b *testing.B) {
	s := Choose(100_000, 4, 256)
	buf := make([]uint32, 0, 64)

	b.ReportAllocs()

	for i := range b.N {
		buf = s.AppendNeighborCells(buf[:0], uint32(i)%s.Cells())
	}

	sink = uint32(len(buf))
}

func BenchmarkJoinCell(b *testing.B) {
	occ := make([]uint32, 25_000)
	for i := range occ {
		occ[i] = 4
	}

	b.ReportAllocs()

	for i := range b.N {
		sink += JoinCell(occ, uint64(i))
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
		sink += uint32(len(BalanceMoves(occ, cands, 32)))
	}
}

// TestChooseHuge: Factor rounds the cell count up, which used to push the
// product past MaxCells and panic in mustShape for very large n.
func TestChooseHuge(t *testing.T) {
	for _, n := range []int{MaxCells, MaxCells + 12345, 1 << 31, 1<<31 - 1, 1 << 40} {
		for _, maxN := range []int{1, 16, 256} {
			s := Choose(n, 1, maxN)
			if err := s.Validate(); err != nil {
				t.Fatalf("Choose(%d, 1, %d): %v", n, maxN, err)
			}
		}
	}
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
