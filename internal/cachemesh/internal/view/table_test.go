// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
)

func TestNewTable(t *testing.T) {
	for _, bits := range []uint8{1, 5, 10, 11, 20} {
		tb := NewTable(bits, 7)
		if tb.Len() != 1<<bits || tb.Bits() != bits {
			t.Fatalf("bits %d: len %d", bits, tb.Len())
		}

		n := 0

		tb.ForEach(func(p uint32, e Entry) {
			if p != uint32(n) || e != (Entry{Owner: 7, Prev: NoMember}) {
				t.Fatalf("entry %d = %+v", p, e)
			}

			n++
		})

		if n != tb.Len() {
			t.Fatalf("ForEach visited %d", n)
		}
	}

	if ValidPartitionBits(0) || ValidPartitionBits(25) || !ValidPartitionBits(24) {
		t.Fatal("ValidBits")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("NewTable(0) must panic")
		}
	}()

	NewTable(0, 0)
}

func TestTableAssign(t *testing.T) {
	a := NewTable(12, 1) // 4 chunks

	b, err := a.Assign([]uint32{5, 6, 2000}, []uint32{2, 1, 3}, 9)
	if err != nil {
		t.Fatal(err)
	}

	if got := b.Get(5); got != (Entry{Owner: 2, Prev: 1, Moved: 9}) {
		t.Fatalf("moved entry %+v", got)
	}

	if got := b.Get(6); got != (Entry{Owner: 1, Prev: NoMember}) {
		t.Fatalf("same-owner entry changed: %+v", got)
	}

	if b.Owner(2000) != 3 || a.Owner(5) != 1 || a.Owner(2000) != 1 {
		t.Fatal("original modified or assign lost")
	}

	if SameChunk(a, b, 0) || SameChunk(a, b, 1) || !SameChunk(a, b, 2) || !SameChunk(a, b, 3) {
		t.Fatal("chunk sharing wrong")
	}

	c, _ := b.Assign([]uint32{5}, []uint32{4}, 10)
	if got := c.Get(5); got != (Entry{Owner: 4, Prev: 2, Moved: 10}) {
		t.Fatalf("second move %+v", got)
	}

	if !SameChunk(b, c, 1) {
		t.Fatal("chunk 1 must be shared")
	}

	// No-op assign returns the same table.
	if d, _ := c.Assign([]uint32{5}, []uint32{4}, 11); d != c {
		t.Fatal("no-op assign must return the receiver")
	}

	if !EqualTables(a, NewTable(12, 1)) || EqualTables(a, b) || a.Hash() == b.Hash() || a.Hash() != NewTable(12, 1).Hash() {
		t.Fatal("Equal/Hash")
	}

	if EqualTables(a, NewTable(11, 1)) {
		t.Fatal("different bits must differ")
	}

	// Bootstrap: Prev stays NoMember.
	e, _ := NewTable(4, NoMember).Assign([]uint32{3}, []uint32{0}, 1)
	if e.Get(3) != (Entry{Owner: 0, Prev: NoMember, Moved: 1}) {
		t.Fatalf("bootstrap %+v", e.Get(3))
	}

	bad := []struct {
		parts, owners []uint32
	}{
		{[]uint32{1}, nil},
		{[]uint32{1 << 12}, []uint32{0}},
		{[]uint32{1}, []uint32{NoMember}},
		{[]uint32{1, 2, 1}, []uint32{3, 4, 5}},
	}
	for _, c := range bad {
		if _, err := a.Assign(c.parts, c.owners, 1); !errors.Is(err, ErrInvalidTable) {
			t.Errorf("Assign(%v,%v) err %v", c.parts, c.owners, err)
		}
	}
}

func TestLoads(t *testing.T) {
	tb := NewTable(4, NoMember)
	tb, _ = tb.Assign([]uint32{0, 1, 2}, []uint32{3, 3, 0}, 1)

	dst := make([]uint32, 2, 10)
	dst[1] = 99
	dst[:10][7] = 42 // stale capacity must be cleared

	got := tb.Loads(dst)
	if !slices.Equal(got, []uint32{1, 0, 0, 2}) {
		t.Fatalf("loads %v", got)
	}
}

var sink uint32

func BenchmarkGet(b *testing.B) {
	tb := NewTable(20, 3)

	var x uint32
	for i := range b.N {
		x += tb.Get(uint32(i) & (1<<20 - 1)).Owner
	}

	sink = x
}

func BenchmarkAssign(b *testing.B) {
	for _, k := range []int{1, 64, 4096} {
		b.Run(strconv.Itoa(k), func(b *testing.B) {
			r := rand.New(rand.NewPCG(1, 2))
			tb := NewTable(20, 0)
			parts := make([]uint32, k)
			owners := make([]uint32, k)

			seen := map[uint32]bool{}

			for i := range parts {
				p := r.Uint32N(1 << 20)
				for seen[p] {
					p = r.Uint32N(1 << 20)
				}

				seen[p] = true
				parts[i], owners[i] = p, 1
			}

			b.ReportAllocs()

			for range b.N {
				if _, err := tb.Assign(parts, owners, 1); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestNewTableFunc(t *testing.T) {
	tb := NewTableFunc(12, func(p uint32) Entry { return Entry{Owner: p % 3, Prev: NoMember, Moved: p} })
	for p := range uint32(1 << 12) {
		if e := tb.Get(p); e.Owner != p%3 || e.Prev != NoMember || e.Moved != p {
			t.Fatalf("entry %d = %+v", p, e)
		}
	}

	u := NewTableFunc(14, func(uint32) Entry { return Entry{Owner: 7, Prev: 2, Moved: 5} })
	for i := 1; i < u.NumChunks(); i++ {
		if u.chunks[i] != u.chunks[0] {
			t.Fatalf("chunk %d not shared", i)
		}
	}

	small := NewTableFunc(2, func(p uint32) Entry { return Entry{Owner: p, Prev: NoMember} })
	if small.Len() != 4 || small.Owner(3) != 3 {
		t.Fatalf("small table wrong")
	}

	if !EqualTables(NewTableFunc(10, func(uint32) Entry { return Entry{Owner: 1, Prev: NoMember} }), NewTable(10, 1)) {
		t.Fatal("NewTableFunc differs from NewTable")
	}
}
