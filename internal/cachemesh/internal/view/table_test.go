// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"bytes"
	"errors"
	"math"
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

// The golden values pin the hash formats. If one of these changes, members
// built from different commits disagree on placement, so the change must be
// treated as a wire format change.
func TestGolden(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"KeyHash", KeyHash(1, []byte("sha256:abc")), KeyHash(1, []byte("sha256:abc"))},
		{"KeyHashString", KeyHashString(1, "sha256:abc"), KeyHash(1, []byte("sha256:abc"))},
		{"Mix64(0)", Mix64(0), 0},
		{"Mix64(1)", Mix64(1), 0x5692161d100b05e5},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

func TestSeedDistinct(t *testing.T) {
	if Seed("blobs") == Seed("meta") {
		t.Fatal("distinct keyspaces must have distinct seeds")
	}

	a, b := Seed("blobs"), Seed("blobs")
	if a != b {
		t.Fatal("Seed must be deterministic")
	}
}

func TestMemberKey(t *testing.T) {
	a := MemberKey("node-a", 1)
	if a != MemberKey("node-a", 1) {
		t.Fatal("MemberKey must be deterministic")
	}

	if a == MemberKey("node-a", 2) {
		t.Fatal("incarnation must change the key")
	}

	if a == MemberKey("node-b", 1) {
		t.Fatal("id must change the key")
	}
	// The separator prevents ("ab", x) colliding with ("a", "b"+x).
	if MemberKey("ab", 0) == MemberKey("a", 0) {
		t.Fatal("unexpected collision")
	}
}

func TestPartition(t *testing.T) {
	cases := []struct {
		h    uint64
		bits uint8
		want uint32
	}{
		{0, 20, 0},
		{^uint64(0), 20, 1<<20 - 1},
		{1 << 63, 1, 1},
		{1 << 62, 1, 0},
		{^uint64(0), 32, ^uint32(0)},
		{0x8000_0000_0000_0000, 20, 1 << 19},
	}
	for _, c := range cases {
		if got := Partition(c.h, c.bits); got != c.want {
			t.Errorf("Partition(%#x, %d) = %#x, want %#x", c.h, c.bits, got, c.want)
		}
	}
}

func TestMix64Bijective(t *testing.T) {
	seen := make(map[uint64]struct{}, 1<<16)

	for i := range uint64(1 << 16) {
		v := Mix64(i)
		if _, dup := seen[v]; dup {
			t.Fatalf("collision at %d", i)
		}

		seen[v] = struct{}{}
	}
}

func TestBetter(t *testing.T) {
	if !Better(2, 0, 1, 9) {
		t.Fatal("higher score must win")
	}

	if !Better(1, 9, 1, 8) {
		t.Fatal("tie must go to larger key")
	}

	if Better(1, 8, 1, 8) {
		t.Fatal("equal is not better")
	}
}

func TestBelow(t *testing.T) {
	if !Below(^uint64(0), ^uint64(0)) {
		t.Fatal("p=1 must always pass")
	}

	if Below(5, 5) || !Below(4, 5) {
		t.Fatal("strict comparison expected")
	}

	if Below(0, 0) {
		t.Fatal("p=0 must never pass")
	}
}

// TestScoreUniform checks that rendezvous winners are spread evenly.
func TestScoreUniform(t *testing.T) {
	const (
		members = 16
		items   = 1 << 16
	)

	keys := make([]uint64, members)
	for i := range keys {
		keys[i] = MemberKey("m", uint64(i+1))
	}

	wins := make([]int, members)

	for it := range uint64(items) {
		best := 0

		for i := 1; i < members; i++ {
			if Better(Score(keys[i], it), keys[i], Score(keys[best], it), keys[best]) {
				best = i
			}
		}

		wins[best]++
	}

	mean := items / members
	for i, w := range wins {
		if w < mean*9/10 || w > mean*11/10 {
			t.Errorf("member %d won %d, mean %d", i, w, mean)
		}
	}
}

func BenchmarkKeyHash(b *testing.B) {
	key := []byte("sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

	b.ReportAllocs()

	for b.Loop() {
		_ = KeyHash(7, key)
	}
}

func BenchmarkScore(b *testing.B) {
	var x uint64

	b.ReportAllocs()

	for i := uint64(0); b.Loop(); i++ {
		x += Score(i, 42)
	}

	_ = x
}

func TestRoundTrip(t *testing.T) {
	var b []byte

	b = AppendU8(b, 7)
	b = AppendBool(b, true)
	b = AppendBool(b, false)
	b = AppendU32(b, 0xdeadbeef)
	b = AppendU64(b, math.MaxUint64-1)
	b = AppendUvarint(b, 300)
	b = AppendBytes(b, []byte("abc"))
	b = AppendString(b, "")
	b = AppendString(b, "hello")
	b = append(b, 1, 2, 3, 4)

	r := NewReader(b)
	if got := r.U8(); got != 7 {
		t.Fatalf("U8 = %d", got)
	}

	if !r.Bool() || r.Bool() {
		t.Fatal("Bool mismatch")
	}

	if got := r.U32(); got != 0xdeadbeef {
		t.Fatalf("U32 = %x", got)
	}

	if got := r.U64(); got != math.MaxUint64-1 {
		t.Fatalf("U64 = %x", got)
	}

	if got := r.Uvarint(); got != 300 {
		t.Fatalf("Uvarint = %d", got)
	}

	if got := r.Bytes(10); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("Bytes = %q", got)
	}

	if got := r.String(10); got != "" {
		t.Fatalf("String = %q", got)
	}

	if got := r.String(10); got != "hello" {
		t.Fatalf("String = %q", got)
	}

	var fixed [4]byte

	r.Fixed(fixed[:])

	if fixed != [4]byte{1, 2, 3, 4} {
		t.Fatalf("Fixed = %v", fixed)
	}

	if err := r.Done(); err != nil {
		t.Fatalf("Done = %v", err)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		read func(r *Reader)
		want error
	}{
		{"short u32", []byte{1, 2}, func(r *Reader) { r.U32() }, ErrShort},
		{"short u64", nil, func(r *Reader) { r.U64() }, ErrShort},
		{"short varint", []byte{0x80}, func(r *Reader) { r.Uvarint() }, ErrShort},
		{"non-minimal varint", []byte{0x80, 0x00}, func(r *Reader) { r.Uvarint() }, ErrVarint},
		{"overflow varint", bytes.Repeat([]byte{0xff}, 11), func(r *Reader) { r.Uvarint() }, ErrVarint},
		{"bad bool", []byte{2}, func(r *Reader) { r.Bool() }, ErrInvalidValue},
		{"uvarint32 overflow", AppendUvarint(nil, math.MaxUint32+1), func(r *Reader) { r.Uvarint32() }, ErrInvalidValue},
		{"bytes over max", AppendBytes(nil, []byte("abcdef")), func(r *Reader) { r.Bytes(3) }, ErrTooLarge},
		{"bytes over input", AppendUvarint(nil, 100), func(r *Reader) { r.Bytes(1000) }, ErrTooLarge},
		{"len over input", append(AppendUvarint(nil, 3), 0, 0, 0, 0, 0), func(r *Reader) { r.Len(10, 2) }, ErrTooLarge},
		{"trailing", []byte{1, 2}, func(r *Reader) { r.U8() }, ErrTrailing},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(tc.in)
			tc.read(&r)

			if err := r.Done(); !errors.Is(err, tc.want) {
				t.Fatalf("Done = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStickyError(t *testing.T) {
	r := NewReader([]byte{1})
	r.U32()

	if got := r.U8(); got != 0 {
		t.Fatalf("read after error = %d, want 0", got)
	}

	r.Fail(ErrInvalidValue)

	if !errors.Is(r.Err(), ErrShort) {
		t.Fatalf("Err = %v, want first error", r.Err())
	}

	var fixed [2]byte

	fixed[0] = 9
	r.Fixed(fixed[:])

	if fixed != [2]byte{} {
		t.Fatalf("Fixed after error = %v", fixed)
	}
}

func TestLenAllowsExactFit(t *testing.T) {
	b := append(AppendUvarint(nil, 2), 0, 0, 0, 0)

	r := NewReader(b)
	if n := r.Len(2, 2); n != 2 || r.Err() != nil {
		t.Fatalf("Len = %d, %v", n, r.Err())
	}

	if r.Remaining() != 4 || r.Offset() != 1 {
		t.Fatalf("Remaining = %d, Offset = %d", r.Remaining(), r.Offset())
	}
}

func TestBytesAliasesInput(t *testing.T) {
	b := AppendBytes(nil, []byte("xy"))
	r := NewReader(b)
	p := r.Bytes(2)

	if cap(p) != len(p) {
		t.Fatalf("cap = %d, want %d so appends cannot clobber the input", cap(p), len(p))
	}
}

func FuzzReader(f *testing.F) {
	f.Add([]byte{0x80, 0x01, 3, 'a', 'b', 'c'})
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(in)
		v := r.Uvarint()
		s := r.Bytes(1 << 10)

		if r.Err() != nil {
			return
		}

		out := AppendBytes(AppendUvarint(nil, v), s)
		if !bytes.Equal(out, in[:r.Offset()]) {
			t.Fatalf("re-encoding %x gave %x", in[:r.Offset()], out)
		}
	})
}
