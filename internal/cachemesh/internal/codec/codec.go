// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package codec holds the binary encoding primitives shared by the view and
// wire packages.
//
// The encoding is canonical: fixed-width integers are little-endian, variable
// integers are minimal unsigned LEB128 (the encoding/binary uvarint format),
// and byte strings are a uvarint length followed by the bytes. Readers reject
// non-minimal varints, so one value has exactly one encoding. That matters
// because view digests hash encoded deltas.
//
// Writers are plain append functions so callers control allocation. A Reader
// records the first error and returns zero values afterwards, so decoders
// can read a whole message and check Err once at the end.
package codec

import (
	"encoding/binary"
	"errors"
	"math"
)

// Errors returned by Reader.
var (
	// ErrShort means the input ended before a value was complete.
	ErrShort = errors.New("codec: input too short")
	// ErrVarint means a varint overflowed 64 bits or was not minimal.
	ErrVarint = errors.New("codec: invalid varint")
	// ErrTooLarge means a length or count exceeded its limit or the
	// remaining input.
	ErrTooLarge = errors.New("codec: length too large")
	// ErrTrailing means bytes remained after the last value.
	ErrTrailing = errors.New("codec: trailing bytes")
	// ErrInvalid means a decoded value is outside its domain.
	ErrInvalid = errors.New("codec: invalid value")
)

// AppendU8 appends one byte.
func AppendU8(b []byte, v uint8) []byte { return append(b, v) }

// AppendBool appends 1 for true and 0 for false.
func AppendBool(b []byte, v bool) []byte {
	if v {
		return append(b, 1)
	}

	return append(b, 0)
}

// AppendU16 appends v as 2 little-endian bytes.
func AppendU16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }

// AppendU32 appends v as 4 little-endian bytes.
func AppendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

// AppendU64 appends v as 8 little-endian bytes.
func AppendU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }

// AppendUvarint appends v as a minimal unsigned varint.
func AppendUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

// AppendVarint appends v as a zig-zag signed varint.
func AppendVarint(b []byte, v int64) []byte { return binary.AppendVarint(b, v) }

// AppendBytes appends a uvarint length followed by p.
func AppendBytes(b, p []byte) []byte {
	b = binary.AppendUvarint(b, uint64(len(p)))

	return append(b, p...)
}

// AppendString appends a uvarint length followed by the bytes of s.
func AppendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))

	return append(b, s...)
}

// UvarintLen returns the number of bytes AppendUvarint writes for v.
func UvarintLen(v uint64) int {
	n := 1

	for v >= 0x80 {
		v >>= 7
		n++
	}

	return n
}

// Reader decodes values from a byte slice. The zero value reads nothing and
// reports ErrShort on the first read.
type Reader struct {
	b   []byte
	off int
	err error
}

// NewReader returns a Reader over b. The Reader never modifies b.
func NewReader(b []byte) Reader { return Reader{b: b} }

// Err returns the first error encountered, or nil.
func (r *Reader) Err() error { return r.err }

// Fail records err as the Reader's error if no error was recorded yet. Decoders
// use it to report domain errors (for example ErrInvalid) through the same
// path as encoding errors.
func (r *Reader) Fail(err error) {
	if r.err == nil && err != nil {
		r.err = err
	}
}

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.b) - r.off }

// Offset returns the number of bytes consumed so far.
func (r *Reader) Offset() int { return r.off }

// Done returns the first error, or ErrTrailing if unread bytes remain.
func (r *Reader) Done() error {
	if r.err != nil {
		return r.err
	}

	if r.off != len(r.b) {
		return ErrTrailing
	}

	return nil
}

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}

	if n < 0 || n > len(r.b)-r.off {
		r.err = ErrShort
		return nil
	}

	p := r.b[r.off : r.off+n : r.off+n]
	r.off += n

	return p
}

// U8 reads one byte.
func (r *Reader) U8() uint8 {
	p := r.take(1)
	if p == nil {
		return 0
	}

	return p[0]
}

// Bool reads a byte that must be 0 or 1.
func (r *Reader) Bool() bool {
	v := r.U8()
	if v > 1 {
		r.Fail(ErrInvalid)
		return false
	}

	return v == 1
}

// U16 reads 2 little-endian bytes.
func (r *Reader) U16() uint16 {
	p := r.take(2)
	if p == nil {
		return 0
	}

	return binary.LittleEndian.Uint16(p)
}

// U32 reads 4 little-endian bytes.
func (r *Reader) U32() uint32 {
	p := r.take(4)
	if p == nil {
		return 0
	}

	return binary.LittleEndian.Uint32(p)
}

// U64 reads 8 little-endian bytes.
func (r *Reader) U64() uint64 {
	p := r.take(8)
	if p == nil {
		return 0
	}

	return binary.LittleEndian.Uint64(p)
}

// Uvarint reads a minimal unsigned varint.
func (r *Reader) Uvarint() uint64 {
	if r.err != nil {
		return 0
	}

	v, n := binary.Uvarint(r.b[r.off:])
	if n == 0 {
		r.err = ErrShort
		return 0
	}

	if n < 0 || (n > 1 && r.b[r.off+n-1] == 0) {
		r.err = ErrVarint
		return 0
	}

	r.off += n

	return v
}

// Varint reads a minimal zig-zag signed varint.
func (r *Reader) Varint() int64 {
	u := r.Uvarint()

	v := int64(u >> 1) //nolint:gosec // zig-zag decoding reinterprets the bits.
	if u&1 != 0 {
		v = ^v
	}

	return v
}

// Uvarint32 reads a uvarint that must fit in 32 bits.
func (r *Reader) Uvarint32() uint32 {
	v := r.Uvarint()
	if v > math.MaxUint32 {
		r.Fail(ErrInvalid)
		return 0
	}

	return uint32(v)
}

// Len reads a uvarint count of elements that each occupy at least minSize
// encoded bytes, and checks it against max and the remaining input, so a
// hostile count cannot make the caller allocate more than the input justifies.
// minSize below 1 is treated as 1.
func (r *Reader) Len(maxCount, minSize int) int {
	v := r.Uvarint()
	if r.err != nil {
		return 0
	}

	if minSize < 1 {
		minSize = 1
	}

	if v > uint64(maxCount) || v > uint64(r.Remaining()/minSize) { //nolint:gosec // maxCount and remaining are non-negative.
		r.err = ErrTooLarge
		return 0
	}

	return int(v) //nolint:gosec // bounded by maxCount above.
}

// Bytes reads a length-prefixed byte string of at most maxLen bytes. The
// result aliases the Reader's input; copy it if the input will be reused.
func (r *Reader) Bytes(maxLen int) []byte {
	n := r.Len(maxLen, 1)
	if r.err != nil {
		return nil
	}

	p := r.take(n)
	if p == nil {
		return nil
	}

	return p
}

// String reads a length-prefixed string of at most maxLen bytes. The result
// is a copy.
func (r *Reader) String(maxLen int) string {
	return string(r.Bytes(maxLen))
}

// Fixed reads exactly len(dst) bytes into dst.
func (r *Reader) Fixed(dst []byte) {
	p := r.take(len(dst))
	if p == nil {
		clear(dst)
		return
	}

	copy(dst, p)
}
