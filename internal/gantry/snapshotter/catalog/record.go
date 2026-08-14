// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package catalog implements the image volume's in-band index: the
// cluster-wide map from a containerd chainID to the location of an EROFS blob
// inside a RACER segment.
//
// The index is read on every Prepare, which is the container start critical
// path. Putting it in the apiserver would mean thousands of objects and one
// watcher per node on that path. Putting it in RACER makes a lookup a block
// read, keeps it consistent with the data it indexes, and scales with the
// fabric rather than with etcd.
//
// It lives in an OCC extent at the head of the image volume. IMMUTABLE is
// write-once per tombstone epoch and so cannot hold a mutable index; LWW would
// let a stale writer clobber a live reservation. OCC's guard is the version
// this node last read, which is exactly a compare-and-swap.
//
// Layout, in 4 KiB blocks:
//
//	block 0            superblock, the only compare-and-swap point
//	block 1..S         segment table, 127 entries per block
//	block S+1..N       records, append only, 31 per block
//
// Records are never rewritten in place. A blob that moves during segment
// cleaning gets a fresh record at a higher generation and readers take the
// highest generation for a key, so a reader that has not yet seen the new
// record keeps resolving the old location, which is still live until the
// cleaner's drain completes.
package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

const (
	// BlockBytes is the catalog's page size. The catalog extent is an OCC
	// extent, whose pages are 4 KiB.
	BlockBytes = segment.BlockBytes

	// RecordBytes is the fixed size of one catalog record.
	RecordBytes = 128

	// recordPageHeaderBytes is the per-block header preceding records: a
	// CRC32C over the rest of the block in bytes 0..3, the format version in
	// bytes 4..5, and two bytes reserved for a future field. There is no
	// count of occupied slots, deliberately: slots fill out of order, so a
	// count would be a second thing to keep consistent under a racing
	// compare-and-swap for no gain over scanning the 31 slots.
	recordPageHeaderBytes = 8

	// RecordsPerBlock is how many records fit in a 4 KiB block.
	RecordsPerBlock = (BlockBytes - recordPageHeaderBytes) / RecordBytes

	// SegmentEntryBytes is the fixed size of one segment table entry.
	SegmentEntryBytes = 64

	// segmentPageHeaderBytes mirrors recordPageHeaderBytes for the segment
	// table.
	segmentPageHeaderBytes = 8

	// SegmentsPerBlock is how many segment table entries fit in a block.
	SegmentsPerBlock = (BlockBytes - segmentPageHeaderBytes) / SegmentEntryBytes

	// WatermarkEntryBytes is the fixed size of one node watermark entry.
	WatermarkEntryBytes = 32

	// watermarkPageHeaderBytes mirrors recordPageHeaderBytes for the
	// watermark table.
	watermarkPageHeaderBytes = 8

	// WatermarksPerBlock is how many node watermarks fit in a block.
	WatermarksPerBlock = (BlockBytes - watermarkPageHeaderBytes) / WatermarkEntryBytes

	// Magic identifies a gantry-snapshotter catalog superblock.
	Magic = 0x47534E50 // "GSNP"

	// Version is the format version this build reads and writes. A reader
	// that finds a higher version refuses the catalog rather than guessing,
	// because a misread record maps a container's root filesystem to the
	// wrong bytes.
	Version = 1

	// DigestBytes is the length of the digests records key on. Only sha256
	// is representable; a layer with any other digest algorithm is never
	// ingested and always takes the local unpack path.
	DigestBytes = 32
)

// ErrConflict reports that an optimistic write lost its compare-and-swap. The
// caller re-reads and retries. Device adapters map RACER's OCC conflict to
// this.
var ErrConflict = errors.New("catalog: optimistic write conflict")

// ErrCorrupt reports a block whose checksum or framing did not hold up.
var ErrCorrupt = errors.New("catalog: corrupt block")

// ErrUnformatted reports a device whose first block is blank, which means no
// catalog has ever been written to it. It is kept distinct from ErrCorrupt
// because only the first case may be formatted over.
var ErrUnformatted = errors.New("catalog: device is not formatted")

// castagnoli is CRC32C, matching what RACER uses for its own small pages.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// RecordType discriminates the three things a record can say.
type RecordType uint8

const (
	// RecordUnused is the zero value and marks a record slot as empty. It is
	// never written deliberately.
	RecordUnused RecordType = 0

	// RecordBlob publishes an EROFS blob: Key is the layer's diffID and Ref
	// is the sha256 of the blob bytes, kept for offline scrub. Verifying it
	// at mount time would read the whole blob and defeat demand paging.
	RecordBlob RecordType = 1

	// RecordChain says a chainID resolves to a diffID's blob: Key is the
	// chainID and Ref is the diffID. Cross-image layer dedup shows up here,
	// as two chainIDs pointing at one blob.
	RecordChain RecordType = 2

	// RecordTombstone retires an earlier record: Key is the retired key and
	// Generation is strictly above the generation it retires.
	RecordTombstone RecordType = 3

	// RecordVoid fills a slot whose writer claimed it and then gave up.
	//
	// It says nothing about any key and resolves to nothing. It exists
	// because a reservation publishes its record count before the records
	// themselves are written, so readers stop at the first empty slot and
	// wait for the writer to catch up. A writer that dies between the two
	// leaves a hole that every node in the cluster stops at forever, which
	// freezes the catalog: nothing appended after it is ever seen again.
	// Writing voids into the slots retires the hole and lets readers past.
	RecordVoid RecordType = 4
)

// String renders a record type for logs and errors.
func (t RecordType) String() string {
	switch t {
	case RecordUnused:
		return "unused"
	case RecordBlob:
		return "blob"
	case RecordChain:
		return "chain"
	case RecordTombstone:
		return "tombstone"
	case RecordVoid:
		return "void"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(t))
	}
}

// Valid reports whether the type is one this build understands.
func (t RecordType) Valid() bool {
	return t == RecordBlob || t == RecordChain || t == RecordTombstone || t == RecordVoid
}

// Digest is a sha256 digest in its raw 32-byte form. Records carry raw bytes
// rather than the "sha256:hex" string so a record stays 128 bytes.
type Digest [DigestBytes]byte

// Record is one catalog entry. It is exactly RecordBytes on the wire.
type Record struct {
	Type  RecordType
	Flags uint8

	// Segment, PageOffset, PageCount and ByteLength address the blob and are
	// meaningful only on RecordBlob.
	Segment    uint32
	PageOffset uint32
	PageCount  uint32
	ByteLength uint64

	// Generation orders records for the same key. Readers take the highest.
	Generation uint64

	Key Digest
	Ref Digest
}

// Address is the blob location a RecordBlob carries.
func (r Record) Address() segment.Address {
	return segment.Address{
		Segment:    r.Segment,
		PageOffset: r.PageOffset,
		PageCount:  r.PageCount,
		ByteLength: r.ByteLength,
	}
}

// Validate checks the invariants a record must hold for its type. It is run on
// every record read back from the device, because a record that survives its
// CRC but addresses nonsense still maps a container's root filesystem to the
// wrong bytes.
func (r Record) Validate() error {
	if !r.Type.Valid() {
		return fmt.Errorf("%w: record type %s", ErrCorrupt, r.Type)
	}

	if r.Generation == 0 {
		return fmt.Errorf("%w: record generation 0", ErrCorrupt)
	}

	switch r.Type {
	case RecordBlob:
		if err := r.Address().Validate(); err != nil {
			return fmt.Errorf("%w: blob record: %s", ErrCorrupt, err)
		}

		if r.Segment == 0 {
			return fmt.Errorf("%w: blob record addresses segment 0", ErrCorrupt)
		}
	case RecordChain:
		if r.Ref == (Digest{}) {
			return fmt.Errorf("%w: chain record names no blob", ErrCorrupt)
		}
	case RecordVoid:
		// A void names nothing. The writer that abandons a reservation
		// after a crash-recovery scan does not know what the slot was
		// going to hold, and a void that had to invent a key would be
		// indistinguishable from a real record for that key.
		return nil
	case RecordTombstone, RecordUnused:
	}

	if r.Key == (Digest{}) {
		return fmt.Errorf("%w: record has an empty key", ErrCorrupt)
	}

	return nil
}

// MarshalTo writes the record into dst, which must be RecordBytes long.
//
// Wire layout:
//
//	  0      type
//	  1      flags
//	  2..4   reserved
//	  4..8   segment id
//	  8..12  page offset within the segment
//	 12..16  page count
//	 16..24  byte length
//	 24..32  generation
//	 32..64  key
//	 64..96  ref
//	 96..124 reserved
//	124..128 CRC32C over bytes 0..124
func (r Record) MarshalTo(dst []byte) error {
	if len(dst) != RecordBytes {
		return fmt.Errorf("record buffer is %d bytes, want %d", len(dst), RecordBytes)
	}

	clear(dst)

	dst[0] = byte(r.Type)
	dst[1] = r.Flags
	binary.LittleEndian.PutUint32(dst[4:8], r.Segment)
	binary.LittleEndian.PutUint32(dst[8:12], r.PageOffset)
	binary.LittleEndian.PutUint32(dst[12:16], r.PageCount)
	binary.LittleEndian.PutUint64(dst[16:24], r.ByteLength)
	binary.LittleEndian.PutUint64(dst[24:32], r.Generation)
	copy(dst[32:64], r.Key[:])
	copy(dst[64:96], r.Ref[:])
	binary.LittleEndian.PutUint32(dst[124:128], crc32.Checksum(dst[:124], castagnoli))

	return nil
}

// UnmarshalRecord decodes one record. A slot that is entirely zero decodes as
// RecordUnused with no error, which is how an unwritten tail of a block reads.
func UnmarshalRecord(src []byte) (Record, error) {
	if len(src) != RecordBytes {
		return Record{}, fmt.Errorf("record buffer is %d bytes, want %d", len(src), RecordBytes)
	}

	if allZero(src) {
		return Record{}, nil
	}

	want := binary.LittleEndian.Uint32(src[124:128])
	if got := crc32.Checksum(src[:124], castagnoli); got != want {
		return Record{}, fmt.Errorf("%w: record checksum %08x, want %08x", ErrCorrupt, got, want)
	}

	r := Record{
		Type:       RecordType(src[0]),
		Flags:      src[1],
		Segment:    binary.LittleEndian.Uint32(src[4:8]),
		PageOffset: binary.LittleEndian.Uint32(src[8:12]),
		PageCount:  binary.LittleEndian.Uint32(src[12:16]),
		ByteLength: binary.LittleEndian.Uint64(src[16:24]),
		Generation: binary.LittleEndian.Uint64(src[24:32]),
	}

	copy(r.Key[:], src[32:64])
	copy(r.Ref[:], src[64:96])

	return r, r.Validate()
}

// MarshalRecordBlock packs records into one 4 KiB block at their given slots.
//
// Slots may be filled out of order and with gaps. That is not a convenience:
// two ingesters can hold reservations for different slots in the same block,
// and each rewrites the block with its own slot filled and the other's
// untouched, relying on the block's own compare-and-swap to serialize them. A
// format that required a dense prefix could not express the intermediate
// state.
//
// The block header is a CRC32C over the rest of the block, so a torn write is
// caught even when every individual record's own checksum happens to hold.
func MarshalRecordBlock(slots map[int]Record) ([]byte, error) {
	block := make([]byte, BlockBytes)
	binary.LittleEndian.PutUint16(block[4:6], Version)

	for slot, r := range slots {
		if slot < 0 || slot >= RecordsPerBlock {
			return nil, fmt.Errorf("record slot %d is outside the %d that fit in a block",
				slot, RecordsPerBlock)
		}

		off := recordPageHeaderBytes + slot*RecordBytes
		if err := r.MarshalTo(block[off : off+RecordBytes]); err != nil {
			return nil, err
		}
	}

	binary.LittleEndian.PutUint32(block[0:4], crc32.Checksum(block[4:], castagnoli))

	return block, nil
}

// UnmarshalRecordBlock decodes a 4 KiB record block into its occupied slots.
// An all-zero block is an unwritten block and decodes to nothing.
func UnmarshalRecordBlock(block []byte) (map[int]Record, error) {
	if len(block) != BlockBytes {
		return nil, fmt.Errorf("record block is %d bytes, want %d", len(block), BlockBytes)
	}

	if allZero(block) {
		return map[int]Record{}, nil
	}

	want := binary.LittleEndian.Uint32(block[0:4])
	if got := crc32.Checksum(block[4:], castagnoli); got != want {
		return nil, fmt.Errorf("%w: record block checksum %08x, want %08x", ErrCorrupt, got, want)
	}

	if version := binary.LittleEndian.Uint16(block[4:6]); version > Version {
		return nil, fmt.Errorf("%w: record block version %d, this build reads %d",
			ErrCorrupt, version, Version)
	}

	slots := make(map[int]Record)

	for i := range RecordsPerBlock {
		off := recordPageHeaderBytes + i*RecordBytes

		r, err := UnmarshalRecord(block[off : off+RecordBytes])
		if err != nil {
			return nil, fmt.Errorf("record slot %d: %w", i, err)
		}

		if r.Type == RecordUnused {
			continue
		}

		slots[i] = r
	}

	return slots, nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}

	return true
}
