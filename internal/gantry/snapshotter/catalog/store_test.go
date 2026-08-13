// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"errors"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// noSleep removes the retry backoff so a contention test costs no wall time.
func noSleep(t *testing.T) {
	t.Helper()

	previous := sleep
	sleep = func(time.Duration) {}

	t.Cleanup(func() { sleep = previous })
}

const testCatalogBytes = 64 * BlockBytes

// newCatalog formats a catalog and opens one client on it.
func newCatalog(t *testing.T) (*occDevice, *Store) {
	t.Helper()

	dev := newOCCDevice()

	if err := Format(dev.client(), FormatOptions{Bytes: testCatalogBytes}); err != nil {
		t.Fatalf("Format: %v", err)
	}

	return dev, open(t, dev)
}

func open(t *testing.T, dev *occDevice) *Store {
	t.Helper()

	s, err := Open(dev.client())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return s
}

// ready returns a catalog with one 64 page segment open for appends.
func ready(t *testing.T) (*occDevice, *Store) {
	t.Helper()

	dev, s := newCatalog(t)

	if err := s.AddSegment(1, 64); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if err := s.SetOpenSegment(1); err != nil {
		t.Fatalf("SetOpenSegment: %v", err)
	}

	return dev, s
}

func TestFormatAndOpen(t *testing.T) {
	_, s := newCatalog(t)

	sb := s.Superblock()
	if sb.Generation != 1 {
		t.Fatalf("generation %d, want 1", sb.Generation)
	}

	if sb.TotalBlocks != testCatalogBytes/BlockBytes {
		t.Fatalf("total blocks %d", sb.TotalBlocks)
	}

	if sb.SegmentBlocks != DefaultSegmentBlocks {
		t.Fatalf("segment blocks %d, want %d", sb.SegmentBlocks, DefaultSegmentBlocks)
	}

	if s.Len() != 0 {
		t.Fatalf("a fresh catalog resolves %d keys", s.Len())
	}
}

func TestOpenUnformatted(t *testing.T) {
	if _, err := Open(newOCCDevice().client()); !errors.Is(err, ErrUnformatted) {
		t.Fatalf("got %v, want ErrUnformatted", err)
	}
}

func TestOpenCorrupt(t *testing.T) {
	dev := newOCCDevice()
	client := dev.client()

	block := make([]byte, BlockBytes)
	block[16] = 0x5a

	if _, err := client.ReadAt(block, 0); err != nil {
		t.Fatalf("read: %v", err)
	}

	block[16] = 0x5a

	if _, err := client.WriteAt(block, 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Open(dev.client()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestFormatRefusesExistingCatalog(t *testing.T) {
	dev, _ := newCatalog(t)

	err := Format(dev.client(), FormatOptions{Bytes: testCatalogBytes})
	if err == nil {
		t.Fatal("reformatting a live catalog was allowed; every blob in every segment would be orphaned")
	}
}

func TestFormatRefusesForeignData(t *testing.T) {
	dev := newOCCDevice()
	client := dev.client()

	block := make([]byte, BlockBytes)
	if _, err := client.ReadAt(block, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}

	block[10] = 0xff

	if _, err := client.WriteAt(block, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	if err := Format(dev.client(), FormatOptions{Bytes: testCatalogBytes}); err == nil {
		t.Fatal("formatting over unrecognized data was allowed")
	}
}

func TestReserveNeedsOpenSegment(t *testing.T) {
	_, s := newCatalog(t)

	if _, err := s.Reserve(1, 1); !errors.Is(err, ErrNoOpenSegment) {
		t.Fatalf("got %v, want ErrNoOpenSegment", err)
	}
}

func TestReserveIsMonotonic(t *testing.T) {
	_, s := ready(t)

	first, err := s.Reserve(2, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if first.Segment != 1 || first.PageOffset != 0 || first.PageCount != 2 {
		t.Fatalf("got %+v", first)
	}

	if first.FirstRecord != 0 || first.RecordCount != 2 {
		t.Fatalf("got %+v", first)
	}

	second, err := s.Reserve(3, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if second.PageOffset != 2 || second.FirstRecord != 2 {
		t.Fatalf("the second reservation overlapped the first: %+v", second)
	}

	if second.Generation <= first.Generation {
		t.Fatalf("generation did not advance: %d then %d", first.Generation, second.Generation)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if len(entries) != 1 || entries[0].CursorPages != 5 {
		t.Fatalf("segment table did not follow the cursor: %+v", entries)
	}
}

func TestReserveRejectsEmptyClaims(t *testing.T) {
	_, s := ready(t)

	if _, err := s.Reserve(1, 0); err == nil {
		t.Fatal("want an error for a reservation of no records")
	}
}

func TestReserveRecordsNeedsNoSegment(t *testing.T) {
	// A chain record onto a blob another node ingested, and a tombstone
	// retiring one, name no bytes. Retiring blobs has to keep working when
	// the volume is full, so a record-only claim must not need an open
	// segment.
	_, s := newCatalog(t)

	res, err := s.ReserveRecords(1)
	if err != nil {
		t.Fatalf("ReserveRecords: %v", err)
	}

	if res.Segment != 0 || res.PageCount != 0 {
		t.Fatalf("a record-only reservation claimed pages: %+v", res)
	}

	if err := s.Append(res, []Record{{Type: RecordChain, Key: digest(2), Ref: digest(1)}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if s.Len() != 1 {
		t.Fatalf("the record did not land: %d keys", s.Len())
	}
}

func TestReserveOutOfPages(t *testing.T) {
	_, s := ready(t)

	if _, err := s.Reserve(65, 1); !errors.Is(err, ErrFull) {
		t.Fatalf("got %v, want ErrFull", err)
	}
}

func TestReserveOutOfRecordSlots(t *testing.T) {
	dev := newOCCDevice()

	// Three blocks: superblock, one segment table block, one record block.
	if err := Format(dev.client(), FormatOptions{Bytes: 3 * BlockBytes}); err != nil {
		t.Fatalf("Format: %v", err)
	}

	s := open(t, dev)

	if err := s.AddSegment(1, 64); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if err := s.SetOpenSegment(1); err != nil {
		t.Fatalf("SetOpenSegment: %v", err)
	}

	if _, err := s.Reserve(1, RecordsPerBlock+1); !errors.Is(err, ErrFull) {
		t.Fatalf("got %v, want ErrFull", err)
	}

	if _, err := s.Reserve(1, RecordsPerBlock); err != nil {
		t.Fatalf("the last slots should still be reservable: %v", err)
	}
}

// ingest reserves and appends one layer, the way the ingester does.
func ingest(t *testing.T, s *Store, diffID, chainID Digest, pages uint32, bytes uint64) Reservation {
	t.Helper()

	res, err := s.Reserve(pages, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	addr := res.Address(bytes)

	err = s.Append(res, []Record{
		{
			Type:       RecordBlob,
			Key:        diffID,
			Ref:        digest(0xaa),
			Segment:    addr.Segment,
			PageOffset: addr.PageOffset,
			PageCount:  addr.PageCount,
			ByteLength: addr.ByteLength,
		},
		{Type: RecordChain, Key: chainID, Ref: diffID},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	return res
}

func TestAppendResolvesLocallyAndOnOtherNodes(t *testing.T) {
	dev, a := ready(t)

	b := open(t, dev)

	ingest(t, a, digest(1), digest(2), 2, 3*segment.PageBytes/2)

	// The ingesting node must serve the layer without waiting for a poll.
	blob, ok := a.Resolve(digest(2))
	if !ok {
		t.Fatal("the ingesting node cannot resolve what it just wrote")
	}

	if blob.DiffID != digest(1) || blob.Address.PageCount != 2 {
		t.Fatalf("got %+v", blob)
	}

	if _, seen := b.Resolve(digest(2)); seen {
		t.Fatal("another node resolved a layer before syncing")
	}

	changed, err := b.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if !changed {
		t.Fatal("Sync reported no change after two records were appended")
	}

	got, ok := b.Resolve(digest(2))
	if !ok {
		t.Fatal("another node cannot resolve the layer after syncing")
	}

	if got != blob {
		t.Fatalf("nodes disagree:\n got %+v\nwant %+v", got, blob)
	}

	// A steady-state poll with nothing new must report no change.
	changed, err = b.Sync()
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if changed {
		t.Fatal("an idle Sync reported a change")
	}
}

func TestSyncStopsAtAHole(t *testing.T) {
	dev, a := ready(t)

	b := open(t, dev)

	// A reservation advances the record count before the records land, so a
	// reader can see a count that runs past the last written slot.
	res, err := a.Reserve(1, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if b.Len() != 0 {
		t.Fatalf("a reader picked up %d records from an unwritten reservation", b.Len())
	}

	addr := res.Address(1234)

	err = a.Append(res, []Record{
		{
			Type: RecordBlob, Key: digest(1), Ref: digest(0xaa),
			Segment: addr.Segment, PageOffset: addr.PageOffset,
			PageCount: addr.PageCount, ByteLength: addr.ByteLength,
		},
		{Type: RecordChain, Key: digest(2), Ref: digest(1)},
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// The watermark stayed at the hole, so the records are picked up now
	// rather than skipped forever.
	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, ok := b.Resolve(digest(2)); !ok {
		t.Fatal("records that landed after the hole were skipped")
	}
}

func TestReserveRetriesOnConflict(t *testing.T) {
	noSleep(t)

	dev, a := ready(t)

	client := dev.client()

	b, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var winner Reservation

	// Slip A's reservation in between B's read of the superblock and B's
	// write of it, which is exactly the race the compare-and-swap exists for.
	client.onWrite = func() {
		winner = ingest(t, a, digest(1), digest(2), 4, 4*segment.PageBytes-10)
	}

	loser, err := b.Reserve(3, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if client.conflicts == 0 {
		t.Fatal("the test did not actually produce a conflict")
	}

	if winner.PageOffset != 0 || winner.PageCount != 4 {
		t.Fatalf("winner %+v", winner)
	}

	if loser.PageOffset != winner.PageOffset+winner.PageCount {
		t.Fatalf("reservations overlap: winner %+v, loser %+v", winner, loser)
	}

	if loser.FirstRecord != uint64(winner.RecordCount) {
		t.Fatalf("record slots overlap: winner %+v, loser %+v", winner, loser)
	}
}

func TestAppendRejectsWrongRecordCount(t *testing.T) {
	_, s := ready(t)

	res, err := s.Reserve(1, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	err = s.Append(res, []Record{{Type: RecordChain, Key: digest(1), Ref: digest(2)}})
	if err == nil {
		t.Fatal("want an error when the records do not fill the reservation")
	}
}

func TestAppendRejectsInvalidRecord(t *testing.T) {
	_, s := ready(t)

	res, err := s.Reserve(1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := s.Append(res, []Record{{Type: RecordChain, Key: digest(1)}}); err == nil {
		t.Fatal("want an error for a chain record with no ref")
	}
}

func TestAppendSharesABlock(t *testing.T) {
	dev, a := ready(t)

	b := open(t, dev)

	// Two ingesters holding adjacent slots in the same block: each has to
	// preserve the other's record when it rewrites the block.
	first, err := a.Reserve(1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	second, err := b.Reserve(1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := b.Append(second, []Record{{Type: RecordChain, Key: digest(2), Ref: digest(20)}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := a.Append(first, []Record{{Type: RecordChain, Key: digest(1), Ref: digest(10)}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	c := open(t, dev)

	if c.Len() != 2 {
		t.Fatalf("a shared block lost a record: %d of 2 survived", c.Len())
	}
}

func TestAddSegmentIsIdempotent(t *testing.T) {
	_, s := newCatalog(t)

	if err := s.AddSegment(1, 64); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if err := s.SetOpenSegment(1); err != nil {
		t.Fatalf("SetOpenSegment: %v", err)
	}

	if _, err := s.Reserve(5, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The operator re-announcing a segment must not reset its accounting.
	if err := s.AddSegment(1, 64); err != nil {
		t.Fatalf("re-adding a segment: %v", err)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if len(entries) != 1 || entries[0].CursorPages != 5 {
		t.Fatalf("re-adding a segment moved its cursor: %+v", entries)
	}

	if err := s.AddSegment(1, 128); err == nil {
		t.Fatal("a segment silently changed size")
	}
}

func TestAddSegmentRejects(t *testing.T) {
	_, s := newCatalog(t)

	if err := s.AddSegment(1, 0); err == nil {
		t.Fatal("want an error for a segment with no capacity")
	}

	if err := s.AddSegment(0, 64); err == nil {
		t.Fatal("want an error for segment id 0")
	}

	if err := s.AddSegment(SegmentsPerBlock+1, 64); err == nil {
		t.Fatal("want an error for a segment past the table")
	}
}

func TestSetOpenSegmentSealsThePrevious(t *testing.T) {
	_, s := ready(t)

	if _, err := s.Reserve(7, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := s.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if err := s.SetOpenSegment(2); err != nil {
		t.Fatalf("SetOpenSegment: %v", err)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}

	// The sealed segment keeps the authoritative cursor the superblock held.
	if entries[0].State != SegmentSealed || entries[0].CursorPages != 7 {
		t.Fatalf("sealing lost the cursor: %+v", entries[0])
	}

	if entries[1].State != SegmentOpen || entries[1].CursorPages != 0 {
		t.Fatalf("the new segment did not open empty: %+v", entries[1])
	}

	res, err := s.Reserve(1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if res.Segment != 2 || res.PageOffset != 0 {
		t.Fatalf("appends did not move to the new segment: %+v", res)
	}
}

func TestSetOpenSegmentRejects(t *testing.T) {
	_, s := ready(t)

	if err := s.SetOpenSegment(1); err != nil {
		t.Fatalf("re-opening the open segment should be a no-op: %v", err)
	}

	if err := s.SetOpenSegment(9); err == nil {
		t.Fatal("want an error for a segment that is not in the table")
	}

	if err := s.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if err := s.SetOpenSegment(2); err != nil {
		t.Fatalf("SetOpenSegment: %v", err)
	}

	// Segment 1 is sealed now, and reopening it would append over live data.
	if err := s.SetOpenSegment(1); err == nil {
		t.Fatal("a sealed segment was reopened for appends")
	}
}

func TestAccount(t *testing.T) {
	_, s := ready(t)

	if _, err := s.Reserve(4, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := s.Account(1, 4*segment.PageBytes, 0); err != nil {
		t.Fatalf("Account: %v", err)
	}

	// A layer going out of use moves bytes from live to dead, which is what
	// tells the cleaner this segment is worth draining.
	if err := s.Account(1, -segment.PageBytes, segment.PageBytes); err != nil {
		t.Fatalf("Account: %v", err)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	e := entries[0]
	if e.LiveBytes != 3*segment.PageBytes || e.DeadBytes != segment.PageBytes {
		t.Fatalf("got %+v", e)
	}

	// The fraction is against the segment's whole capacity, which is what the
	// cleaner ranks victims by: 3 live pages of 64.
	if got := e.LiveFraction(); got < 0.046 || got > 0.047 {
		t.Fatalf("live fraction %v, want about 0.0469", got)
	}

	// Accounting that goes negative is a bug in the caller, not a number to
	// wrap around.
	if err := s.Account(1, -10*segment.PageBytes, 0); err == nil {
		t.Fatal("live bytes went negative")
	}

	if err := s.Account(2, 1, 0); err == nil {
		t.Fatal("want an error accounting to a segment that is not in the table")
	}
}
