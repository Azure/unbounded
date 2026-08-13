// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package segment resolves the RACER image volume's segments to local block
// devices and converts blob addresses into byte offsets on those devices.
//
// The image volume is a set of IMMUTABLE_4M extents, one per segment, each
// exported as its own RACER device. A device's extent list is frozen for the
// device's life, so giving every segment its own device means growing image
// capacity only ever adds a device and never republishes an existing one. No
// node has to re-point a live dm table when the cluster grows.
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

// Segment is one IMMUTABLE_4M extent exported as its own RACER device.
type Segment struct {
	// ID is the segment's cluster-wide identifier. It is stable across the
	// segment's life and is what catalog records address.
	ID uint32 `json:"id"`

	// Device is the local block device path, /dev/ublkb<minor>. The minor is
	// node-local and may differ between nodes for the same segment, which is
	// exactly why records address segments by ID and not by path.
	Device string `json:"device"`

	// Bytes is the segment's capacity. Always a multiple of PageBytes.
	Bytes uint64 `json:"bytes"`
}

// Pages is the segment's capacity in 4 MiB pages.
func (s Segment) Pages() uint64 { return s.Bytes / PageBytes }

// Catalog names the device holding the image volume's OCC catalog extent.
type Catalog struct {
	Device string `json:"device"`
	Bytes  uint64 `json:"bytes"`
}

// Set is the whole published mapping.
type Set struct {
	// Generation strictly increases each time racer-ctrl republishes. A
	// reader that sees a generation go backwards is looking at a stale file
	// and ignores it.
	Generation uint64 `json:"generation"`

	// Universe is the RACER universe the image volume lives in. Carried for
	// diagnostics; nothing on the read path needs it.
	Universe uint32 `json:"universe"`

	Catalog  Catalog   `json:"catalog"`
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

// Fingerprint is 8 hex characters that identify where a blob sits.
//
// It exists to be part of a device mapper name, so that a blob the cleaner has
// relocated gets a different name from the one it had before. Nothing depends
// on it being collision free across different placements of different blobs:
// the digest is what identifies the content, and the fingerprint only has to
// change when the placement does.
func (a Address) Fingerprint() string {
	h := fnv.New64a()

	var buf [24]byte

	binary.LittleEndian.PutUint32(buf[0:4], a.Segment)
	binary.LittleEndian.PutUint32(buf[4:8], a.PageOffset)
	binary.LittleEndian.PutUint32(buf[8:12], a.PageCount)
	binary.LittleEndian.PutUint64(buf[12:20], a.ByteLength)

	_, _ = h.Write(buf[:20])

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
	s.byID = make(map[uint32]Segment, len(s.Segments))

	for _, seg := range s.Segments {
		if seg.ID == 0 {
			return errors.New("segment 0 is reserved")
		}

		if _, dup := s.byID[seg.ID]; dup {
			return fmt.Errorf("segment %d listed twice", seg.ID)
		}

		if seg.Device == "" {
			return fmt.Errorf("segment %d has no device", seg.ID)
		}

		if seg.Bytes == 0 || seg.Bytes%PageBytes != 0 {
			return fmt.Errorf("segment %d has %d bytes, not a positive multiple of %d",
				seg.ID, seg.Bytes, PageBytes)
		}

		s.byID[seg.ID] = seg
	}

	// Sorted so that anything derived from the set - log lines, metrics
	// series, the cleaner's choice of victim - is stable regardless of the
	// order racer-ctrl happened to emit.
	sort.Slice(s.Segments, func(i, j int) bool { return s.Segments[i].ID < s.Segments[j].ID })

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

// CatalogDevice is the device holding the catalog extent.
func (s *Set) CatalogDevice() (Catalog, error) {
	if s.Catalog.Device == "" {
		return Catalog{}, ErrNoCatalog
	}

	if s.Catalog.Bytes == 0 || s.Catalog.Bytes%BlockBytes != 0 {
		return Catalog{}, fmt.Errorf("catalog has %d bytes, not a positive multiple of %d",
			s.Catalog.Bytes, BlockBytes)
	}

	return s.Catalog, nil
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

	offset = addr.ByteOffset()
	length = addr.Span()

	if offset+length > seg.Bytes {
		return "", 0, 0, fmt.Errorf(
			"blob at pages %d..%d runs past segment %d, which holds %d pages",
			addr.PageOffset, uint64(addr.PageOffset)+uint64(addr.PageCount), seg.ID, seg.Pages())
	}

	return seg.Device, offset, length, nil
}
