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

// A writer that takes a reservation and then dies is the failure that matters:
// its slots stay empty, every reader in the cluster stops there, and the
// catalog never publishes anything again. Abandon is the way back out.
func TestAbandonRetiresAHole(t *testing.T) {
	dev, a := ready(t)

	b := open(t, dev)

	res, err := a.Reserve(4, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// The ingest fails somewhere between here and Append.
	if err := a.Abandon(res); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	// A later, unrelated ingest publishes normally.
	next, err := a.ReserveRecords(1)
	if err != nil {
		t.Fatalf("ReserveRecords: %v", err)
	}

	if err := a.Append(next, []Record{{Type: RecordChain, Key: digest(7), Ref: digest(8)}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if b.Len() != 1 {
		t.Fatalf("a reader resolved %d keys past the abandoned slots, want 1", b.Len())
	}

	if _, ok := b.index.entries[digest(7)]; !ok {
		t.Fatal("a record appended after an abandoned reservation was never seen")
	}

	// The voids themselves resolve to nothing, including under the zero key
	// they carry.
	if _, ok := b.index.entries[Digest{}]; ok {
		t.Fatal("a void record was folded into the index")
	}
}

// The pages a reservation claimed cannot go back on the cursor, which only
// moves forward, so they have to be booked as dead for the cleaner instead.
func TestAbandonBooksThePagesDead(t *testing.T) {
	_, s := ready(t)

	res, err := s.Reserve(3, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := s.Abandon(res); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("got %d segments, want 1", len(entries))
	}

	if want := uint64(3) * segment.PageBytes; entries[0].DeadBytes != want {
		t.Fatalf("segment holds %d dead bytes, want %d", entries[0].DeadBytes, want)
	}

	if entries[0].LiveBytes != 0 {
		t.Fatalf("an abandoned reservation counted %d live bytes", entries[0].LiveBytes)
	}
}

// Append writes one block at a time and can fail partway, so Abandon has to
// tolerate finding real records in slots it was going to void. Those records
// are published and true, and voiding them would unpublish a live layer.
func TestAbandonLeavesWrittenRecordsAlone(t *testing.T) {
	dev, a := ready(t)

	b := open(t, dev)

	res, err := a.ReserveRecords(2)
	if err != nil {
		t.Fatalf("ReserveRecords: %v", err)
	}

	if err := a.Append(res, []Record{
		{Type: RecordChain, Key: digest(1), Ref: digest(2)},
		{Type: RecordChain, Key: digest(3), Ref: digest(4)},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if err := a.Abandon(res); err != nil {
		t.Fatalf("Abandon over written slots: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if b.Len() != 2 {
		t.Fatalf("Abandon retired %d of 2 published records", 2-b.Len())
	}
}

// clock replaces time.Now with one the test drives, and returns a function
// that moves it forward.
func clock(t *testing.T) func(time.Duration) {
	t.Helper()

	previous := now
	at := time.Unix(1700000000, 0)
	now = func() time.Time { return at }

	t.Cleanup(func() { now = previous })

	return func(d time.Duration) { at = at.Add(d) }
}

// A node that is evicted or loses power between Reserve and Append never gets
// to call Abandon. The hole it leaves is the same hole, and every node in the
// cluster stops at it forever, so the catalog has to heal without an operator.
func TestRepairRetiresACrashedWritersHole(t *testing.T) {
	advance := clock(t)
	dev, a := ready(t)

	b := open(t, dev)

	// The writer takes a reservation and is never heard from again.
	if _, err := a.Reserve(4, 2); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Another node publishes behind the hole.
	next, err := a.ReserveRecords(1)
	if err != nil {
		t.Fatalf("ReserveRecords: %v", err)
	}

	if err := a.Append(next, []Record{{Type: RecordChain, Key: digest(7), Ref: digest(8)}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if b.Len() != 0 {
		t.Fatalf("a reader saw %d keys past an unwritten slot", b.Len())
	}

	// Before the grace expires the writer is only assumed to be slow.
	if voided, err := b.Repair(time.Hour); err != nil || voided != 0 {
		t.Fatalf("Repair before the grace = %d, %v", voided, err)
	}

	advance(3 * time.Hour)

	voided, err := b.Repair(time.Hour)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}

	if voided != 2 {
		t.Fatalf("Repair retired %d slots, want 2", voided)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync after repair: %v", err)
	}

	if _, ok := b.index.entries[digest(7)]; !ok {
		t.Fatal("a record behind the repaired hole is still invisible")
	}

	// The repair is written through, so every other node sees it too.
	c := open(t, dev)
	if _, ok := c.index.entries[digest(7)]; !ok {
		t.Fatal("a node opening the catalog fresh still stops at the hole")
	}
}

// Only the hole observed when the clock started is repaired. Slots reserved
// after that have not had their grace period, and voiding them would destroy
// the work of a writer that is behaving perfectly well.
func TestRepairLeavesLaterReservationsAlone(t *testing.T) {
	advance := clock(t)
	dev, a := ready(t)

	b := open(t, dev)

	if _, err := a.Reserve(4, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	advance(3 * time.Hour)

	// A second writer starts just before the repair runs.
	fresh, err := a.Reserve(4, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	voided, err := b.Repair(time.Hour)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}

	if voided != 1 {
		t.Fatalf("Repair retired %d slots, want only the aged one", voided)
	}

	// The live writer's slot is untouched, so it can still publish.
	if err := a.Append(fresh, []Record{{Type: RecordChain, Key: digest(9), Ref: digest(10)}}); err != nil {
		t.Fatalf("Append after a repair: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, ok := b.index.entries[digest(9)]; !ok {
		t.Fatal("Repair voided a slot a live writer went on to fill")
	}
}

// Every node watches the same hole and reaches the same conclusion, so the
// repair has to be safe to run from all of them. The second one through finds
// the slots taken and writes nothing.
func TestRepairIsSafeToRaceAndIdempotent(t *testing.T) {
	advance := clock(t)
	dev, a := ready(t)

	b := open(t, dev)

	if _, err := a.Reserve(4, 2); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if _, err := a.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	advance(3 * time.Hour)

	if _, err := b.Repair(time.Hour); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	writes := dev.writes()

	if _, err := a.Repair(time.Hour); err != nil {
		t.Fatalf("a second node repairing the same hole: %v", err)
	}

	if got := dev.writes(); got != writes {
		t.Fatalf("a redundant repair wrote %d blocks, want none", got-writes)
	}

	// And repairing again after the hole is gone is a no-op.
	if _, err := b.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if voided, err := b.Repair(time.Hour); err != nil || voided != 0 {
		t.Fatalf("Repair with no hole = %d, %v", voided, err)
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

func TestReserveRollsIntoTheNextSegment(t *testing.T) {
	_, s := ready(t)

	var rolls []Roll

	s.SetRollObserver(func(r Roll) { rolls = append(rolls, r) })

	if err := s.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if _, err := s.Reserve(60, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Four pages are left, which is the whole point: the blob does not fit
	// and the reservation is the only thing that knows it.
	res, err := s.Reserve(10, 1)
	if err != nil {
		t.Fatalf("a full segment did not roll: %v", err)
	}

	if res.Segment != 2 || res.PageOffset != 0 {
		t.Fatalf("the reservation did not land in the new segment: %+v", res)
	}

	// Record slots are catalog wide, so they carry on across the roll.
	if res.FirstRecord != 1 {
		t.Fatalf("record numbering restarted: %+v", res)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if entries[0].State != SegmentSealed || entries[0].CursorPages != 60 {
		t.Fatalf("the full segment was not sealed at its cursor: %+v", entries[0])
	}

	if entries[1].State != SegmentOpen || entries[1].CursorPages != 10 {
		t.Fatalf("the new segment did not take the reservation: %+v", entries[1])
	}

	want := Roll{Sealed: 1, SealedPages: 60, Opened: 2, OpenedPages: 32}
	if len(rolls) != 1 || rolls[0] != want {
		t.Fatalf("got %+v, want one %+v", rolls, want)
	}

	// The next reservation fits, so it must not roll again.
	if _, err := s.Reserve(1, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if len(rolls) != 1 {
		t.Fatalf("a reservation that fits rolled anyway: %+v", rolls)
	}
}

func TestReserveRollsPastASegmentTooSmall(t *testing.T) {
	_, s := ready(t)

	// A segment that cannot hold the blob is no use to this reservation, and
	// opening it would seal what is left of segment 1 for nothing.
	if err := s.AddSegment(2, 4); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if err := s.AddSegment(3, 64); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if _, err := s.Reserve(64, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	res, err := s.Reserve(10, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if res.Segment != 3 {
		t.Fatalf("the reservation landed in segment %d, want 3: %+v", res.Segment, res)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if entries[1].State != SegmentEmpty {
		t.Fatalf("the segment that was skipped was spent anyway: %+v", entries[1])
	}
}

func TestReserveFullWhenNoSegmentHoldsTheBlob(t *testing.T) {
	_, s := ready(t)

	if err := s.AddSegment(2, 4); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if _, err := s.Reserve(64, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if _, err := s.Reserve(10, 1); !errors.Is(err, ErrFull) {
		t.Fatalf("got %v, want ErrFull", err)
	}

	// A blob nothing can hold must not cost the cluster its remaining
	// capacity, so the failed reservation leaves the segments as they were.
	if got := s.Superblock().OpenSegment; got != 1 {
		t.Fatalf("open segment %d, want the full segment 1 still open", got)
	}

	entries, err := s.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if entries[1].State != SegmentEmpty {
		t.Fatalf("the small segment was sealed for a blob it could not hold: %+v", entries[1])
	}

	res, err := s.Reserve(4, 1)
	if err != nil {
		t.Fatalf("a blob that still fits was refused: %v", err)
	}

	if res.Segment != 2 {
		t.Fatalf("the reservation landed in segment %d, want 2", res.Segment)
	}
}

func TestReserveRecordsDoesNotRoll(t *testing.T) {
	_, s := ready(t)

	if err := s.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	if _, err := s.Reserve(64, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// A record-only claim names no bytes, so a full segment is nothing to it.
	// Rolling here would spend a segment to write a tombstone.
	if _, err := s.ReserveRecords(1); err != nil {
		t.Fatalf("ReserveRecords: %v", err)
	}

	if got := s.Superblock().OpenSegment; got != 1 {
		t.Fatalf("a record-only reservation rolled to segment %d", got)
	}
}

func TestReserveRollConverges(t *testing.T) {
	noSleep(t)

	dev, a := ready(t)

	if err := a.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	client := dev.client()

	b, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := a.Reserve(64, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	var winner Reservation

	// Both nodes see the same full segment at the same instant. A rolls while
	// B is part way through its own roll, which is the case that must not end
	// with two sealed segments or a reservation into a segment nobody opened.
	client.onWrite = func() {
		var err error

		winner, err = a.Reserve(6, 1)
		if err != nil {
			t.Errorf("Reserve: %v", err)
		}
	}

	loser, err := b.Reserve(5, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if client.conflicts == 0 {
		t.Fatal("the test did not actually produce a conflict")
	}

	if winner.Segment != 2 || loser.Segment != 2 {
		t.Fatalf("the two nodes rolled apart: winner %+v, loser %+v", winner, loser)
	}

	if loser.PageOffset != winner.PageOffset+winner.PageCount {
		t.Fatalf("reservations overlap: winner %+v, loser %+v", winner, loser)
	}

	entries, err := b.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	// One roll happened, not two: the old segment keeps every page it handed
	// out and exactly one segment is open.
	if entries[0].State != SegmentSealed || entries[0].CursorPages != 64 {
		t.Fatalf("sealing lost pages that were handed out: %+v", entries[0])
	}

	open := 0

	for _, e := range entries {
		if e.State == SegmentOpen {
			open++
		}
	}

	if open != 1 {
		t.Fatalf("%d segments are open: %+v", open, entries)
	}
}

func TestReserveRollSealsAtTheTrueCursor(t *testing.T) {
	noSleep(t)

	dev, a := ready(t)

	if err := a.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	client := dev.client()

	b, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := a.Reserve(60, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// B decides to roll against a cursor at page 60, and A fills the last four
	// pages and rolls first. B's seal then lands on a segment another node has
	// already sealed further along, and writing B's stale cursor back would
	// hide the pages A handed out: the Account for a blob living in them would
	// be refused for accounting past the cursor.
	client.onWrite = func() {
		if _, err := a.Reserve(4, 1); err != nil {
			t.Errorf("Reserve: %v", err)
		}

		if _, err := a.Reserve(10, 1); err != nil {
			t.Errorf("Reserve: %v", err)
		}
	}

	if _, err := b.Reserve(10, 1); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	entries, err := b.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if entries[0].State != SegmentSealed || entries[0].CursorPages != 64 {
		t.Fatalf("sealing moved the cursor backwards: %+v", entries[0])
	}

	// The cursor has to hold up what is accounted against it.
	if err := b.Account(1, 64*segment.PageBytes, 0); err != nil {
		t.Fatalf("Account: %v", err)
	}
}

func TestSetOpenSegmentResumesAPartialRoll(t *testing.T) {
	_, s := ready(t)

	if err := s.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	// A rollover that died between marking the successor open and publishing
	// it in the superblock. Refusing the entry would strand the segment's
	// whole capacity for as long as the catalog lives.
	block, slot, err := s.Superblock().segmentLocation(2)
	if err != nil {
		t.Fatalf("segmentLocation: %v", err)
	}

	s.io.Lock()
	err = s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, _ bool) (SegmentEntry, error) {
		existing.State = SegmentOpen

		return existing, nil
	})
	s.io.Unlock()

	if err != nil {
		t.Fatalf("mergeSegmentBlock: %v", err)
	}

	if err := s.SetOpenSegment(2); err != nil {
		t.Fatalf("a half finished rollover stranded segment 2: %v", err)
	}

	res, err := s.Reserve(1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if res.Segment != 2 || res.PageOffset != 0 {
		t.Fatalf("appends did not move to the resumed segment: %+v", res)
	}
}

func TestSetOpenSegmentRetriesALostWrite(t *testing.T) {
	noSleep(t)

	dev, a := ready(t)

	client := dev.client()

	b, err := Open(client)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := b.AddSegment(2, 32); err != nil {
		t.Fatalf("AddSegment: %v", err)
	}

	// Reservations from other nodes land on the superblock all the time, so a
	// rollover that gave up on the first lost compare-and-swap would rarely
	// finish on a busy cluster.
	client.onWrite = func() {
		if _, err := a.Reserve(3, 1); err != nil {
			t.Errorf("Reserve: %v", err)
		}
	}

	if err := b.SetOpenSegment(2); err != nil {
		t.Fatalf("SetOpenSegment: %v", err)
	}

	if client.conflicts == 0 {
		t.Fatal("the test did not actually produce a conflict")
	}

	entries, err := b.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	// The reservation that slipped in is sealed into the segment it came
	// from, not dropped by the roll that read the cursor before it moved.
	if entries[0].State != SegmentSealed || entries[0].CursorPages != 3 {
		t.Fatalf("the roll sealed away a reservation: %+v", entries[0])
	}

	if entries[1].State != SegmentOpen {
		t.Fatalf("the new segment did not open: %+v", entries[1])
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

// A node's catalog can be replaced while an ingest is holding a reservation.
// Page offsets and record slots mean nothing outside the catalog they came
// from, and nothing on the read path re-checks a blob's digest, so applying one
// to the wrong catalog would quietly publish a record naming unrelated bytes.
func TestForeignReservationIsRefused(t *testing.T) {
	_, a := ready(t)
	_, b := ready(t)

	res, err := a.Reserve(1, 2)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	before := b.Superblock()

	segsBefore, err := b.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	addr := res.Address(1234)

	records := []Record{
		{
			Type: RecordBlob, Key: digest(1), Ref: digest(0xaa),
			Segment: addr.Segment, PageOffset: addr.PageOffset,
			PageCount: addr.PageCount, ByteLength: addr.ByteLength,
		},
		{Type: RecordChain, Key: digest(2), Ref: digest(1)},
	}

	if err := b.Append(res, records); !errors.Is(err, ErrForeignReservation) {
		t.Fatalf("Append to a foreign catalog = %v, want ErrForeignReservation", err)
	}

	// Abandon is refused too. The hole is real, but it is in a's catalog, and
	// a's Repair is what retires it.
	if err := b.Abandon(res); !errors.Is(err, ErrForeignReservation) {
		t.Fatalf("Abandon on a foreign catalog = %v, want ErrForeignReservation", err)
	}

	if got := b.Superblock(); got != before {
		t.Errorf("superblock moved: %+v, want %+v", got, before)
	}

	segsAfter, err := b.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}

	if len(segsAfter) != len(segsBefore) {
		t.Fatalf("segment count %d, want %d", len(segsAfter), len(segsBefore))
	}

	for i := range segsAfter {
		if segsAfter[i] != segsBefore[i] {
			t.Errorf("segment %d moved: %+v, want %+v", i, segsAfter[i], segsBefore[i])
		}
	}

	// The reservation is still good in the catalog that issued it.
	if err := a.Append(res, records); err != nil {
		t.Fatalf("Append to the issuing catalog: %v", err)
	}
}

// A Reservation literal carries no owner, which is what keeps it usable as a
// test fixture and as a zero value.
func TestUnownedReservationIsAccepted(t *testing.T) {
	_, s := ready(t)

	res, err := s.Reserve(1, 1)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Rebuilt from the exported fields only, as a caller outside the package
	// can construct it.
	unowned := Reservation{
		Segment:     res.Segment,
		PageOffset:  res.PageOffset,
		PageCount:   res.PageCount,
		FirstRecord: res.FirstRecord,
		RecordCount: res.RecordCount,
		Generation:  res.Generation,
	}

	if err := s.Abandon(unowned); err != nil {
		t.Fatalf("Abandon an unowned reservation: %v", err)
	}
}
