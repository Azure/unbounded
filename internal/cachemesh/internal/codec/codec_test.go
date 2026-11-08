// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package codec

import (
	"bytes"
	"errors"
	"math"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var b []byte

	b = AppendU8(b, 7)
	b = AppendBool(b, true)
	b = AppendBool(b, false)
	b = AppendU16(b, 0xbeef)
	b = AppendU32(b, 0xdeadbeef)
	b = AppendU64(b, math.MaxUint64-1)
	b = AppendUvarint(b, 300)
	b = AppendVarint(b, -12345)
	b = AppendVarint(b, math.MinInt64)
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

	if got := r.U16(); got != 0xbeef {
		t.Fatalf("U16 = %x", got)
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

	if got := r.Varint(); got != -12345 {
		t.Fatalf("Varint = %d", got)
	}

	if got := r.Varint(); got != math.MinInt64 {
		t.Fatalf("Varint = %d", got)
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

func TestUvarintLen(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, math.MaxUint32, math.MaxUint64} {
		if got, want := UvarintLen(v), len(AppendUvarint(nil, v)); got != want {
			t.Fatalf("UvarintLen(%d) = %d, want %d", v, got, want)
		}
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
		{"bad bool", []byte{2}, func(r *Reader) { r.Bool() }, ErrInvalid},
		{"uvarint32 overflow", AppendUvarint(nil, math.MaxUint32+1), func(r *Reader) { r.Uvarint32() }, ErrInvalid},
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

	r.Fail(ErrInvalid)

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
