// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// Volume is the catalog extent, byte addressed. In production it is an
// O_DIRECT handle on the RACER device carrying the catalog's OCC extent; in
// tests it is a fake that reproduces OCC's compare-and-swap semantics.
//
// Every access is a whole 4 KiB block at a block-aligned offset, because that
// is the unit RACER versions and therefore the unit a compare-and-swap covers.
type Volume interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
}

// ErrFull reports that the catalog has no room left: either its record slots
// are used up, or no segment can hold the blob. A full open segment is not
// enough on its own, because a reservation rolls into an empty segment first.
// Ingest stops; reads are unaffected.
var ErrFull = errors.New("catalog: full")

// ErrNoOpenSegment reports that no segment is accepting appends. The operator
// has to add capacity before ingest resumes.
var ErrNoOpenSegment = errors.New("catalog: no open segment")

// sleep is time.Sleep, indirected so tests can run the retry paths without
// spending real time in them.
var sleep = time.Sleep

// now is time.Now, indirected so tests can age a hole without waiting for it.
var now = time.Now

// DefaultRetries is how many times an optimistic write is retried before
// giving up. A caller that exhausts this is contending with many other
// ingesters, and failing is fine: ingest is off the container start path and
// the layer is simply ingested later.
const DefaultRetries = 16

// DefaultHoleGrace is how long a hole in the record slots has to persist
// before a reader concludes its writer is never coming back and retires it.
//
// It is deliberately far longer than any legitimate writer takes. The longest
// gap between a reservation and its records is a layer build plus a multi-GiB
// aligned write plus the read-back verify, and voiding a hole that a live
// writer is about to fill costs that writer its work. Waiting an hour to
// recover from a crash nobody has noticed yet is the cheaper mistake.
const DefaultHoleGrace = time.Hour

// Store is the catalog on a device.
//
// Locking has two layers and the reason matters. RACER's OCC guard is the
// version *this node* last read a page at, which is process-wide state rather
// than per-goroutine: if two goroutines read block 0, then both write it, the
// second read re-arms the guard and both writes can land, silently handing the
// same pages to two ingesters. So every device access, read included, is
// serialized by a single mutex. The in-memory index has its own reader lock so
// that the container start path, which only resolves, never waits behind I/O.
type Store struct {
	vol     Volume
	retries int

	// io serializes all device access. See the type comment: this is a
	// correctness requirement of OCC, not a convenience.
	io sync.Mutex

	state   sync.RWMutex
	sb      Superblock
	applied uint64
	index   *Index

	// hole is where the last Sync stopped short, and since when. See Repair.
	hole      uint64
	holeEnd   uint64
	holeSince time.Time

	// skew spreads repairs out. Every node in the cluster watches the same
	// hole and reaches the same conclusion at the same moment; without this
	// they would all pile onto the same block's compare-and-swap.
	skew float64

	// onRoll reports segment rollovers, for the daemon's log. It is read
	// without synchronization, so it has to be set before the store is
	// handed to anything else and never changed after.
	onRoll func(Roll)
}

// SetRollObserver registers a callback for segment rollovers.
//
// It must be called before the store is shared with other goroutines. The
// callback runs on the goroutine that took the reservation, with no catalog
// lock held, and must not call back into the store.
func (s *Store) SetRollObserver(fn func(Roll)) { s.onRoll = fn }

// FormatOptions describes a catalog to be created.
type FormatOptions struct {
	// Bytes is the catalog extent's size. It is rounded down to a whole
	// number of blocks.
	Bytes uint64

	// SegmentBlocks is how many blocks the segment table occupies, and so
	// how many segments the image volume can ever hold. It cannot be changed
	// afterwards without moving every record, so it is set generously: one
	// block already covers 127 segments, which at the default 16 GiB segment
	// is 2 TiB, more than a node's export slot budget allows it to map.
	SegmentBlocks uint32
}

// DefaultSegmentBlocks covers 127 segments, which exceeds the number of image
// devices a node can export at once.
const DefaultSegmentBlocks = 1

// Format writes a fresh catalog. It refuses to run against a device that
// already holds one, because reformatting orphans every blob in every segment
// with no way to find them again.
func Format(vol Volume, opts FormatOptions) error {
	block := make([]byte, BlockBytes)

	if _, err := vol.ReadAt(block, 0); err != nil {
		return fmt.Errorf("read superblock: %w", err)
	}

	if !allZero(block) {
		if _, err := UnmarshalSuperblock(block); err == nil {
			return errors.New("catalog: device already holds a catalog")
		}

		return errors.New("catalog: device holds data that is not a catalog")
	}

	segmentBlocks := opts.SegmentBlocks
	if segmentBlocks == 0 {
		segmentBlocks = DefaultSegmentBlocks
	}

	sb := Superblock{
		Generation:    1,
		SegmentBlocks: segmentBlocks,
		TotalBlocks:   opts.Bytes / BlockBytes,
	}

	if err := sb.Validate(); err != nil {
		return err
	}

	// The segment table is zeroed before the superblock is published, so a
	// reader that finds a valid superblock never reads a table left over
	// from whatever was on the device before.
	//
	// Each block is read first. Under OCC a write with no prior read of the
	// page has no guard to compare against and is rejected, so reading is
	// not an optimization here; it is what makes the write land. Blocks that
	// are already zero are left alone.
	zero := make([]byte, BlockBytes)
	scratch := make([]byte, BlockBytes)

	for b := uint64(1); b <= uint64(segmentBlocks); b++ {
		off := int64(b * BlockBytes) //nolint:gosec // block index is bounded by the extent size

		if _, err := vol.ReadAt(scratch, off); err != nil {
			return fmt.Errorf("read segment table block %d: %w", b, err)
		}

		if allZero(scratch) {
			continue
		}

		if _, err := vol.WriteAt(zero, off); err != nil {
			return fmt.Errorf("zero segment table block %d: %w", b, err)
		}
	}

	if err := sb.MarshalTo(block); err != nil {
		return err
	}

	if _, err := vol.WriteAt(block, 0); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	return nil
}

// Open reads an existing catalog and loads every record into memory.
func Open(vol Volume) (*Store, error) {
	s := &Store{vol: vol, retries: DefaultRetries, index: NewIndex(), skew: rand.Float64()} //nolint:gosec // scheduling jitter, not a secret

	if _, err := s.Sync(); err != nil {
		return nil, err
	}

	return s, nil
}

// Superblock returns the last superblock read.
func (s *Store) Superblock() Superblock {
	s.state.RLock()
	defer s.state.RUnlock()

	return s.sb
}

// Resolve maps a chainID to its blob. It touches no device: this is the
// container start path, and a lookup here has to be a map read.
func (s *Store) Resolve(chainID Digest) (Blob, bool) {
	s.state.RLock()
	defer s.state.RUnlock()

	return s.index.Resolve(chainID)
}

// Blob maps a layer's diffID to its blob without going through a chainID. The
// ingester uses it to avoid re-ingesting a layer another node already did.
func (s *Store) Blob(diffID Digest) (Blob, bool) {
	s.state.RLock()
	defer s.state.RUnlock()

	return s.index.Blob(diffID)
}

// Len is how many keys the index resolves.
func (s *Store) Len() int {
	s.state.RLock()
	defer s.state.RUnlock()

	return s.index.Len()
}

// Sync re-reads the superblock and folds in any records appended since the
// last call, reporting whether anything changed.
//
// This is the poll on the read side. The superblock is one 4 KiB read and is
// hot in RACER's cache, and only the record blocks past the last applied
// record are read, so a steady-state poll costs one block.
func (s *Store) Sync() (bool, error) {
	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return false, err
	}

	s.state.RLock()
	applied := s.applied
	previous := s.sb
	s.state.RUnlock()

	if sb.Generation == previous.Generation && sb.RecordCount == applied {
		return false, nil
	}

	records, read, err := s.readRecords(sb, applied)
	if err != nil {
		return false, err
	}

	s.state.Lock()
	s.sb = sb
	s.index.Apply(records...)
	s.applied = read
	s.noteHole(read, sb.RecordCount)
	s.state.Unlock()

	return true, nil
}

// noteHole records where this Sync stopped short, so Repair can tell a writer
// that is slow from one that is gone. It runs under s.state held for writing.
//
// The end of the hole is pinned at the record count observed the first time it
// was seen and is never extended. Slots reserved after that have not had their
// grace period yet, and repairing them early would race a writer that is
// behaving perfectly well.
func (s *Store) noteHole(read, count uint64) {
	if read >= count {
		s.hole, s.holeEnd, s.holeSince = 0, 0, time.Time{}

		return
	}

	if s.hole == read && !s.holeSince.IsZero() {
		return
	}

	s.hole, s.holeEnd, s.holeSince = read, count, now()
}

// readRecords reads records [from, sb.RecordCount) and reports how far it got.
//
// It can legitimately stop short. A reservation advances the record count
// before the records themselves are written, so a reader can see a count that
// runs past the last written slot. Stopping at the first gap and leaving the
// watermark there means the next Sync picks the records up once they land,
// rather than skipping them forever.
func (s *Store) readRecords(sb Superblock, from uint64) ([]Record, uint64, error) {
	if from > sb.RecordCount {
		// The catalog was reformatted or replaced underneath us. Start over
		// rather than believing a watermark from a different catalog.
		from = 0
	}

	var (
		records []Record
		block   = make([]byte, BlockBytes)
		at      = from
	)

	for at < sb.RecordCount {
		index, _ := sb.RecordLocation(at)

		if _, err := s.vol.ReadAt(block, int64(index*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return nil, 0, fmt.Errorf("read record block %d: %w", index, err)
		}

		slots, err := UnmarshalRecordBlock(block)
		if err != nil {
			return nil, 0, fmt.Errorf("record block %d: %w", index, err)
		}

		// Walk this block's slots in order and stop at the first hole.
		for at < sb.RecordCount {
			nextIndex, nextSlot := sb.RecordLocation(at)
			if nextIndex != index {
				break
			}

			r, ok := slots[nextSlot]
			if !ok {
				return records, at, nil
			}

			records = append(records, r)
			at++
		}
	}

	return records, at, nil
}

func (s *Store) readSuperblock() (Superblock, error) {
	block := make([]byte, BlockBytes)

	if _, err := s.vol.ReadAt(block, 0); err != nil {
		return Superblock{}, fmt.Errorf("read superblock: %w", err)
	}

	return UnmarshalSuperblock(block)
}

// Reservation is an exclusive claim on a range of pages in the open segment
// and a range of record slots, taken in one compare-and-swap.
type Reservation struct {
	Segment     uint32
	PageOffset  uint32
	PageCount   uint32
	FirstRecord uint64
	RecordCount int

	// Generation is the catalog generation the reservation was taken at, and
	// is what the records written under it carry.
	Generation uint64
}

// Address is where the reservation's blob goes.
func (r Reservation) Address(byteLength uint64) segment.Address {
	return segment.Address{
		Segment:    r.Segment,
		PageOffset: r.PageOffset,
		PageCount:  r.PageCount,
		ByteLength: byteLength,
	}
}

// ReserveRecords claims record slots without claiming any pages.
//
// Records that name no bytes still need slots: a chain record pointing at a
// blob another node ingested, and a tombstone retiring one, are both pure index
// edits. They are deliberately allowed when no segment is open, because
// retiring blobs is exactly what has to keep working when the volume is full.
func (s *Store) ReserveRecords(records int) (Reservation, error) {
	return s.Reserve(0, records)
}

// Reserve claims pages in the open segment and record slots, atomically.
//
// This is the one place allocation is serialized cluster-wide, and it is one
// block write. On conflict it re-reads and retries with jittered backoff; a
// conflict means another node reserved first, so retrying is exactly right.
//
// A request the open segment cannot hold rolls the catalog into the next empty
// segment rather than failing. Rolling here rather than from a control loop is
// what makes it happen at all: the reservation is the only code in the cluster
// that knows a segment is full, and it knows it while holding the lock that
// makes moving the cursor safe.
func (s *Store) Reserve(pages uint32, records int) (Reservation, error) {
	if records <= 0 {
		return Reservation{}, errors.New("catalog: reservation of no records")
	}

	s.io.Lock()

	// Rollovers are reported after the device lock is released: the observer
	// is a log line on the caller's goroutine, and running it under the lock
	// would let anything that touches the catalog from it deadlock. Deferred
	// before the unlock so it runs after it.
	var rolled Roll

	defer func() {
		if rolled.Opened != 0 && s.onRoll != nil {
			s.onRoll(rolled)
		}
	}()
	defer s.io.Unlock()

	var lastErr error

	for attempt := range s.retries {
		if attempt > 0 {
			sleep(backoff(attempt))
		}

		sb, err := s.readSuperblock()
		if err != nil {
			return Reservation{}, err
		}

		if pages > 0 {
			if sb.OpenSegment == 0 {
				return Reservation{}, ErrNoOpenSegment
			}

			if sb.OpenFreePages() < pages {
				// One rollover per reservation. If the segment we just
				// opened is full too, another node is filling it as fast
				// as we roll, and sealing segment after segment to chase
				// it would empty the volume. Ingest is off the container
				// start path, so the layer is simply ingested later.
				if rolled.Opened != 0 {
					return Reservation{}, fmt.Errorf("%w: open segment %d has %d of %d pages free",
						ErrFull, sb.OpenSegment, sb.OpenFreePages(), pages)
				}

				roll, err := s.rollOpenSegmentLocked(sb, pages)
				if err != nil {
					// Another node moved appends first. Its segment is
					// almost certainly the one we would have chosen, so
					// re-read and use it.
					if errors.Is(err, ErrConflict) {
						lastErr = err

						continue
					}

					return Reservation{}, err
				}

				rolled = roll

				continue
			}
		}

		if sb.RecordCount+uint64(records) > sb.RecordCapacity() {
			return Reservation{}, fmt.Errorf("%w: %d of %d record slots used",
				ErrFull, sb.RecordCount, sb.RecordCapacity())
		}

		next := sb
		next.Generation++
		next.OpenCursorPages += pages
		next.RecordCount += uint64(records)

		res := Reservation{
			FirstRecord: sb.RecordCount,
			RecordCount: records,
			Generation:  next.Generation,
		}

		if pages > 0 {
			res.Segment = sb.OpenSegment
			res.PageOffset = sb.OpenCursorPages
			res.PageCount = pages
		}

		if err := s.writeSuperblock(next); err != nil {
			if errors.Is(err, ErrConflict) {
				lastErr = err

				continue
			}

			return Reservation{}, err
		}

		s.state.Lock()
		s.sb = next
		s.state.Unlock()

		return res, nil
	}

	return Reservation{}, fmt.Errorf("catalog: reservation lost %d compare-and-swaps: %w",
		s.retries, lastErr)
}

func (s *Store) writeSuperblock(sb Superblock) error {
	if err := sb.Validate(); err != nil {
		return err
	}

	block := make([]byte, BlockBytes)
	if err := sb.MarshalTo(block); err != nil {
		return err
	}

	if _, err := s.vol.WriteAt(block, 0); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}

	return nil
}

// Append writes records into the slots a reservation claimed.
//
// The blob bytes must already be durable in the segment before this is called.
// A record is a promise that the bytes are there, and a reader that acts on it
// mounts them immediately.
//
// Record blocks are shared: two ingesters can hold slots in the same block, so
// each write is a read-modify-write under the block's own compare-and-swap.
// Retrying is safe because the retry re-reads the other writer's record and
// re-places only its own slots.
func (s *Store) Append(res Reservation, records []Record) error {
	if len(records) != res.RecordCount {
		return fmt.Errorf("catalog: %d records for a reservation of %d", len(records), res.RecordCount)
	}

	byBlock, err := s.placeRecords(res, records)
	if err != nil {
		return err
	}

	s.io.Lock()
	defer s.io.Unlock()

	for index, slots := range byBlock {
		if _, err := s.mergeRecordBlock(index, slots, false); err != nil {
			return err
		}
	}

	// Fold our own records in immediately rather than waiting for a poll, so
	// the node that ingested a layer can serve it without a round trip.
	s.state.Lock()
	for _, slots := range byBlock {
		for _, r := range slots {
			s.index.Apply(r)
		}
	}
	s.state.Unlock()

	return nil
}

// Abandon retires a reservation whose records will never be written.
//
// Reserve publishes the record count before the records exist, and readers
// deliberately stop at the first empty slot so a slow writer's records are
// picked up rather than skipped. That is correct for a writer that is merely
// slow and fatal for one that gives up: the empty slots stay empty, every node
// stops there forever, and nothing appended after them is ever visible again.
// One failed ingest would freeze the whole cluster's catalog.
//
// So every path that takes a reservation and then fails has to come back here.
// Abandon fills the slots with voids, which resolve to nothing and let readers
// walk past, and books any pages the reservation claimed as dead so the cleaner
// reclaims them later. The pages themselves are not returned to the cursor:
// the cursor only moves forward, which is what makes the reservation a single
// compare-and-swap.
//
// A slot that already holds a real record is left alone. Append can fail after
// writing some of its blocks, and those records are published and true.
func (s *Store) Abandon(res Reservation) error {
	if res.RecordCount <= 0 {
		return nil
	}

	records := make([]Record, res.RecordCount)
	for i := range records {
		records[i] = Record{Type: RecordVoid, Generation: res.Generation}
	}

	byBlock, err := s.placeRecords(res, records)
	if err != nil {
		return err
	}

	s.io.Lock()

	for index, slots := range byBlock {
		if _, err := s.mergeRecordBlock(index, slots, true); err != nil {
			s.io.Unlock()

			return err
		}
	}

	s.io.Unlock()

	if res.PageCount == 0 {
		return nil
	}

	dead := int64(res.PageCount) * int64(segment.PageBytes)
	if err := s.Account(res.Segment, 0, dead); err != nil {
		return fmt.Errorf("catalog: account %d abandoned pages in segment %d: %w",
			res.PageCount, res.Segment, err)
	}

	return nil
}

// Repair retires a hole whose writer never came back, reporting how many
// slots it voided.
//
// Abandon covers a writer that fails and lives to say so. This covers the one
// that does not: a node that is evicted, OOM-killed, or loses power between
// taking a reservation and appending to it leaves empty slots that every node
// in the cluster stops at forever. Nothing appended after them is ever visible
// again, so the catalog has to be able to heal itself without an operator.
//
// The rule is time, not ownership. A hole that has not moved for grace is
// declared dead and filled with voids, by whichever node notices first; the
// rest find the slots taken and write nothing. Any node can do this because
// the repair is a compare-and-swap on the record block and voids resolve to
// nothing, so the worst case of two nodes agreeing is one wasted read.
//
// A writer that comes back after grace and appends its records finds the slots
// taken and fails, and the pages it wrote are stranded: booked live in the
// segment with no record naming them. That is why grace is an hour. Stranding
// a blob costs one layer's worth of space until the cleaner runs; leaving the
// hole costs the cluster its catalog.
func (s *Store) Repair(grace time.Duration) (int, error) {
	s.state.RLock()
	at, end, since := s.hole, s.holeEnd, s.holeSince
	skew, generation := s.skew, s.sb.Generation
	s.state.RUnlock()

	if since.IsZero() || end <= at {
		return 0, nil
	}

	// Spread the herd over the second half of the window so a thousand nodes
	// do not converge on the same block at the same instant.
	if now().Sub(since) < grace+time.Duration(float64(grace)*skew) {
		return 0, nil
	}

	// The voids carry the generation this node last observed. Nothing reads
	// it, because the index skips voids entirely, but every record on the
	// device has to be valid on its own terms and generation zero is how a
	// corrupt record reads.
	res := Reservation{FirstRecord: at, RecordCount: int(end - at), Generation: generation} //nolint:gosec // bounded by the record capacity

	records := make([]Record, res.RecordCount)
	for i := range records {
		records[i] = Record{Type: RecordVoid}
	}

	byBlock, err := s.placeRecords(res, records)
	if err != nil {
		return 0, err
	}

	s.io.Lock()
	defer s.io.Unlock()

	voided := 0

	for index, slots := range byBlock {
		filled, err := s.mergeRecordBlock(index, slots, true)
		if err != nil {
			return 0, fmt.Errorf("catalog: repair record block %d: %w", index, err)
		}

		voided += filled
	}

	// Leave the watermark where it is. The next Sync reads the voids back
	// off the device, which is what proves the repair landed; believing it
	// landed because we asked for it would skip records another node wrote
	// into the same range while we were repairing it.
	s.state.Lock()
	s.hole, s.holeEnd, s.holeSince = 0, 0, time.Time{}
	s.state.Unlock()

	return voided, nil
}

// Hole reports the first unwritten record slot a reader is stopped at and how
// long it has been stopped there. Reported for metrics; zero means no hole.
func (s *Store) Hole() (uint64, time.Duration) {
	s.state.RLock()
	defer s.state.RUnlock()

	if s.holeSince.IsZero() {
		return 0, 0
	}

	return s.hole, now().Sub(s.holeSince)
}

// placeRecords maps a reservation's records onto the blocks and slots they
// belong in, validating each one on the way.
func (s *Store) placeRecords(res Reservation, records []Record) (map[uint64]map[int]Record, error) {
	byBlock := make(map[uint64]map[int]Record)

	s.state.RLock()
	sb := s.sb
	s.state.RUnlock()

	for i, r := range records {
		r.Generation = res.Generation

		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}

		index, slot := sb.RecordLocation(res.FirstRecord + uint64(i)) //nolint:gosec // i is bounded by len(records)

		if byBlock[index] == nil {
			byBlock[index] = make(map[int]Record)
		}

		byBlock[index][slot] = r
	}

	return byBlock, nil
}

// mergeRecordBlock writes mine into the given block under its own
// compare-and-swap, reporting how many slots it actually filled.
//
// A slot that already holds exactly the record we were going to write is left
// alone, so a retry that follows a write which landed costs nothing.
//
// When keepTaken is set, a slot holding a *different* record is also left as it
// is instead of being reported as a collision. That is for Abandon and Repair,
// which both have to tolerate finding real records in slots they meant to void.
func (s *Store) mergeRecordBlock(index uint64, mine map[int]Record, keepTaken bool) (int, error) {
	block := make([]byte, BlockBytes)

	var lastErr error

	for attempt := range s.retries {
		if attempt > 0 {
			sleep(backoff(attempt))
		}

		if _, err := s.vol.ReadAt(block, int64(index*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return 0, fmt.Errorf("read record block %d: %w", index, err)
		}

		slots, err := UnmarshalRecordBlock(block)
		if err != nil {
			return 0, fmt.Errorf("record block %d: %w", index, err)
		}

		filled := 0

		for slot, r := range mine {
			if existing, taken := slots[slot]; taken {
				if existing == r {
					continue
				}

				if keepTaken {
					continue
				}

				return 0, fmt.Errorf("catalog: record slot %d in block %d is already taken by %s %s",
					slot, index, existing.Type, existing.Key)
			}

			slots[slot] = r
			filled++
		}

		if filled == 0 {
			// Every slot we were asked to fill already holds what we
			// wanted there. Writing the block back would burn a
			// compare-and-swap on the block every other ingester in the
			// cluster is contending for.
			return 0, nil
		}

		merged, err := MarshalRecordBlock(slots)
		if err != nil {
			return 0, err
		}

		if _, err := s.vol.WriteAt(merged, int64(index*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			if errors.Is(err, ErrConflict) {
				lastErr = err

				continue
			}

			return 0, fmt.Errorf("write record block %d: %w", index, err)
		}

		return filled, nil
	}

	return 0, fmt.Errorf("catalog: record block %d lost %d compare-and-swaps: %w", index, s.retries, lastErr)
}

// backoff is exponential with full jitter, capped. Ingest is off the container
// start path, so waiting is cheap; a thundering herd of ingesters retrying in
// lockstep is not.
func backoff(attempt int) time.Duration {
	const (
		base    = 2 * time.Millisecond
		ceiling = 250 * time.Millisecond
	)

	d := base << min(attempt, 10)
	if d > ceiling {
		d = ceiling
	}

	return time.Duration(rand.Int64N(int64(d))) + time.Millisecond //nolint:gosec // jitter, not a secret
}
