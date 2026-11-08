// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package view

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/Azure/unbounded/internal/cachemesh/internal/codec"
	"github.com/Azure/unbounded/internal/cachemesh/internal/hashing"
	"github.com/Azure/unbounded/internal/cachemesh/internal/placement"
	"github.com/Azure/unbounded/internal/cachemesh/internal/topology"
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
func appendSlot32(b []byte, s uint32) []byte { return codec.AppendUvarint(b, uint64(s+1)) }

func readSlot32(r *codec.Reader) uint32 { return r.Uvarint32() - 1 }

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
	b = codec.AppendUvarint(b, d.Base.Term)
	b = codec.AppendUvarint(b, d.Base.Seq)
	b = codec.AppendUvarint(b, d.Version.Term)
	b = codec.AppendUvarint(b, d.Version.Seq)
	b = codec.AppendUvarint(b, uint64(len(d.Ops)))

	for i := range d.Ops {
		b = appendOp(b, &d.Ops[i])
	}

	return b
}

func appendOp(b []byte, op *Op) []byte {
	b = append(b, byte(op.Kind))

	switch op.Kind {
	case KindNewTerm:
		b = codec.AppendUvarint(b, op.Term)
		b = appendSlot32(b, op.Slot)
	case KindConfig:
		b = append(b, op.Config.PartitionBits)
		b = appendSlot32(b, op.Config.InitialOwner)
		b = codec.AppendUvarint(b, uint64(op.Config.SlotReuseEpochs))
		b = codec.AppendUvarint(b, uint64(op.Config.HandoffEpochs))
	case KindAddMember:
		m := &op.Member
		b = appendSlot32(b, op.Slot)
		b = codec.AppendString(b, m.ID)
		b = codec.AppendUvarint(b, m.Incarnation)
		b = codec.AppendU64(b, m.Key)
		b = codec.AppendString(b, m.Addr)
		b = codec.AppendString(b, m.Zone)
		b = append(b, byte(m.State))
		b = codec.AppendUvarint(b, uint64(m.Weight))
		b = appendSlot32(b, m.Cell)
		b = codec.AppendUvarint(b, m.Joined)
		b = codec.AppendUvarint(b, m.Since)
	case KindRemoveMember:
		b = appendSlot32(b, op.Slot)
	case KindSetState:
		b = appendSlot32(b, op.Slot)
		b = append(b, byte(op.State))
		b = codec.AppendUvarint(b, uint64(op.Weight))
	case KindSetCell:
		b = appendSlot32(b, op.Slot)
		b = appendSlot32(b, op.Cell)
	case KindReshape:
		b = appendShape(b, &op.Shape)

		b = codec.AppendUvarint(b, uint64(len(op.Cells)))
		for _, c := range op.Cells {
			b = appendSlot32(b, c)
		}
	case KindAssign:
		b = codec.AppendUvarint(b, uint64(len(op.Parts)))
		for _, p := range op.Parts {
			b = codec.AppendUvarint(b, uint64(p))
		}

		// A length mismatch is encoded faithfully so Apply rejects it.
		b = codec.AppendUvarint(b, uint64(len(op.Owners)))
		for _, o := range op.Owners {
			b = appendSlot32(b, o)
		}
	case KindEpoch:
		b = codec.AppendUvarint(b, op.Epoch)
		b = codec.AppendUvarint(b, uint64(op.Period)) //nolint:gosec // negative periods round-trip and are rejected by Apply.
	case KindInvalidate:
		b = codec.AppendU64(b, op.Seed)
		b = codec.AppendUvarint(b, uint64(op.Gen))
	}

	return b
}

func appendShape(b []byte, s *topology.Shape) []byte {
	b = append(b, s.Dims)
	for i := range int(min(s.Dims, topology.MaxDims)) {
		b = codec.AppendUvarint(b, uint64(s.Size[i]))
	}

	return b
}

func readShape(r *codec.Reader) topology.Shape {
	d := r.U8()
	if r.Err() != nil {
		return topology.Shape{}
	}

	if d == 0 || d > topology.MaxDims {
		r.Fail(codec.ErrInvalid)
		return topology.Shape{}
	}

	var sizes [topology.MaxDims]uint32
	for i := range int(d) {
		sizes[i] = r.Uvarint32()
	}

	if r.Err() != nil {
		return topology.Shape{}
	}

	s, err := topology.NewShape(sizes[:d]...)
	if err != nil {
		r.Fail(codec.ErrInvalid)
		return topology.Shape{}
	}

	return s
}

// DecodeDelta decodes a delta encoded by AppendDelta. It bounds every count
// by the input size and the package limits, and rejects non-canonical input.
// It does not check that the delta applies; Apply does.
func DecodeDelta(b []byte) (*Delta, error) {
	r := codec.NewReader(b)
	if f := r.U8(); r.Err() == nil && f != DeltaFormat {
		return nil, fmt.Errorf("%w: delta format %d", ErrFormat, f)
	}

	d := &Delta{}
	d.Base.Term = r.Uvarint()
	d.Base.Seq = r.Uvarint()
	d.Version.Term = r.Uvarint()
	d.Version.Seq = r.Uvarint()

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

func readOp(r *codec.Reader, op *Op) {
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
		n := r.Len(1<<placement.MaxBits, 1)
		if n > 0 {
			op.Parts = make([]uint32, n)
			for i := range op.Parts {
				op.Parts[i] = r.Uvarint32()
			}
		}

		n = r.Len(1<<placement.MaxBits, 1)
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
			r.Fail(codec.ErrInvalid)
		}

		op.Period = time.Duration(p) //nolint:gosec // checked above.
	case KindInvalidate:
		op.Seed = r.U64()
		op.Gen = r.Uvarint32()
	default:
		r.Fail(fmt.Errorf("%w: op kind %d", codec.ErrInvalid, op.Kind))
	}
}

// AppendSnapshot appends the canonical encoding of v. The encoding stores
// every member and the table (bit-packed columns), not the derived indexes.
func AppendSnapshot(b []byte, v *View) []byte {
	b = append(b, SnapshotFormat)
	b = codec.AppendUvarint(b, v.version.Term)
	b = codec.AppendUvarint(b, v.version.Seq)
	b = append(b, v.digest[:]...)
	b = appendSlot32(b, v.leader)
	b = codec.AppendUvarint(b, v.epoch)
	b = codec.AppendUvarint(b, uint64(v.period)) //nolint:gosec // periods are never negative in a view.

	b = codec.AppendUvarint(b, uint64(len(v.gens)))
	for _, g := range v.gens {
		b = codec.AppendU64(b, g.Seed)
		b = codec.AppendUvarint(b, uint64(g.Gen))
	}

	b = codec.AppendBool(b, v.hasConfig)
	if !v.hasConfig {
		return b
	}

	c := v.config
	b = append(b, c.PartitionBits)
	b = appendSlot32(b, c.InitialOwner)
	b = codec.AppendUvarint(b, uint64(c.SlotReuseEpochs))
	b = codec.AppendUvarint(b, uint64(c.HandoffEpochs))
	b = appendShape(b, &v.shape)
	b = codec.AppendUvarint(b, uint64(v.slotCap))

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

	b = codec.AppendUvarint(b, uint64(len(zones)))
	for _, z := range zones {
		b = codec.AppendString(b, z)
	}

	b = codec.AppendUvarint(b, uint64(v.n))
	next := Slot(0)

	v.ForEachMember(func(s Slot, m *Member) bool {
		b = codec.AppendUvarint(b, uint64(s-next))
		next = s + 1
		b = codec.AppendString(b, m.ID)
		b = codec.AppendUvarint(b, m.Incarnation)
		b = codec.AppendString(b, m.Addr)
		b = codec.AppendUvarint(b, zoneIdx[m.Zone])
		b = append(b, byte(m.State))
		b = codec.AppendUvarint(b, uint64(m.Weight))
		b = codec.AppendUvarint(b, uint64(m.Cell))
		b = codec.AppendUvarint(b, m.Joined)
		b = codec.AppendUvarint(b, m.Since-m.Joined)

		return true
	})

	for s := range v.slotCap {
		c := v.mem[s>>memShift]
		if c.mem[s&memMask].State == 0 {
			b = codec.AppendUvarint(b, c.reuse[s&memMask])
		}
	}

	t := v.table
	b = appendColumn(b, t, func(e placement.Entry) uint32 { return e.Owner + 1 })
	b = appendColumn(b, t, func(e placement.Entry) uint32 { return e.Prev + 1 })
	b = appendColumn(b, t, func(e placement.Entry) uint32 { return e.Moved })

	return b
}

// EncodeSnapshot returns the canonical snapshot encoding of v.
func EncodeSnapshot(v *View) []byte { return AppendSnapshot(nil, v) }

// appendColumn writes one table column as uvarint base, width byte, and a
// length-prefixed LSB-first bit-packed array of value-base.
func appendColumn(b []byte, t *placement.Table, f func(placement.Entry) uint32) []byte {
	lo, hi := uint32(math.MaxUint32), uint32(0)

	for ci := range t.NumChunks() {
		for _, e := range t.Chunk(ci) {
			x := f(e)
			lo, hi = min(lo, x), max(hi, x)
		}
	}

	w := bits.Len32(hi - lo)
	b = codec.AppendUvarint(b, uint64(lo))
	b = append(b, byte(w))

	nbytes := (t.Len()*w + 7) / 8
	b = codec.AppendUvarint(b, uint64(nbytes))

	if w == 0 {
		return b
	}

	var (
		acc  uint64
		have int
	)

	for ci := range t.NumChunks() {
		for _, e := range t.Chunk(ci) {
			acc |= uint64(f(e)-lo) << have

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

func readColumn(r *codec.Reader, n int) column {
	var c column

	c.lo = r.Uvarint32()
	c.w = int(r.U8())

	if c.w > 32 {
		r.Fail(codec.ErrInvalid)
		return c
	}

	c.data = r.Bytes(math.MaxInt32)
	if r.Err() == nil && len(c.data) != (n*c.w+7)/8 {
		r.Fail(codec.ErrInvalid)
	}

	// Unused high bits of the last byte must be zero (canonical form).
	if r.Err() == nil && c.w > 0 {
		if extra := len(c.data)*8 - n*c.w; extra > 0 && c.data[len(c.data)-1]>>(8-extra) != 0 {
			r.Fail(codec.ErrInvalid)
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
	r := codec.NewReader(b)
	if f := r.U8(); r.Err() == nil && f != SnapshotFormat {
		return nil, fmt.Errorf("%w: snapshot format %d", ErrFormat, f)
	}

	v := &View{}
	v.version.Term = r.Uvarint()
	v.version.Seq = r.Uvarint()
	r.Fixed(v.digest[:])
	v.leader = readSlot32(&r)
	v.epoch = r.Uvarint()

	p := r.Uvarint()
	if p > math.MaxInt64 {
		r.Fail(codec.ErrInvalid)
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

	if r.Err() == nil && (!placement.ValidBits(c.PartitionBits) || v.shape.Cells() > MaxCells) {
		r.Fail(codec.ErrInvalid)
	}

	slotCap := r.Uvarint()
	if r.Err() == nil && slotCap > MaxSlots {
		r.Fail(codec.ErrTooLarge)
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
		return nil, codec.ErrTooLarge
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

func readMembers(r *codec.Reader, v *View, zones []string) error {
	next := uint64(0)
	usedZones := uint64(0)

	for range v.n {
		s := next + r.Uvarint()
		if r.Err() == nil && s >= uint64(v.slotCap) {
			r.Fail(codec.ErrInvalid)
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
				r.Fail(codec.ErrInvalid)
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
			return codec.ErrInvalid
		}

		m.Zone = zones[z]
		m.Key = hashing.MemberKey(m.ID, m.Incarnation)
		v.mem[s>>memShift].mem[s&memMask] = m
	}

	if usedZones != uint64(len(zones)) {
		return codec.ErrInvalid
	}

	return nil
}

func readTable(r *codec.Reader, v *View) error {
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
	v.table = placement.NewTableFunc(v.config.PartitionBits, func(uint32) placement.Entry {
		var x [3]uint32
		for i := range cols {
			x[i] = cols[i].next()
			lo[i], hi[i] = min(lo[i], x[i]), max(hi[i], x[i])
		}

		e := placement.Entry{Owner: x[0] - 1, Prev: x[1] - 1, Moved: x[2]}
		if e.Owner != NoSlot && e.Owner >= MaxSlots || e.Prev != NoSlot && e.Prev >= MaxSlots {
			bad = true
		}

		return e
	})

	if bad {
		return codec.ErrInvalid
	}

	// Canonical columns use the minimum as base and the minimal width.
	for i := range cols {
		if lo[i] != cols[i].lo || bits.Len32(hi[i]-lo[i]) != cols[i].w {
			return codec.ErrInvalid
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
