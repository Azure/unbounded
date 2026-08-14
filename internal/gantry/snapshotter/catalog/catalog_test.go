// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// occDevice is a shared block device with RACER's OCC semantics: every block
// carries a version, and a write lands only if the writing client's last read
// of that block saw the version the block still has. Each client tracks its
// own read versions, which is what makes a second client's write conflict.
type occDevice struct {
	mu       sync.Mutex
	blocks   map[int64][]byte
	versions map[int64]uint64

	// written counts writes that landed, so a test can assert that work it
	// expects to be redundant costs the device nothing.
	written int
}

// writes reports how many writes have landed on the device.
func (d *occDevice) writes() int {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.written
}

func newOCCDevice() *occDevice {
	return &occDevice{blocks: make(map[int64][]byte), versions: make(map[int64]uint64)}
}

// client returns a handle with its own read-version state, which is what a
// separate node looks like from the device's point of view.
func (d *occDevice) client() *occClient {
	return &occClient{dev: d, read: make(map[int64]uint64)}
}

type occClient struct {
	dev  *occDevice
	read map[int64]uint64

	// conflicts counts writes rejected by the guard, so a test can assert a
	// contended reservation actually retried rather than getting lucky.
	conflicts int

	// onWrite runs once, before the next write is evaluated, and is how a
	// test slips another node's write in between this client's read and its
	// write.
	onWrite func()
}

func (c *occClient) ReadAt(p []byte, off int64) (int, error) {
	c.dev.mu.Lock()
	defer c.dev.mu.Unlock()

	if len(p) != BlockBytes || off%BlockBytes != 0 {
		return 0, errors.New("unaligned access")
	}

	block, ok := c.dev.blocks[off]
	if !ok {
		clear(p)
	} else {
		copy(p, block)
	}

	c.read[off] = c.dev.versions[off]

	return len(p), nil
}

func (c *occClient) WriteAt(p []byte, off int64) (int, error) {
	// The hook runs outside the device lock so it can drive another client.
	if c.onWrite != nil {
		hook := c.onWrite
		c.onWrite = nil

		hook()
	}

	c.dev.mu.Lock()
	defer c.dev.mu.Unlock()

	if len(p) != BlockBytes || off%BlockBytes != 0 {
		return 0, errors.New("unaligned access")
	}

	seen, read := c.read[off]
	if !read || seen != c.dev.versions[off] {
		c.conflicts++

		return 0, ErrConflict
	}

	block := make([]byte, BlockBytes)
	copy(block, p)
	c.dev.blocks[off] = block
	c.dev.versions[off]++
	c.dev.written++
	c.read[off] = c.dev.versions[off]

	return len(p), nil
}

func digest(b byte) Digest {
	var d Digest

	for i := range d {
		d[i] = b
	}

	return d
}

func TestRecordRoundTrip(t *testing.T) {
	r := Record{
		Type:       RecordBlob,
		Segment:    3,
		PageOffset: 12,
		PageCount:  2,
		ByteLength: segment.PageBytes + 4096,
		Generation: 9,
		Key:        digest(1),
		Ref:        digest(2),
	}

	buf := make([]byte, RecordBytes)
	if err := r.MarshalTo(buf); err != nil {
		t.Fatalf("MarshalTo: %v", err)
	}

	got, err := UnmarshalRecord(buf)
	if err != nil {
		t.Fatalf("UnmarshalRecord: %v", err)
	}

	if got != r {
		t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, r)
	}
}

func TestUnmarshalRecordZeroSlot(t *testing.T) {
	got, err := UnmarshalRecord(make([]byte, RecordBytes))
	if err != nil {
		t.Fatalf("UnmarshalRecord: %v", err)
	}

	if got.Type != RecordUnused {
		t.Fatalf("an unwritten slot decoded as %s, want unused", got.Type)
	}
}

func TestUnmarshalRecordDetectsCorruption(t *testing.T) {
	r := Record{Type: RecordChain, Generation: 1, Key: digest(1), Ref: digest(2)}

	buf := make([]byte, RecordBytes)
	if err := r.MarshalTo(buf); err != nil {
		t.Fatalf("MarshalTo: %v", err)
	}

	buf[40] ^= 0xff

	if _, err := UnmarshalRecord(buf); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestRecordValidate(t *testing.T) {
	bad := map[string]Record{
		"unknown type":  {Type: RecordType(9), Generation: 1, Key: digest(1)},
		"generation 0":  {Type: RecordChain, Key: digest(1), Ref: digest(2)},
		"no key":        {Type: RecordChain, Generation: 1, Ref: digest(2)},
		"chain no ref":  {Type: RecordChain, Generation: 1, Key: digest(1)},
		"blob segment0": {Type: RecordBlob, Generation: 1, Key: digest(1), PageCount: 1, ByteLength: 10},
		"blob no pages": {Type: RecordBlob, Generation: 1, Key: digest(1), Segment: 1, ByteLength: 10},
	}

	for name, r := range bad {
		t.Run(name, func(t *testing.T) {
			if err := r.Validate(); err == nil {
				t.Fatalf("want an error for %+v", r)
			}
		})
	}
}

func TestRecordBlockGaps(t *testing.T) {
	// Two ingesters holding slots 0 and 5 of the same block is the case the
	// format has to express: a dense-prefix encoding could not.
	slots := map[int]Record{
		0: {Type: RecordBlob, Generation: 1, Key: digest(1), Segment: 1, PageOffset: 0, PageCount: 1, ByteLength: 99},
		5: {Type: RecordChain, Generation: 2, Key: digest(3), Ref: digest(1)},
	}

	block, err := MarshalRecordBlock(slots)
	if err != nil {
		t.Fatalf("MarshalRecordBlock: %v", err)
	}

	got, err := UnmarshalRecordBlock(block)
	if err != nil {
		t.Fatalf("UnmarshalRecordBlock: %v", err)
	}

	if len(got) != 2 || got[0] != slots[0] || got[5] != slots[5] {
		t.Fatalf("round trip changed the block: %+v", got)
	}
}

func TestRecordBlockRejects(t *testing.T) {
	if _, err := MarshalRecordBlock(map[int]Record{RecordsPerBlock: {}}); err == nil {
		t.Fatal("want an error for a slot past the end of the block")
	}

	if _, err := UnmarshalRecordBlock(make([]byte, 10)); err == nil {
		t.Fatal("want an error for a short block")
	}

	empty, err := UnmarshalRecordBlock(make([]byte, BlockBytes))
	if err != nil {
		t.Fatalf("UnmarshalRecordBlock on an unwritten block: %v", err)
	}

	if len(empty) != 0 {
		t.Fatalf("an unwritten block decoded to %d records", len(empty))
	}

	block, err := MarshalRecordBlock(map[int]Record{0: {Type: RecordChain, Generation: 1, Key: digest(1), Ref: digest(2)}})
	if err != nil {
		t.Fatalf("MarshalRecordBlock: %v", err)
	}

	block[2000] ^= 0xff

	if _, err := UnmarshalRecordBlock(block); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestSuperblockRoundTrip(t *testing.T) {
	sb := Superblock{
		Generation:      42,
		RecordCount:     100,
		OpenSegment:     2,
		OpenCursorPages: 7,
		OpenTotalPages:  4096,
		SegmentBlocks:   1,
		WatermarkBlocks: 1,
		TotalBlocks:     4096,
	}

	block := make([]byte, BlockBytes)
	if err := sb.MarshalTo(block); err != nil {
		t.Fatalf("MarshalTo: %v", err)
	}

	got, err := UnmarshalSuperblock(block)
	if err != nil {
		t.Fatalf("UnmarshalSuperblock: %v", err)
	}

	if got != sb {
		t.Fatalf("round trip changed the superblock:\n got %+v\nwant %+v", got, sb)
	}

	if got.RecordBlockBase() != 3 {
		t.Fatalf("record base is block %d, want 3", got.RecordBlockBase())
	}

	if want := (4096 - uint64(3)) * RecordsPerBlock; got.RecordCapacity() != want {
		t.Fatalf("record capacity %d, want %d", got.RecordCapacity(), want)
	}

	blockIndex, slot := got.RecordLocation(RecordsPerBlock + 3)
	if blockIndex != 4 || slot != 3 {
		t.Fatalf("record %d is at block %d slot %d, want block 4 slot 3", RecordsPerBlock+3, blockIndex, slot)
	}
}

func TestSuperblockRejects(t *testing.T) {
	bad := map[string]Superblock{
		"generation 0":   {SegmentBlocks: 1, TotalBlocks: 100},
		"no table":       {Generation: 1, TotalBlocks: 100},
		"no room":        {Generation: 1, SegmentBlocks: 1, TotalBlocks: 2},
		"too many recs":  {Generation: 1, SegmentBlocks: 1, TotalBlocks: 3, RecordCount: 1e6},
		"cursor no open": {Generation: 1, SegmentBlocks: 1, TotalBlocks: 100, OpenCursorPages: 3},
		"cursor past":    {Generation: 1, SegmentBlocks: 1, TotalBlocks: 100, OpenSegment: 1, OpenCursorPages: 5, OpenTotalPages: 4},
	}

	for name, sb := range bad {
		t.Run(name, func(t *testing.T) {
			if err := sb.Validate(); err == nil {
				t.Fatalf("want an error for %+v", sb)
			}
		})
	}

	if _, err := UnmarshalSuperblock(make([]byte, BlockBytes)); !errors.Is(err, ErrUnformatted) {
		t.Fatal("want ErrUnformatted for a blank block")
	}

	garbage := make([]byte, BlockBytes)
	garbage[7] = 0xff

	if _, err := UnmarshalSuperblock(garbage); !errors.Is(err, ErrCorrupt) {
		t.Fatal("want ErrCorrupt for a block with no magic")
	}
}

func TestSegmentEntryRoundTrip(t *testing.T) {
	e := SegmentEntry{
		ID:          4,
		State:       SegmentSealed,
		CursorPages: 100,
		TotalPages:  4096,
		LiveBytes:   50 * segment.PageBytes,
		DeadBytes:   40 * segment.PageBytes,
	}

	block, err := MarshalSegmentBlock(map[int]SegmentEntry{3: e})
	if err != nil {
		t.Fatalf("MarshalSegmentBlock: %v", err)
	}

	slots, err := UnmarshalSegmentBlock(block)
	if err != nil {
		t.Fatalf("UnmarshalSegmentBlock: %v", err)
	}

	if len(slots) != 1 || slots[3] != e {
		t.Fatalf("round trip changed the table: %+v", slots)
	}

	if got := e.FreePages(); got != 3996 {
		t.Fatalf("free pages %d, want 3996", got)
	}

	if got := e.LiveFraction(); got < 0.012 || got > 0.013 {
		t.Fatalf("live fraction %v, want about 0.0122", got)
	}
}

func TestSegmentEntryValidate(t *testing.T) {
	bad := map[string]SegmentEntry{
		"id 0":           {TotalPages: 10},
		"cursor past":    {ID: 1, CursorPages: 11, TotalPages: 10},
		"over accounted": {ID: 1, CursorPages: 1, TotalPages: 10, LiveBytes: 10 * segment.PageBytes},
	}

	for name, e := range bad {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err == nil {
				t.Fatalf("want an error for %+v", e)
			}
		})
	}
}

func TestIndexGenerationOrdering(t *testing.T) {
	idx := NewIndex()

	blob := Record{Type: RecordBlob, Generation: 1, Key: digest(1), Segment: 1, PageOffset: 0, PageCount: 1, ByteLength: 100}
	moved := blob
	moved.Generation = 5
	moved.Segment = 2
	moved.PageOffset = 9

	// Out of order on purpose: a reader that catches up out of order still
	// has to converge on the newest record.
	idx.Apply(moved, blob)

	got, ok := idx.Blob(digest(1))
	if !ok {
		t.Fatal("blob not found")
	}

	if got.Address.Segment != 2 || got.Address.PageOffset != 9 || got.Generation != 5 {
		t.Fatalf("got %+v, want the generation 5 record", got)
	}
}

func TestIndexResolve(t *testing.T) {
	idx := NewIndex()
	idx.Apply(
		Record{Type: RecordBlob, Generation: 1, Key: digest(1), Segment: 1, PageCount: 1, ByteLength: 100, Ref: digest(7)},
		Record{Type: RecordChain, Generation: 2, Key: digest(2), Ref: digest(1)},
		Record{Type: RecordChain, Generation: 3, Key: digest(3), Ref: digest(1)},
	)

	// Two chainIDs resolving to one blob is cross-image layer dedup.
	for _, chain := range []Digest{digest(2), digest(3)} {
		blob, ok := idx.Resolve(chain)
		if !ok {
			t.Fatalf("chain %s did not resolve", chain.Short())
		}

		if blob.DiffID != digest(1) || blob.Sum != digest(7) {
			t.Fatalf("got %+v", blob)
		}
	}

	// The chain's generation, not the blob's, is what the mount depends on.
	blob, _ := idx.Resolve(digest(3))
	if blob.Generation != 3 {
		t.Fatalf("resolved at generation %d, want 3", blob.Generation)
	}

	if _, ok := idx.Resolve(digest(9)); ok {
		t.Fatal("an unknown chain resolved")
	}

	// A chain is not a blob and must not resolve as one.
	if _, ok := idx.Blob(digest(2)); ok {
		t.Fatal("a chain record resolved as a blob")
	}
}

func TestIndexDanglingChain(t *testing.T) {
	idx := NewIndex()
	idx.Apply(Record{Type: RecordChain, Generation: 1, Key: digest(2), Ref: digest(1)})

	if _, ok := idx.Resolve(digest(2)); ok {
		t.Fatal("a chain whose blob is missing resolved; it must report a miss so the caller unpacks locally")
	}
}

func TestIndexTombstone(t *testing.T) {
	idx := NewIndex()
	idx.Apply(
		Record{Type: RecordBlob, Generation: 1, Key: digest(1), Segment: 1, PageCount: 1, ByteLength: 100},
		Record{Type: RecordChain, Generation: 2, Key: digest(2), Ref: digest(1)},
	)

	idx.Apply(Record{Type: RecordTombstone, Generation: 3, Key: digest(1)})

	if _, ok := idx.Blob(digest(1)); ok {
		t.Fatal("a retired blob still resolves")
	}

	if _, ok := idx.Resolve(digest(2)); ok {
		t.Fatal("a chain onto a retired blob still resolves")
	}

	// A tombstone older than the record it names must not retire it.
	idx.Apply(Record{Type: RecordBlob, Generation: 10, Key: digest(1), Segment: 1, PageCount: 1, ByteLength: 100})
	idx.Apply(Record{Type: RecordTombstone, Generation: 4, Key: digest(1)})

	if _, ok := idx.Blob(digest(1)); !ok {
		t.Fatal("a stale tombstone retired a newer record")
	}
}

func TestParseDigest(t *testing.T) {
	d, err := ParseDigest("sha256:" + strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}

	if d[0] != 0xab || d[31] != 0xab {
		t.Fatalf("got %v", d)
	}

	if d.String() != "sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("String round trip: %s", d)
	}

	if d.Short() != "ababababababab"[:12] {
		t.Fatalf("Short is %q", d.Short())
	}

	for _, s := range []string{
		"", "sha256:", "sha512:" + strings.Repeat("ab", 32),
		"sha256:" + strings.Repeat("zz", 32), strings.Repeat("ab", 32),
	} {
		if _, err := ParseDigest(s); err == nil {
			t.Fatalf("want an error for %q", s)
		}
	}
}
