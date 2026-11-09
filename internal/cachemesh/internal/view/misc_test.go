// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view_test

import (
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/view"
	"github.com/Azure/unbounded/internal/cachemesh/internal/view/viewtest"
)

// chain returns views v[0..n] and deltas d[i] from v[i] to v[i+1].
func chain(t *testing.T, n int) ([]*view.View, []*view.Delta) {
	t.Helper()

	vs := []*view.View{boot(t, 2)}

	var ds []*view.Delta

	for i := range n {
		var ops []view.Op
		if i == n/2 {
			ops = append(ops, view.OpNewTerm(vs[i].Term()+1, 1))
		}

		ops = append(ops, view.OpEpoch(vs[i].Epoch()+1, time.Second))
		d := view.NewDelta(vs[i], ops...)

		nv, err := view.Apply(vs[i], d)
		if err != nil {
			t.Fatal(err)
		}

		vs, ds = append(vs, nv), append(ds, d)
	}

	return vs, ds
}

func TestDeltaLog(t *testing.T) {
	vs, ds := chain(t, 10)

	l := view.NewDeltaLog(4)
	if got, ok := l.Since(view.Genesis().Version(), view.GenesisDigest(), 0); !ok || got != nil {
		t.Fatal("empty log at genesis should be up to date")
	}

	if _, ok := l.Since(vs[0].Version(), vs[0].Digest(), 0); ok {
		t.Fatal("empty log served an unknown version")
	}

	if err := l.Append(ds[0], nil); !errors.Is(err, view.ErrBase) {
		t.Fatalf("append on wrong head: %v", err)
	}

	l.Reset(vs[0])

	for i, d := range ds {
		if err := l.Append(d, nil); err != nil {
			t.Fatal(err)
		}

		if h, dg := l.Head(); h != vs[i+1].Version() || dg != vs[i+1].Digest() {
			t.Fatal("head")
		}
	}

	if l.Len() != 4 {
		t.Fatalf("len %d", l.Len())
	}

	// Exact head.
	if got, ok := l.Since(vs[10].Version(), vs[10].Digest(), 0); !ok || len(got) != 0 {
		t.Fatal("head should be up to date")
	}

	// Head version, wrong digest.
	if _, ok := l.Since(vs[10].Version(), vs[9].Digest(), 0); ok {
		t.Fatal("wrong head digest accepted")
	}

	// Oldest servable base is vs[6] (window of 4).
	got, ok := l.Since(vs[6].Version(), vs[6].Digest(), 0)
	if !ok || len(got) != 4 || got[0].Delta != ds[6] || got[3].Delta != ds[9] {
		t.Fatalf("since 6: %v %d", ok, len(got))
	}

	for _, e := range got {
		dd, err := view.DecodeDelta(e.Encoded)
		if err != nil || dd.Digest != e.Delta.Digest {
			t.Fatal("cached encoding")
		}
	}

	if lim, ok := l.Since(vs[7].Version(), vs[7].Digest(), 2); !ok || len(lim) != 2 || lim[1].Delta != ds[8] {
		t.Fatal("since with limit")
	}

	if _, ok := l.Since(vs[5].Version(), vs[5].Digest(), 0); ok {
		t.Fatal("older than window served")
	}

	if _, ok := l.Since(vs[7].Version(), vs[6].Digest(), 0); ok {
		t.Fatal("wrong digest served")
	}

	// Replaying the returned deltas reaches the head.
	v := vs[6]

	for _, e := range got {
		var err error
		if v, err = view.Apply(v, e.Delta); err != nil {
			t.Fatal(err)
		}
	}

	if !view.Equal(v, vs[10]) {
		t.Fatal("replay")
	}

	l.Reset(vs[10])

	if l.Len() != 0 {
		t.Fatal("reset")
	}

	if _, ok := l.Since(vs[9].Version(), vs[9].Digest(), 0); ok {
		t.Fatal("reset log served old deltas")
	}

	if view.NewDeltaLog(0) == nil {
		t.Fatal("default size")
	}
}

func TestInterner(t *testing.T) {
	vs, ds := chain(t, 6)
	in := view.NewInterner(3)

	a, err := in.Apply(vs[0], ds[0])
	if err != nil {
		t.Fatal(err)
	}

	// An equal but distinct base view shares the result.
	base2, err := view.DecodeSnapshot(view.EncodeSnapshot(vs[0]))
	if err != nil {
		t.Fatal(err)
	}

	b, err := in.Apply(base2, ds[0])
	if err != nil || a != b || !view.Equal(a, vs[1]) {
		t.Fatal("apply not shared")
	}

	if in.Hits != 1 || in.Misses != 1 {
		t.Fatalf("hits %d misses %d", in.Hits, in.Misses)
	}

	if in.View(vs[1]) != a {
		t.Fatal("view not canonicalized")
	}

	enc := view.EncodeDelta(ds[1])

	d1, err := in.Delta(enc)
	if err != nil {
		t.Fatal(err)
	}

	d2, _ := in.Delta(slices.Clone(enc))
	if d1 != d2 {
		t.Fatal("delta not shared")
	}

	if _, err := in.Delta([]byte{9}); err == nil {
		t.Fatal("bad delta")
	}

	if _, err := in.Apply(vs[1], ds[0]); !errors.Is(err, view.ErrBase) {
		t.Fatalf("wrong base: %v", err)
	}

	// Eviction keeps the tables bounded and results correct.
	v := vs[0]
	for _, d := range ds {
		if v, err = in.Apply(v, d); err != nil {
			t.Fatal(err)
		}
	}

	if !view.Equal(v, vs[len(vs)-1]) {
		t.Fatal("interned chain")
	}

	if view.NewInterner(0) == nil {
		t.Fatal("default")
	}
}

func checkMonitoring(t *testing.T, v *view.View) {
	t.Helper()

	observers := map[uint32][]uint32{}

	var subj, obs, nb []uint32

	v.ForEachMember(func(s uint32, m *view.Member) bool {
		subj = view.Subjects(v, s, subj[:0])
		if len(subj) != len(dedup(subj)) || slices.Contains(subj, s) {
			t.Fatalf("slot %d: subjects %v", s, subj)
		}

		nb = view.Neighbors(v, s, nb[:0])
		if !slices.IsSorted(nb) || slices.Contains(nb, s) || len(dedup(nb)) != len(nb) {
			t.Fatalf("slot %d: neighbors %v", s, nb)
		}

		for _, x := range subj {
			if _, ok := slices.BinarySearch(nb, x); !ok {
				t.Fatalf("subject %d of %d is not a neighbor", x, s)
			}

			observers[x] = append(observers[x], s)
		}

		// Brute-force neighbors.
		want := 0

		v.ForEachMember(func(x uint32, o *view.Member) bool {
			if x != s && v.Shape().Adjacent(m.Cell, o.Cell) {
				want++
			}

			return true
		})

		if want != len(nb) {
			t.Fatalf("slot %d: %d neighbors, want %d", s, len(nb), want)
		}

		return true
	})

	v.ForEachMember(func(s uint32, _ *view.Member) bool {
		obs = view.Observers(v, s, obs[:0])
		got, want := dedup(obs), dedup(observers[s])

		if len(got) != len(obs) || !slices.Equal(got, want) {
			t.Fatalf("slot %d: observers %v, inverse of subjects %v", s, got, want)
		}

		cells := map[uint32]bool{}
		for _, o := range obs {
			cells[v.Cell(o)] = true
		}

		if view.ObserverCells(v, s) != len(cells) {
			t.Fatalf("slot %d: observer cells %d, want %d", s, view.ObserverCells(v, s), len(cells))
		}

		return true
	})
}

func dedup(s []uint32) []uint32 {
	c := slices.Clone(s)
	slices.Sort(c)

	return slices.Compact(c)
}

func TestMonitoring(t *testing.T) {
	cases := []viewtest.Options{
		{PartitionBits: 6},
		{PartitionBits: 6, CellSize: 2, MaxNeighbors: 20},
		{PartitionBits: 6, CellSize: 4, MaxNeighbors: 40},
		{PartitionBits: 6, CellSize: 3, MaxNeighbors: 12},
	}

	for _, n := range []int{1, 2, 7, 64, 500} {
		for _, o := range cases {
			checkMonitoring(t, viewtest.Build(t, n, o))
		}
	}

	// Uneven occupancy, sizes 2 and 3, and empty cells.
	r := rand.New(rand.NewPCG(1, 2))

	for range 20 {
		v := viewtest.Build(t, 30, viewtest.Options{PartitionBits: 6})
		sh, _ := view.NewShape(2, 3, 1+r.Uint32N(3))

		cells := make([]uint32, v.SlotCap())
		for i := range cells {
			cells[i] = r.Uint32N(sh.Cells())
		}

		checkMonitoring(t, mustApply(t, v, view.OpReshape(sh, cells)))
	}

	if view.Subjects(view.Genesis(), 0, nil) != nil || view.Observers(view.Genesis(), 0, nil) != nil ||
		view.Neighbors(view.Genesis(), 0, nil) != nil || view.ObserverCells(view.Genesis(), 0) != 0 {
		t.Fatal("non-member")
	}
}

func TestMonitoringCounts(t *testing.T) {
	// A balanced grid with every cell at occupancy 4: each member has
	// exactly 3 + 2D observers in 1 + 2D cells (all sizes > 2).
	v := viewtest.Build(t, 4*4*5*6, viewtest.Options{PartitionBits: 6})
	sh, _ := view.NewShape(4, 5, 6)

	keys := make([]uint64, v.SlotCap())
	for i := range keys {
		keys[i] = v.Key(uint32(i))
	}

	v = mustApply(t, v, view.OpReshape(sh, view.ReshapeCells(sh, keys)))
	d := int(sh.Dims)

	var obs []uint32

	v.ForEachMember(func(s uint32, _ *view.Member) bool {
		obs = view.Observers(v, s, obs[:0])
		if len(obs) != 3+2*d || view.ObserverCells(v, s) != 1+2*d {
			t.Fatalf("slot %d: %d observers in %d cells, D=%d", s, len(obs), view.ObserverCells(v, s), d)
		}

		return true
	})
}
