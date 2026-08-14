// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// SegmentState is where a segment sits in the log-structured cycle.
type SegmentState uint32

const (
	// SegmentEmpty is a segment that has been reclaimed and carries nothing.
	SegmentEmpty SegmentState = 0

	// SegmentOpen is the segment reservations currently append to. At most
	// one segment is open at a time, named by Superblock.OpenSegment.
	SegmentOpen SegmentState = 1

	// SegmentSealed is a full segment, still serving reads.
	SegmentSealed SegmentState = 2

	// SegmentCleaning is a sealed segment whose survivors are being copied
	// out. Its records are still live; nothing may be reclaimed until every
	// node reports past the repoint generation.
	SegmentCleaning SegmentState = 3

	// SegmentDraining is a segment with no live records left, waiting for
	// the last node to drop its mounts before the operator bumps the
	// extent's tombstone epoch. The bump is destructive to every page still
	// live below the new epoch, which is why the drain exists.
	SegmentDraining SegmentState = 4

	// SegmentMarking is a sealed segment the cleaner has opened a mark
	// round on: every node is being asked which of the segment's blobs it
	// still references, so the ones nobody names can be retired. Nothing
	// moves and nothing is destroyed while a segment is marking; the state
	// exists so that the question, and the generation it was asked at, are
	// visible to every node rather than held in one cleaner's memory.
	SegmentMarking SegmentState = 5
)

// String renders a segment state for logs and errors.
func (s SegmentState) String() string {
	switch s {
	case SegmentEmpty:
		return "empty"
	case SegmentOpen:
		return "open"
	case SegmentSealed:
		return "sealed"
	case SegmentCleaning:
		return "cleaning"
	case SegmentDraining:
		return "draining"
	case SegmentMarking:
		return "marking"
	default:
		return fmt.Sprintf("unknown(%d)", uint32(s))
	}
}

// SegmentEntry is the catalog's view of one segment: where its append cursor
// sits and how much of what it holds is still referenced.
type SegmentEntry struct {
	ID    uint32
	State SegmentState

	// CursorPages is the next free 4 MiB page. It only ever advances within
	// a segment's life; reclaiming is what resets it, and reclaiming means a
	// tombstone epoch bump.
	CursorPages uint32

	// TotalPages is the segment's capacity in 4 MiB pages.
	TotalPages uint32

	// LiveBytes and DeadBytes are the padded sizes of the blobs this segment
	// holds that are and are not still referenced. Their sum plus the free
	// tail is the segment's capacity. The cleaner picks a victim by the
	// ratio.
	LiveBytes uint64
	DeadBytes uint64

	// Epoch is the tombstone epoch this entry was written against. It is
	// the catalog's copy of the extent's epoch, and it is how a reclaim is
	// noticed: RACER reports nothing when it collects an extent, so the
	// only evidence is the published epoch running past this one.
	Epoch uint32

	// RepointGeneration is the superblock generation by which every record
	// naming this segment had been superseded. It is set when the cleaner
	// finishes copying survivors out, and it is what the drain gate is
	// measured against: a node whose watermark has passed it can no longer
	// resolve a blob to this segment.
	RepointGeneration uint64
}

// FreePages is how many 4 MiB pages remain past the append cursor.
func (e SegmentEntry) FreePages() uint32 {
	if e.CursorPages >= e.TotalPages {
		return 0
	}

	return e.TotalPages - e.CursorPages
}

// LiveFraction is the share of the segment's capacity that is still
// referenced, in [0, 1]. The cleaner uses it to choose a victim. A segment with
// no capacity reports 0 rather than dividing by zero.
func (e SegmentEntry) LiveFraction() float64 {
	capacity := uint64(e.TotalPages) * segment.PageBytes
	if capacity == 0 {
		return 0
	}

	return float64(e.LiveBytes) / float64(capacity)
}

// Validate checks a segment entry's internal consistency.
func (e SegmentEntry) Validate() error {
	if e.ID == 0 {
		return fmt.Errorf("%w: segment table entry with id 0", ErrCorrupt)
	}

	if e.CursorPages > e.TotalPages {
		return fmt.Errorf("%w: segment %d cursor at page %d past its %d page capacity",
			ErrCorrupt, e.ID, e.CursorPages, e.TotalPages)
	}

	used := e.LiveBytes + e.DeadBytes
	if used > uint64(e.CursorPages)*segment.PageBytes {
		return fmt.Errorf("%w: segment %d accounts for %d bytes but its cursor has only handed out %d",
			ErrCorrupt, e.ID, used, uint64(e.CursorPages)*segment.PageBytes)
	}

	if e.State == SegmentDraining && e.RepointGeneration == 0 {
		return fmt.Errorf("%w: segment %d is draining with no repoint generation to drain past",
			ErrCorrupt, e.ID)
	}

	return nil
}

// MarshalTo writes the entry into dst, which must be SegmentEntryBytes long.
//
// Wire layout:
//
//	 0..4   id
//	 4..8   state
//	 8..12  cursor pages
//	12..16  total pages
//	16..24  live bytes
//	24..32  dead bytes
//	32..36  tombstone epoch
//	36..40  reserved
//	40..48  repoint generation
//	48..64  reserved
func (e SegmentEntry) MarshalTo(dst []byte) error {
	if len(dst) != SegmentEntryBytes {
		return fmt.Errorf("segment entry buffer is %d bytes, want %d", len(dst), SegmentEntryBytes)
	}

	clear(dst)

	binary.LittleEndian.PutUint32(dst[0:4], e.ID)
	binary.LittleEndian.PutUint32(dst[4:8], uint32(e.State))
	binary.LittleEndian.PutUint32(dst[8:12], e.CursorPages)
	binary.LittleEndian.PutUint32(dst[12:16], e.TotalPages)
	binary.LittleEndian.PutUint64(dst[16:24], e.LiveBytes)
	binary.LittleEndian.PutUint64(dst[24:32], e.DeadBytes)
	binary.LittleEndian.PutUint32(dst[32:36], e.Epoch)
	binary.LittleEndian.PutUint64(dst[40:48], e.RepointGeneration)

	return nil
}

// UnmarshalSegmentEntry decodes one segment table entry. An all-zero slot
// decodes to the zero entry with no error.
func UnmarshalSegmentEntry(src []byte) (SegmentEntry, error) {
	if len(src) != SegmentEntryBytes {
		return SegmentEntry{}, fmt.Errorf("segment entry buffer is %d bytes, want %d",
			len(src), SegmentEntryBytes)
	}

	if allZero(src) {
		return SegmentEntry{}, nil
	}

	e := SegmentEntry{
		ID:          binary.LittleEndian.Uint32(src[0:4]),
		State:       SegmentState(binary.LittleEndian.Uint32(src[4:8])),
		CursorPages: binary.LittleEndian.Uint32(src[8:12]),
		TotalPages:  binary.LittleEndian.Uint32(src[12:16]),
		LiveBytes:   binary.LittleEndian.Uint64(src[16:24]),
		DeadBytes:   binary.LittleEndian.Uint64(src[24:32]),

		Epoch:             binary.LittleEndian.Uint32(src[32:36]),
		RepointGeneration: binary.LittleEndian.Uint64(src[40:48]),
	}

	return e, e.Validate()
}

// MarshalSegmentBlock packs segment table entries into one block at their
// given slots. As with record blocks, slots may be filled out of order and
// with gaps, so that a writer updating one segment's accounting rewrites the
// block without needing every other segment's entry in hand.
func MarshalSegmentBlock(slots map[int]SegmentEntry) ([]byte, error) {
	block := make([]byte, BlockBytes)
	binary.LittleEndian.PutUint16(block[4:6], Version)

	for slot, e := range slots {
		if slot < 0 || slot >= SegmentsPerBlock {
			return nil, fmt.Errorf("segment slot %d is outside the %d that fit in a block",
				slot, SegmentsPerBlock)
		}

		off := segmentPageHeaderBytes + slot*SegmentEntryBytes
		if err := e.MarshalTo(block[off : off+SegmentEntryBytes]); err != nil {
			return nil, err
		}
	}

	binary.LittleEndian.PutUint32(block[0:4], crc32.Checksum(block[4:], castagnoli))

	return block, nil
}

// UnmarshalSegmentBlock decodes a segment table block into its occupied slots.
func UnmarshalSegmentBlock(block []byte) (map[int]SegmentEntry, error) {
	if len(block) != BlockBytes {
		return nil, fmt.Errorf("segment block is %d bytes, want %d", len(block), BlockBytes)
	}

	if allZero(block) {
		return map[int]SegmentEntry{}, nil
	}

	want := binary.LittleEndian.Uint32(block[0:4])
	if got := crc32.Checksum(block[4:], castagnoli); got != want {
		return nil, fmt.Errorf("%w: segment block checksum %08x, want %08x", ErrCorrupt, got, want)
	}

	if version := binary.LittleEndian.Uint16(block[4:6]); version > Version {
		return nil, fmt.Errorf("%w: segment block version %d, this build reads %d",
			ErrCorrupt, version, Version)
	}

	slots := make(map[int]SegmentEntry)

	for i := range SegmentsPerBlock {
		off := segmentPageHeaderBytes + i*SegmentEntryBytes

		e, err := UnmarshalSegmentEntry(block[off : off+SegmentEntryBytes])
		if err != nil {
			return nil, fmt.Errorf("segment slot %d: %w", i, err)
		}

		if e.ID == 0 {
			continue
		}

		slots[i] = e
	}

	return slots, nil
}

// Superblock is block 0 and the catalog's only compare-and-swap point.
//
// Reserving space for a blob has to advance both the open segment's append
// cursor and the record count, and it has to do so atomically: if the cursor
// moved and the record count did not, a second writer would hand the same
// pages to somebody else. Two writes cannot do that, because the second one
// can fail. So the open segment's cursor lives here, in the same block as the
// record count, and a reservation is one optimistic write. That serializes
// allocation cluster-wide without a lock service.
//
// The segment table's copy of the open segment's cursor is therefore only
// accounting. It is brought up to date lazily, and authoritatively when the
// segment is sealed.
type Superblock struct {
	// Generation strictly increases on every successful write. It is what
	// makes the compare-and-swap observable to a reader and what nodes
	// report when the cleaner needs to know a drain is complete.
	Generation uint64

	// RecordCount is the number of records appended so far. Records are
	// append only, so this doubles as the append cursor: record n lives at
	// block RecordBlockBase + n/RecordsPerBlock, slot n%RecordsPerBlock.
	RecordCount uint64

	// OpenSegment is the segment reservations append to, or 0 if none is
	// open, in which case ingest is refused until the operator adds one.
	OpenSegment uint32

	// OpenCursorPages is the authoritative append cursor within the open
	// segment, in 4 MiB pages.
	OpenCursorPages uint32

	// OpenTotalPages is the open segment's capacity in 4 MiB pages. Carried
	// here so a reservation can be bounds-checked without reading the
	// segment table, which would put a second block on the allocation path.
	OpenTotalPages uint32

	// SegmentBlocks is how many blocks the segment table occupies. It is
	// fixed when the catalog is formatted, because growing it would move
	// every record.
	SegmentBlocks uint32

	// NodeBlocks is how many blocks the node table occupies, immediately
	// after the segment table, one block per node. Fixed at format time for
	// the same reason.
	NodeBlocks uint32

	// TotalBlocks is the catalog extent's size in 4 KiB blocks.
	TotalBlocks uint64
}

// OpenFreePages is how many 4 MiB pages remain in the open segment.
func (s Superblock) OpenFreePages() uint32 {
	if s.OpenSegment == 0 || s.OpenCursorPages >= s.OpenTotalPages {
		return 0
	}

	return s.OpenTotalPages - s.OpenCursorPages
}

// NodeBlockBase is the first block of the node table.
func (s Superblock) NodeBlockBase() uint64 { return 1 + uint64(s.SegmentBlocks) }

// RecordBlockBase is the first block holding records.
func (s Superblock) RecordBlockBase() uint64 {
	return s.NodeBlockBase() + uint64(s.NodeBlocks)
}

// NodeCapacity is how many nodes the node table can hold.
func (s Superblock) NodeCapacity() int { return int(s.NodeBlocks) }

// NodeBlockAt is the block node n occupies.
func (s Superblock) NodeBlockAt(n int) uint64 {
	return s.NodeBlockBase() + uint64(n) //nolint:gosec // bounded by the node capacity
}

// RecordCapacity is how many records the catalog can hold before it is full.
func (s Superblock) RecordCapacity() uint64 {
	base := s.RecordBlockBase()
	if s.TotalBlocks <= base {
		return 0
	}

	return (s.TotalBlocks - base) * RecordsPerBlock
}

// RecordLocation is the block and slot record n occupies.
func (s Superblock) RecordLocation(n uint64) (block uint64, slot int) {
	return s.RecordBlockBase() + n/RecordsPerBlock, int(n % RecordsPerBlock)
}

// Validate checks the superblock's internal consistency.
func (s Superblock) Validate() error {
	if s.Generation == 0 {
		return fmt.Errorf("%w: superblock generation 0", ErrCorrupt)
	}

	if s.SegmentBlocks == 0 {
		return fmt.Errorf("%w: superblock reserves no segment table blocks", ErrCorrupt)
	}

	if s.NodeBlocks == 0 {
		return fmt.Errorf("%w: superblock reserves no node table blocks", ErrCorrupt)
	}

	if s.TotalBlocks <= s.RecordBlockBase() {
		return fmt.Errorf("%w: catalog of %d blocks leaves no room for records past block %d",
			ErrCorrupt, s.TotalBlocks, s.RecordBlockBase())
	}

	if s.RecordCount > s.RecordCapacity() {
		return fmt.Errorf("%w: superblock claims %d records, the catalog holds %d",
			ErrCorrupt, s.RecordCount, s.RecordCapacity())
	}

	if s.OpenSegment == 0 && (s.OpenCursorPages != 0 || s.OpenTotalPages != 0) {
		return fmt.Errorf("%w: superblock has no open segment but a cursor at page %d of %d",
			ErrCorrupt, s.OpenCursorPages, s.OpenTotalPages)
	}

	if s.OpenCursorPages > s.OpenTotalPages {
		return fmt.Errorf("%w: open segment %d cursor at page %d past its %d page capacity",
			ErrCorrupt, s.OpenSegment, s.OpenCursorPages, s.OpenTotalPages)
	}

	return nil
}

// MarshalTo writes the superblock into dst, which must be BlockBytes long.
//
// Wire layout:
//
//	   0..4   magic
//	   4..6   version
//	   6..8   flags
//	   8..16  generation
//	  16..24  record count
//	  24..28  open segment
//	  28..32  open segment cursor, in 4 MiB pages
//	  32..36  open segment capacity, in 4 MiB pages
//	  36..40  segment table blocks
//	  40..48  total blocks
//	  48..52  node table blocks
//	  52..4092 reserved
//	4092..4096 CRC32C over bytes 0..4092
func (s Superblock) MarshalTo(dst []byte) error {
	if len(dst) != BlockBytes {
		return fmt.Errorf("superblock buffer is %d bytes, want %d", len(dst), BlockBytes)
	}

	clear(dst)

	binary.LittleEndian.PutUint32(dst[0:4], Magic)
	binary.LittleEndian.PutUint16(dst[4:6], Version)
	binary.LittleEndian.PutUint64(dst[8:16], s.Generation)
	binary.LittleEndian.PutUint64(dst[16:24], s.RecordCount)
	binary.LittleEndian.PutUint32(dst[24:28], s.OpenSegment)
	binary.LittleEndian.PutUint32(dst[28:32], s.OpenCursorPages)
	binary.LittleEndian.PutUint32(dst[32:36], s.OpenTotalPages)
	binary.LittleEndian.PutUint32(dst[36:40], s.SegmentBlocks)
	binary.LittleEndian.PutUint64(dst[40:48], s.TotalBlocks)
	binary.LittleEndian.PutUint32(dst[48:52], s.NodeBlocks)
	binary.LittleEndian.PutUint32(dst[BlockBytes-4:], crc32.Checksum(dst[:BlockBytes-4], castagnoli))

	return nil
}

// UnmarshalSuperblock decodes block 0.
func UnmarshalSuperblock(block []byte) (Superblock, error) {
	if len(block) != BlockBytes {
		return Superblock{}, fmt.Errorf("superblock is %d bytes, want %d", len(block), BlockBytes)
	}

	// A blank device is reported separately from a corrupt one. The
	// difference decides whether a daemon may lay down a fresh catalog, and
	// conflating them would let one erase a catalog it merely failed to
	// parse.
	if allZero(block) {
		return Superblock{}, ErrUnformatted
	}

	if magic := binary.LittleEndian.Uint32(block[0:4]); magic != Magic {
		return Superblock{}, fmt.Errorf("%w: superblock magic %08x, want %08x", ErrCorrupt, magic, Magic)
	}

	want := binary.LittleEndian.Uint32(block[BlockBytes-4:])
	if got := crc32.Checksum(block[:BlockBytes-4], castagnoli); got != want {
		return Superblock{}, fmt.Errorf("%w: superblock checksum %08x, want %08x", ErrCorrupt, got, want)
	}

	switch version := binary.LittleEndian.Uint16(block[4:6]); {
	case version > Version:
		return Superblock{}, fmt.Errorf("%w: catalog version %d, this build reads %d",
			ErrCorrupt, version, Version)
	case version < MinVersion:
		return Superblock{}, fmt.Errorf("%w: catalog version %d predates the node table, reformat the volume",
			ErrCorrupt, version)
	}

	s := Superblock{
		Generation:      binary.LittleEndian.Uint64(block[8:16]),
		RecordCount:     binary.LittleEndian.Uint64(block[16:24]),
		OpenSegment:     binary.LittleEndian.Uint32(block[24:28]),
		OpenCursorPages: binary.LittleEndian.Uint32(block[28:32]),
		OpenTotalPages:  binary.LittleEndian.Uint32(block[32:36]),
		SegmentBlocks:   binary.LittleEndian.Uint32(block[36:40]),
		TotalBlocks:     binary.LittleEndian.Uint64(block[40:48]),
		NodeBlocks:      binary.LittleEndian.Uint32(block[48:52]),
	}

	return s, s.Validate()
}
