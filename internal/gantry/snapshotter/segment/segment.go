// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package segment resolves the RACER image volume's segments to local block
// devices and converts blob addresses into byte offsets on those devices.
//
// The image volume is one RACER volume exported as one device on every node:
// an OCC catalog extent at offset zero, then a run of IMMUTABLE_4M extents
// concatenated after it. A segment is one of those extents, and it stays the
// unit of reclamation because RACER's only reclaim primitive, an extent's
// tombstone epoch, is per extent. What the snapshotter sees is one flat byte
// range, so a segment is addressed here by an offset into it rather than by a
// device of its own.
//
// Capacity is therefore fixed when the volume is created: a device's extent
// list is frozen for the device's life, so nothing can be appended later. The
// cleaner is what takes the place of growth.
//
// racer-ctrl publishes the mapping as a small JSON file (by default
// /run/racer/image-devices.json) that this package reads and watches. The
// container start path therefore never talks to the apiserver.
package segment

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
)

const (
	// PageBytes is the page size of an IMMUTABLE_4M extent. Writes and
	// discards must cover a whole aligned page of this size; reads may be
	// any BlockBytes-aligned subrange, which is what demand paging needs.
	PageBytes = 4 << 20

	// BlockBytes is RACER's LBA size and EROFS's block size.
	BlockBytes = 4 << 10

	// DefaultPath is where racer-ctrl publishes the mapping.
	DefaultPath = "/run/racer/image-devices.json"
)

// ErrUnknownSegment reports a blob addressed to a segment this node does not
// export. It is not fatal: the caller falls back to the local unpack path.
var ErrUnknownSegment = errors.New("segment not exported by this node")

// ErrNoCatalog reports a set that carries no catalog device, which means the
// image volume is not yet usable on this node.
var ErrNoCatalog = errors.New("no catalog device")

// Segment is one IMMUTABLE_4M extent of the image volume.
type Segment struct {
	// ID is the segment's cluster-wide identifier, which is the RACER extent
	// id. It is stable for the segment's life and is what catalog records
	// address. The device offset cannot serve that purpose, because the same
	// segment may sit at a different offset on a volume built differently.
	ID uint32 `json:"id"`

	// Offset is where the segment starts in the image device, in bytes. It
	// is a multiple of PageBytes and is the same on every node, because the
	// composition that produced it is stamped once and frozen.
	Offset uint64 `json:"offset"`

	// Bytes is the segment's capacity. Always a multiple of PageBytes.
	Bytes uint64 `json:"bytes"`

	// Epoch is the extent's tombstone epoch. It advances only when the
	// control plane has collected the extent, so a segment whose published
	// epoch is past the one the catalog recorded is one whose pages are gone
	// and whose space is free to reuse. It is the only signal that a reclaim
	// finished, because RACER reports nothing else about it.
	Epoch uint32 `json:"epoch"`
}

// Pages is the segment's capacity in 4 MiB pages.
func (s Segment) Pages() uint64 { return s.Bytes / PageBytes }

// End is the first byte of the device past the segment.
func (s Segment) End() uint64 { return s.Offset + s.Bytes }

// Set is the whole published mapping.
type Set struct {
	// Generation strictly increases each time racer-ctrl republishes. A
	// reader that sees a generation go backwards is looking at a stale file
	// and ignores it.
	Generation uint64 `json:"generation"`

	// Universe is the RACER universe the image volume lives in. Carried for
	// diagnostics; nothing on the read path needs it.
	Universe uint32 `json:"universe"`

	// Device is the local block device path, /dev/ublkb<minor>, carrying the
	// whole image volume. The minor is node-local and may differ between
	// nodes, which is why nothing persisted ever names it.
	Device string `json:"device"`

	// CatalogBytes is the size of the OCC catalog extent, which is always at
	// offset zero because it is the volume's mutable head.
	CatalogBytes uint64 `json:"catalogBytes"`

	Segments []Segment `json:"segments"`

	byID map[uint32]Segment
}

// Address is a blob's location: a segment and a whole number of 4 MiB pages
// within it, plus the blob's true byte length, which is at most the page span
// because a blob is tail-padded to a page boundary.
type Address struct {
	Segment    uint32
	PageOffset uint32
	PageCount  uint32
	ByteLength uint64
}

// Span is the address's page span in bytes.
func (a Address) Span() uint64 { return uint64(a.PageCount) * PageBytes }

// ByteOffset is the address's start in bytes from the beginning of its segment.
func (a Address) ByteOffset() uint64 { return uint64(a.PageOffset) * PageBytes }

// Fingerprint is 8 hex characters that identify where a blob sits and which
// writing of it this is.
//
// It exists to be part of a device mapper name, so that a blob the cleaner has
// relocated gets a different name from the one it had before. The record's
// generation is folded in as well as the address, because reclamation hands the
// same pages of the same segment to a different blob: without it, a mapping
// left over from the segment's previous life would be indistinguishable from
// the one wanted now, and Ensure would adopt it rather than rebuild it.
//
// Nothing depends on it being collision free across different placements of
// different blobs: the digest is what identifies the content, and the
// fingerprint only has to change when the placement does.
func (a Address) Fingerprint(generation uint64) string {
	h := fnv.New64a()

	var buf [28]byte

	binary.LittleEndian.PutUint32(buf[0:4], a.Segment)
	binary.LittleEndian.PutUint32(buf[4:8], a.PageOffset)
	binary.LittleEndian.PutUint32(buf[8:12], a.PageCount)
	binary.LittleEndian.PutUint64(buf[12:20], a.ByteLength)
	binary.LittleEndian.PutUint64(buf[20:28], generation)

	_, _ = h.Write(buf[:])

	return hex.EncodeToString(h.Sum(nil))[:8]
}

// Validate reports whether the address is self-consistent. A zero page count
// is rejected: a blob always occupies at least one page, and a zero-length
// dm-linear target is not a thing.
func (a Address) Validate() error {
	if a.PageCount == 0 {
		return errors.New("address spans no pages")
	}

	if a.ByteLength == 0 {
		return errors.New("address has zero byte length")
	}

	if a.ByteLength > a.Span() {
		return fmt.Errorf("address byte length %d exceeds its %d page span of %d bytes",
			a.ByteLength, a.PageCount, a.Span())
	}

	// The tail padding may never be a whole page: that would mean the page
	// count was computed from something other than the byte length, and a
	// dm-linear target longer than it needs to be is a silent bug.
	if a.Span()-a.ByteLength >= PageBytes {
		return fmt.Errorf("address spans %d pages for %d bytes, at least one page of which is padding",
			a.PageCount, a.ByteLength)
	}

	if uint64(a.PageOffset)+uint64(a.PageCount) > uint64(^uint32(0)) {
		return errors.New("address overflows the segment page space")
	}

	return nil
}

// PagesFor is the number of 4 MiB pages a blob of n bytes occupies. A blob is
// always padded up, because IMMUTABLE_4M refuses a write that does not cover a
// whole aligned page.
func PagesFor(n uint64) uint32 {
	if n == 0 {
		return 0
	}

	return uint32((n + PageBytes - 1) / PageBytes)
}

// PaddedSize rounds n up to a whole number of 4 MiB pages.
func PaddedSize(n uint64) uint64 { return uint64(PagesFor(n)) * PageBytes }

// Load reads and validates a published mapping.
func Load(path string) (*Set, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is operator-configured, not attacker-supplied
	if err != nil {
		return nil, fmt.Errorf("read image device map %q: %w", path, err)
	}

	return Parse(data)
}

// Parse decodes and validates a published mapping.
func Parse(data []byte) (*Set, error) {
	var set Set

	decoder := json.NewDecoder(bytes.NewReader(data))

	// Unknown fields are refused so that a newer racer-ctrl publishing a
	// field this build does not understand fails loudly here rather than
	// silently mapping blobs with half the mapping applied.
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&set); err != nil {
		return nil, fmt.Errorf("decode image device map: %w", err)
	}

	if err := set.index(); err != nil {
		return nil, err
	}

	return &set, nil
}

func (s *Set) index() error {
	if len(s.Segments) > 0 && s.Device == "" {
		return errors.New("segments named but no device to find them on")
	}

	s.byID = make(map[uint32]Segment, len(s.Segments))

	for _, seg := range s.Segments {
		if seg.ID == 0 {
			return errors.New("segment 0 is reserved")
		}

		if _, dup := s.byID[seg.ID]; dup {
			return fmt.Errorf("segment %d listed twice", seg.ID)
		}

		if seg.Bytes == 0 || seg.Bytes%PageBytes != 0 {
			return fmt.Errorf("segment %d has %d bytes, not a positive multiple of %d",
				seg.ID, seg.Bytes, PageBytes)
		}

		if seg.Offset%PageBytes != 0 {
			return fmt.Errorf("segment %d starts at %d, not a multiple of %d",
				seg.ID, seg.Offset, PageBytes)
		}

		if seg.Offset < s.CatalogBytes {
			return fmt.Errorf("segment %d starts at %d, inside the %d byte catalog head",
				seg.ID, seg.Offset, s.CatalogBytes)
		}

		s.byID[seg.ID] = seg
	}

	// Sorted so that anything derived from the set - log lines, metrics
	// series, the cleaner's choice of victim - is stable regardless of the
	// order racer-ctrl happened to emit.
	sort.Slice(s.Segments, func(i, j int) bool { return s.Segments[i].ID < s.Segments[j].ID })

	// Segments share one address space now, so an overlap is not a segment
	// reading its own bytes wrongly, it is two segments writing over each
	// other. Checked after the sort so the comparison is against the
	// neighbour rather than against whatever came last in the file.
	for i := 1; i < len(s.Segments); i++ {
		if prev, seg := s.Segments[i-1], s.Segments[i]; seg.Offset < prev.End() {
			return fmt.Errorf("segment %d starts at %d, inside segment %d which ends at %d",
				seg.ID, seg.Offset, prev.ID, prev.End())
		}
	}

	return nil
}

// Segment looks up an exported segment by id.
func (s *Set) Segment(id uint32) (Segment, error) {
	seg, ok := s.byID[id]
	if !ok {
		return Segment{}, fmt.Errorf("segment %d: %w", id, ErrUnknownSegment)
	}

	return seg, nil
}

// Catalog names the byte range of the image device holding the OCC catalog.
type Catalog struct {
	// Device is the image device. The catalog shares it with every segment.
	Device string

	// Bytes is the catalog extent's size. It starts at offset zero, because
	// a volume's mutable head is always the first entry of its composition.
	Bytes uint64
}

// CatalogDevice is the byte range holding the catalog extent.
func (s *Set) CatalogDevice() (Catalog, error) {
	if s.Device == "" || s.CatalogBytes == 0 {
		return Catalog{}, ErrNoCatalog
	}

	if s.CatalogBytes%BlockBytes != 0 {
		return Catalog{}, fmt.Errorf("catalog has %d bytes, not a positive multiple of %d",
			s.CatalogBytes, BlockBytes)
	}

	return Catalog{Device: s.Device, Bytes: s.CatalogBytes}, nil
}

// SegmentRange is the device byte range a whole segment occupies. The cleaner
// discards it, which is the only operation that addresses a segment as a whole.
func (s *Set) SegmentRange(id uint32) (device string, offset, length uint64, err error) {
	seg, err := s.Segment(id)
	if err != nil {
		return "", 0, 0, err
	}

	return s.Device, seg.Offset, seg.Bytes, nil
}

// Locate resolves a blob address to the device and byte range that carries it.
// It is the only place a cluster-wide segment id becomes a node-local path.
func (s *Set) Locate(addr Address) (device string, offset, length uint64, err error) {
	if err := addr.Validate(); err != nil {
		return "", 0, 0, err
	}

	seg, err := s.Segment(addr.Segment)
	if err != nil {
		return "", 0, 0, err
	}

	within := addr.ByteOffset()
	length = addr.Span()

	if within+length > seg.Bytes {
		return "", 0, 0, fmt.Errorf(
			"blob at pages %d..%d runs past segment %d, which holds %d pages",
			addr.PageOffset, uint64(addr.PageOffset)+uint64(addr.PageCount), seg.ID, seg.Pages())
	}

	return s.Device, seg.Offset + within, length, nil
}
