// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"
)

// Encoding format versions, the first byte of an encoded delta or snapshot.
const (
	DeltaFormat    = 1
	SnapshotFormat = 1
)

// ErrFormat reports an unknown encoding format byte.
var ErrFormat = errors.New("view: unknown encoding format")

// appendSlot32 encodes a slot or cell with NoSlot (NoCell) as 0, so the
// common sentinel takes one byte.
func appendSlot32(b []byte, s uint32) []byte { return AppendUvarint(b, uint64(s+1)) }

func readSlot32(r *Reader) uint32 { return r.Uvarint32() - 1 }

func deltaSizeHint(d *Delta) int {
	n := 48

	for i := range d.Ops {
		op := &d.Ops[i]

		switch op.Kind {
		case KindAssign:
			n += 2 + 8*len(op.Parts)
		case KindReshape:
			n += 48 + 3*len(op.Cells)
		case KindAddMember:
			n += 64 + len(op.Member.ID) + len(op.Member.Addr) + len(op.Member.Zone)
		default:
			n += 24
		}
	}

	return n
}

// AppendDelta appends the canonical encoding of d, including its digest.
func AppendDelta(b []byte, d *Delta) []byte {
	b = appendDeltaBody(b, d)

	return append(b, d.Digest[:]...)
}

// EncodeDelta returns the canonical encoding of d.
func EncodeDelta(d *Delta) []byte { return AppendDelta(make([]byte, 0, deltaSizeHint(d)+32), d) }

func appendDeltaBody(b []byte, d *Delta) []byte {
	b = append(b, DeltaFormat)
	b = AppendVersion(b, d.Base)
	b = AppendVersion(b, d.Version)
	b = AppendUvarint(b, uint64(len(d.Ops)))

	for i := range d.Ops {
		b = appendOp(b, &d.Ops[i])
	}

	return b
}

func appendOp(b []byte, op *Op) []byte {
	b = append(b, byte(op.Kind))

	switch op.Kind {
	case KindNewTerm:
		b = AppendUvarint(b, op.Term)
		b = appendSlot32(b, op.Slot)
	case KindConfig:
		b = append(b, op.Config.PartitionBits)
		b = appendSlot32(b, op.Config.InitialOwner)
		b = AppendUvarint(b, uint64(op.Config.SlotReuseEpochs))
		b = AppendUvarint(b, uint64(op.Config.HandoffEpochs))
	case KindAddMember:
		m := &op.Member
		b = appendSlot32(b, op.Slot)
		b = AppendString(b, m.ID)
		b = AppendUvarint(b, m.Incarnation)
		b = AppendU64(b, m.Key)
		b = AppendString(b, m.Addr)
		b = AppendString(b, m.Zone)
		b = append(b, byte(m.State))
		b = AppendUvarint(b, uint64(m.Weight))
		b = appendSlot32(b, m.Cell)
		b = AppendUvarint(b, m.Joined)
		b = AppendUvarint(b, m.Since)
	case KindRemoveMember:
		b = appendSlot32(b, op.Slot)
	case KindSetState:
		b = appendSlot32(b, op.Slot)
		b = append(b, byte(op.State))
		b = AppendUvarint(b, uint64(op.Weight))
	case KindSetCell:
		b = appendSlot32(b, op.Slot)
		b = appendSlot32(b, op.Cell)
	case KindReshape:
		b = appendShape(b, &op.Shape)

		b = AppendUvarint(b, uint64(len(op.Cells)))
		for _, c := range op.Cells {
			b = appendSlot32(b, c)
		}
	case KindAssign:
		b = AppendUvarint(b, uint64(len(op.Parts)))
		for _, p := range op.Parts {
			b = AppendUvarint(b, uint64(p))
		}

		// A length mismatch is encoded faithfully so Apply rejects it.
		b = AppendUvarint(b, uint64(len(op.Owners)))
		for _, o := range op.Owners {
			b = appendSlot32(b, o)
		}
	case KindEpoch:
		b = AppendUvarint(b, op.Epoch)
		b = AppendUvarint(b, uint64(op.Period)) //nolint:gosec // negative periods round-trip and are rejected by Apply.
	case KindInvalidate:
		b = AppendU64(b, op.Seed)
		b = AppendUvarint(b, uint64(op.Gen))
	}

	return b
}

func appendShape(b []byte, s *Shape) []byte {
	b = append(b, s.Dims)
	for i := range int(min(s.Dims, MaxDims)) {
		b = AppendUvarint(b, uint64(s.Size[i]))
	}

	return b
}

func readShape(r *Reader) Shape {
	d := r.U8()
	if r.Err() != nil {
		return Shape{}
	}

	if d == 0 || d > MaxDims {
		r.Fail(ErrInvalidValue)
		return Shape{}
	}

	var sizes [MaxDims]uint32
	for i := range int(d) {
		sizes[i] = r.Uvarint32()
	}

	if r.Err() != nil {
		return Shape{}
	}

	s, err := NewShape(sizes[:d]...)
	if err != nil {
		r.Fail(ErrInvalidValue)
		return Shape{}
	}

	return s
}

// DecodeDelta decodes a delta encoded by AppendDelta. It bounds every count
// by the input size and the package limits, and rejects non-canonical input.
// It does not check that the delta applies; Apply does.
func DecodeDelta(b []byte) (*Delta, error) {
	r := NewReader(b)
	if f := r.U8(); r.Err() == nil && f != DeltaFormat {
		return nil, fmt.Errorf("%w: delta format %d", ErrFormat, f)
	}

	d := &Delta{}
	d.Base = ReadVersion(&r)
	d.Version = ReadVersion(&r)

	n := r.Len(MaxDeltaOps, 2)
	if n > 0 {
		d.Ops = make([]Op, n)
	}

	for i := range d.Ops {
		readOp(&r, &d.Ops[i])

		if r.Err() != nil {
			break
		}
	}

	r.Fixed(d.Digest[:])

	if err := r.Done(); err != nil {
		return nil, fmt.Errorf("view: decode delta: %w", err)
	}

	return d, nil
}

func readOp(r *Reader, op *Op) {
	op.Kind = OpKind(r.U8())

	switch op.Kind {
	case KindNewTerm:
		op.Term = r.Uvarint()
		op.Slot = readSlot32(r)
	case KindConfig:
		op.Config.PartitionBits = r.U8()
		op.Config.InitialOwner = readSlot32(r)
		op.Config.SlotReuseEpochs = r.Uvarint32()
		op.Config.HandoffEpochs = r.Uvarint32()
	case KindAddMember:
		m := &op.Member
		op.Slot = readSlot32(r)
		m.ID = r.String(MaxIDLen)
		m.Incarnation = r.Uvarint()
		m.Key = r.U64()
		m.Addr = r.String(MaxAddrLen)
		m.Zone = r.String(MaxZoneLen)
		m.State = MemberState(r.U8())
		m.Weight = r.Uvarint32()
		m.Cell = readSlot32(r)
		m.Joined = r.Uvarint()
		m.Since = r.Uvarint()
	case KindRemoveMember:
		op.Slot = readSlot32(r)
	case KindSetState:
		op.Slot = readSlot32(r)
		op.State = MemberState(r.U8())
		op.Weight = r.Uvarint32()
	case KindSetCell:
		op.Slot = readSlot32(r)
		op.Cell = readSlot32(r)
	case KindReshape:
		op.Shape = readShape(r)

		n := r.Len(MaxSlots, 1)
		if n > 0 {
			op.Cells = make([]uint32, n)
			for i := range op.Cells {
				op.Cells[i] = readSlot32(r)
			}
		}
	case KindAssign:
		n := r.Len(1<<MaxPartitionBits, 1)
		if n > 0 {
			op.Parts = make([]uint32, n)
			for i := range op.Parts {
				op.Parts[i] = r.Uvarint32()
			}
		}

		n = r.Len(1<<MaxPartitionBits, 1)
		if n > 0 {
			op.Owners = make([]uint32, n)
			for i := range op.Owners {
				op.Owners[i] = readSlot32(r)
			}
		}
	case KindEpoch:
		op.Epoch = r.Uvarint()

		p := r.Uvarint()
		if p > math.MaxInt64 {
			r.Fail(ErrInvalidValue)
		}

		op.Period = time.Duration(p) //nolint:gosec // checked above.
	case KindInvalidate:
		op.Seed = r.U64()
		op.Gen = r.Uvarint32()
	default:
		r.Fail(fmt.Errorf("%w: op kind %d", ErrInvalidValue, op.Kind))
	}
}

// AppendSnapshot appends the canonical encoding of v. The encoding stores
// every member and the table (bit-packed columns), not the derived indexes.
func AppendSnapshot(b []byte, v *View) []byte {
	b = append(b, SnapshotFormat)
	b = AppendVersion(b, v.version)
	b = append(b, v.digest[:]...)
	b = appendSlot32(b, v.leader)
	b = AppendUvarint(b, v.epoch)
	b = AppendUvarint(b, uint64(v.period)) //nolint:gosec // periods are never negative in a view.

	b = AppendUvarint(b, uint64(len(v.gens)))
	for _, g := range v.gens {
		b = AppendU64(b, g.Seed)
		b = AppendUvarint(b, uint64(g.Gen))
	}

	b = AppendBool(b, v.hasConfig)
	if !v.hasConfig {
		return b
	}

	c := v.config
	b = append(b, c.PartitionBits)
	b = appendSlot32(b, c.InitialOwner)
	b = AppendUvarint(b, uint64(c.SlotReuseEpochs))
	b = AppendUvarint(b, uint64(c.HandoffEpochs))
	b = appendShape(b, &v.shape)
	b = AppendUvarint(b, uint64(v.slotCap))

	// Zones in order of first appearance.
	var zones []string

	zoneIdx := map[string]uint64{}

	v.ForEachMember(func(_ Slot, m *Member) bool {
		if _, ok := zoneIdx[m.Zone]; !ok {
			zoneIdx[m.Zone] = uint64(len(zones))
			zones = append(zones, m.Zone)
		}

		return true
	})

	b = AppendUvarint(b, uint64(len(zones)))
	for _, z := range zones {
		b = AppendString(b, z)
	}

	b = AppendUvarint(b, uint64(v.n))
	next := Slot(0)

	v.ForEachMember(func(s Slot, m *Member) bool {
		b = AppendUvarint(b, uint64(s-next))
		next = s + 1
		b = AppendString(b, m.ID)
		b = AppendUvarint(b, m.Incarnation)
		b = AppendString(b, m.Addr)
		b = AppendUvarint(b, zoneIdx[m.Zone])
		b = append(b, byte(m.State))
		b = AppendUvarint(b, uint64(m.Weight))
		b = AppendUvarint(b, uint64(m.Cell))
		b = AppendUvarint(b, m.Joined)
		b = AppendUvarint(b, m.Since-m.Joined)

		return true
	})

	for s := range v.slotCap {
		c := v.mem[s>>memShift]
		if c.mem[s&memMask].State == 0 {
			b = AppendUvarint(b, c.reuse[s&memMask])
		}
	}

	t := v.table
	b = appendColumn(b, t, 0, 1)
	b = appendColumn(b, t, 1, 1)
	b = appendColumn(b, t, 2, 0)

	return b
}

// EncodeSnapshot returns the canonical snapshot encoding of v.
func EncodeSnapshot(v *View) []byte { return AppendSnapshot(nil, v) }

// tableColumn returns column col (0 owner, 1 prev, 2 moved) of chunk ci.
func tableColumn(t *Table, ci, col int) []uint32 {
	owner, prev, moved := t.Columns(ci)

	switch col {
	case 0:
		return owner
	case 1:
		return prev
	}

	return moved
}

// appendColumn writes table column col (see tableColumn), each value plus
// add, as uvarint base, width byte, and a length-prefixed LSB-first
// bit-packed array of value-base.
func appendColumn(b []byte, t *Table, col int, add uint32) []byte {
	lo, hi := uint32(math.MaxUint32), uint32(0)

	for ci := range t.NumChunks() {
		for _, x := range tableColumn(t, ci, col) {
			x += add
			lo, hi = min(lo, x), max(hi, x)
		}
	}

	w := bits.Len32(hi - lo)
	b = AppendUvarint(b, uint64(lo))
	b = append(b, byte(w))

	nbytes := (t.Len()*w + 7) / 8
	b = AppendUvarint(b, uint64(nbytes))

	if w == 0 {
		return b
	}

	var (
		acc  uint64
		have int
	)

	for ci := range t.NumChunks() {
		for _, x := range tableColumn(t, ci, col) {
			acc |= uint64(x+add-lo) << have

			have += w
			for have >= 8 {
				b = append(b, byte(acc))
				acc >>= 8
				have -= 8
			}
		}
	}

	if have > 0 {
		b = append(b, byte(acc))
	}

	return b
}

type column struct {
	lo   uint32
	w    int
	data []byte
	pos  int
	acc  uint64
	have int
}

func readColumn(r *Reader, n int) column {
	var c column

	c.lo = r.Uvarint32()
	c.w = int(r.U8())

	if c.w > 32 {
		r.Fail(ErrInvalidValue)
		return c
	}

	c.data = r.Bytes(math.MaxInt32)
	if r.Err() == nil && len(c.data) != (n*c.w+7)/8 {
		r.Fail(ErrInvalidValue)
	}

	// Unused high bits of the last byte must be zero (canonical form).
	if r.Err() == nil && c.w > 0 {
		if extra := len(c.data)*8 - n*c.w; extra > 0 && c.data[len(c.data)-1]>>(8-extra) != 0 {
			r.Fail(ErrInvalidValue)
		}
	}

	return c
}

func (c *column) next() uint32 {
	if c.w == 0 {
		return c.lo
	}

	for c.have < c.w {
		c.acc |= uint64(c.data[c.pos]) << c.have
		c.pos++
		c.have += 8
	}

	x := uint32(c.acc & (1<<c.w - 1))
	c.acc >>= c.w
	c.have -= c.w

	return c.lo + x
}

// DecodeSnapshot decodes a snapshot encoded by AppendSnapshot, rebuilds the
// derived indexes, and checks the result with Validate. Allocation is
// bounded by the input size times a constant and the package limits.
func DecodeSnapshot(b []byte) (*View, error) {
	v, err := decodeSnapshot(b)
	if err != nil {
		return nil, fmt.Errorf("view: decode snapshot: %w", err)
	}

	return v, nil
}

func decodeSnapshot(b []byte) (*View, error) {
	r := NewReader(b)
	if f := r.U8(); r.Err() == nil && f != SnapshotFormat {
		return nil, fmt.Errorf("%w: snapshot format %d", ErrFormat, f)
	}

	v := &View{}
	v.version = ReadVersion(&r)
	r.Fixed(v.digest[:])
	v.leader = readSlot32(&r)
	v.epoch = r.Uvarint()

	p := r.Uvarint()
	if p > math.MaxInt64 {
		r.Fail(ErrInvalidValue)
	}

	v.period = time.Duration(p) //nolint:gosec // checked above.

	if n := r.Len(MaxGens, 9); n > 0 {
		v.gens = make([]KeyspaceGen, n)
		for i := range v.gens {
			v.gens[i] = KeyspaceGen{Seed: r.U64(), Gen: r.Uvarint32()}
		}
	}

	v.hasConfig = r.Bool()
	if r.Err() != nil || !v.hasConfig {
		if err := r.Done(); err != nil {
			return nil, err
		}

		return v, Validate(v)
	}

	c := &v.config
	c.PartitionBits = r.U8()
	c.InitialOwner = readSlot32(&r)
	c.SlotReuseEpochs = r.Uvarint32()
	c.HandoffEpochs = r.Uvarint32()
	v.shape = readShape(&r)

	if r.Err() == nil && (!ValidPartitionBits(c.PartitionBits) || v.shape.Cells() > MaxCells) {
		r.Fail(ErrInvalidValue)
	}

	slotCap := r.Uvarint()
	if r.Err() == nil && slotCap > MaxSlots {
		r.Fail(ErrTooLarge)
	}

	zones := make([]string, r.Len(MaxSlots, 1))
	for i := range zones {
		zones[i] = r.String(MaxZoneLen)
	}

	// Each member takes at least 10 bytes and each free slot 1.
	n := r.Len(int(slotCap), 10) //nolint:gosec // bounded by MaxSlots.
	if r.Err() != nil {
		return nil, r.Err()
	}

	if slotCap-uint64(n) > uint64(r.Remaining()) {
		return nil, ErrTooLarge
	}

	v.slotCap = uint32(slotCap)
	v.n = n
	nchunks := (int(slotCap) + memChunk - 1) / memChunk
	v.mem = make([]*memberChunk, nchunks)
	v.minReuse = make([]uint64, nchunks)

	for i := range v.mem {
		v.mem[i] = &memberChunk{}
	}

	if err := readMembers(&r, v, zones); err != nil {
		return nil, err
	}

	for s := range v.slotCap {
		ch := v.mem[s>>memShift]
		if ch.mem[s&memMask].State == 0 {
			ch.reuse[s&memMask] = r.Uvarint()
		}
	}

	for ci := range v.mem {
		v.minReuse[ci] = chunkMinReuse(v, uint32(ci))
	}

	if err := readTable(&r, v); err != nil {
		return nil, err
	}

	if err := r.Done(); err != nil {
		return nil, err
	}

	v.cells = buildCells(v)
	if v.n > 0 {
		v.ids = buildIDs(v, idSizeFor(v.n))
	}

	if err := Validate(v); err != nil {
		return nil, err
	}

	return v, nil
}

func readMembers(r *Reader, v *View, zones []string) error {
	next := uint64(0)
	usedZones := uint64(0)

	for range v.n {
		s := next + r.Uvarint()
		if r.Err() == nil && s >= uint64(v.slotCap) {
			r.Fail(ErrInvalidValue)
		}

		next = s + 1

		var m Member

		m.ID = r.String(MaxIDLen)
		m.Incarnation = r.Uvarint()
		m.Addr = r.String(MaxAddrLen)

		z := r.Uvarint()
		if r.Err() == nil {
			// Zones must be listed in order of first appearance.
			switch {
			case z < usedZones:
			case z == usedZones && z < uint64(len(zones)):
				usedZones++
			default:
				r.Fail(ErrInvalidValue)
			}
		}

		m.State = MemberState(r.U8())
		m.Weight = r.Uvarint32()
		m.Cell = r.Uvarint32()
		m.Joined = r.Uvarint()

		since := r.Uvarint()
		if r.Err() != nil {
			return r.Err()
		}

		m.Since = m.Joined + since
		if m.Since < m.Joined || !m.State.Valid() || m.Cell >= v.shape.Cells() {
			return ErrInvalidValue
		}

		m.Zone = zones[z]
		m.Key = MemberKey(m.ID, m.Incarnation)
		v.mem[s>>memShift].mem[s&memMask] = m
	}

	if usedZones != uint64(len(zones)) {
		return ErrInvalidValue
	}

	return nil
}

func readTable(r *Reader, v *View) error {
	n := 1 << v.config.PartitionBits
	cols := [3]column{readColumn(r, n), readColumn(r, n), readColumn(r, n)}

	if r.Err() != nil {
		return r.Err()
	}

	var lo, hi [3]uint32
	for i := range lo {
		lo[i] = math.MaxUint32
	}

	bad := false
	v.table = NewTableFunc(v.config.PartitionBits, func(uint32) Entry {
		var x [3]uint32
		for i := range cols {
			x[i] = cols[i].next()
			lo[i], hi[i] = min(lo[i], x[i]), max(hi[i], x[i])
		}

		e := Entry{Owner: x[0] - 1, Prev: x[1] - 1, Moved: x[2]}
		if e.Owner != NoSlot && e.Owner >= MaxSlots || e.Prev != NoSlot && e.Prev >= MaxSlots {
			bad = true
		}

		return e
	})

	if bad {
		return ErrInvalidValue
	}

	// Canonical columns use the minimum as base and the minimal width.
	for i := range cols {
		if lo[i] != cols[i].lo || bits.Len32(hi[i]-lo[i]) != cols[i].w {
			return ErrInvalidValue
		}
	}

	loads := v.table.Loads(nil)
	for s, l := range loads {
		if l == 0 {
			continue
		}

		ci := s >> loadShift
		for len(v.loads) <= ci {
			v.loads = append(v.loads, nil)
		}

		if v.loads[ci] == nil {
			v.loads[ci] = make([]uint32, loadChunk)
		}

		v.loads[ci][s&loadMask] = l

		if !v.IsMember(Slot(s)) {
			v.orphans += uint64(l)
		}
	}

	return nil
}

// AppendVersion appends the encoding of v: Term then Seq, as uvarints.
func AppendVersion(b []byte, v Version) []byte {
	b = AppendUvarint(b, v.Term)

	return AppendUvarint(b, v.Seq)
}

// ReadVersion reads a Version written by AppendVersion.
func ReadVersion(r *Reader) Version {
	t := r.Uvarint()

	return Version{Term: t, Seq: r.Uvarint()}
}

// Binary encoding primitives, shared by the view codecs and the control
// package's wire messages.
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

// Errors returned by Reader.
var (
	// ErrShort means the input ended before a value was complete.
	ErrShort = errors.New("view: input too short")
	// ErrVarint means a varint overflowed 64 bits or was not minimal.
	ErrVarint = errors.New("view: invalid varint")
	// ErrTooLarge means a length or count exceeded its limit or the
	// remaining input.
	ErrTooLarge = errors.New("view: length too large")
	// ErrTrailing means bytes remained after the last value.
	ErrTrailing = errors.New("view: trailing bytes")
	// ErrInvalidValue means a decoded value is outside its domain.
	ErrInvalidValue = errors.New("view: invalid value")
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

// AppendU32 appends v as 4 little-endian bytes.
func AppendU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

// AppendU64 appends v as 8 little-endian bytes.
func AppendU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }

// AppendUvarint appends v as a minimal unsigned varint.
func AppendUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

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
		r.Fail(ErrInvalidValue)
		return false
	}

	return v == 1
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

// Uvarint32 reads a uvarint that must fit in 32 bits.
func (r *Reader) Uvarint32() uint32 {
	v := r.Uvarint()
	if v > math.MaxUint32 {
		r.Fail(ErrInvalidValue)
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
