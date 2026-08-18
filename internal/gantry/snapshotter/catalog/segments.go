// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// segmentLocation is the block and slot a segment's table entry occupies.
// Slots are addressed by segment id rather than packed, so updating one
// segment's accounting never moves another's entry and so an entry can be
// found without scanning.
func (s Superblock) segmentLocation(id uint32) (block uint64, slot int, err error) {
	if id == 0 {
		return 0, 0, errors.New("catalog: segment id 0 is reserved")
	}

	index := uint64(id - 1)

	block = 1 + index/SegmentsPerBlock
	if block > uint64(s.SegmentBlocks) {
		return 0, 0, fmt.Errorf("catalog: segment %d is past the %d the table was formatted for",
			id, uint64(s.SegmentBlocks)*SegmentsPerBlock)
	}

	return block, int(index % SegmentsPerBlock), nil
}

// Segments reads the whole segment table.
//
// The open segment's entry is patched from the superblock on the way out. The
// superblock holds the authoritative cursor, because a reservation has to move
// the cursor and the record count in one write; the table's copy is accounting
// that catches up lazily, and returning the stale number here would make the
// cleaner's free-space arithmetic wrong.
func (s *Store) Segments() ([]SegmentEntry, error) {
	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return nil, err
	}

	return s.segmentsLocked(sb)
}

// segmentsLocked is Segments against a superblock the caller has already read,
// with s.io held. Reserve needs the table while it holds the device lock, and
// the lock is not reentrant.
func (s *Store) segmentsLocked(sb Superblock) ([]SegmentEntry, error) {
	var entries []SegmentEntry

	block := make([]byte, BlockBytes)

	for b := uint64(1); b <= uint64(sb.SegmentBlocks); b++ {
		if _, err := s.vol.ReadAt(block, int64(b*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return nil, fmt.Errorf("read segment block %d: %w", b, err)
		}

		slots, err := UnmarshalSegmentBlock(block)
		if err != nil {
			return nil, fmt.Errorf("segment block %d: %w", b, err)
		}

		for _, e := range slots {
			if e.ID == sb.OpenSegment {
				e.State = SegmentOpen
				e.CursorPages = sb.OpenCursorPages
				e.TotalPages = sb.OpenTotalPages
			}

			entries = append(entries, e)
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	return entries, nil
}

// AddSegment records a segment the operator has added to the image volume. It
// does not open it; SetOpenSegment does that.
//
// epoch is the extent's tombstone epoch as published now. It is recorded rather
// than assumed to be zero so that a catalog formatted onto a volume whose
// extents have already been collected once does not immediately believe every
// segment is due a reclaim.
func (s *Store) AddSegment(id, totalPages, epoch uint32) error {
	if totalPages == 0 {
		return errors.New("catalog: segment with no capacity")
	}

	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return err
	}

	block, slot, err := sb.segmentLocation(id)
	if err != nil {
		return err
	}

	return s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, present bool) (SegmentEntry, error) {
		if present {
			if existing.TotalPages != totalPages {
				return SegmentEntry{}, fmt.Errorf(
					"catalog: segment %d is already known with %d pages, not %d",
					id, existing.TotalPages, totalPages)
			}

			// Idempotent: the operator re-announcing a segment it already
			// added must not reset its accounting.
			return existing, nil
		}

		return SegmentEntry{ID: id, State: SegmentEmpty, TotalPages: totalPages, Epoch: epoch}, nil
	})
}

// ErrSegmentState reports a lifecycle transition that did not happen because
// the segment was not where the caller believed it was. It is not a failure:
// another node got there first, and the caller should re-read and reconsider.
var ErrSegmentState = errors.New("catalog: segment is not in the expected state")

// SetSegmentState moves a segment from one lifecycle state to another under the
// table block's compare-and-swap, so that only one node can drive a given
// transition.
//
// repoint is written when it is non-zero, which is how the cleaner records the
// generation a reader has to have passed before the segment's old contents stop
// being reachable. It is never cleared here: the value has to survive the whole
// of Cleaning and Draining, and it is the reclaim that retires it.
//
// The open segment is refused outright. Its cursor lives in the superblock, so
// moving it out of Open without going through openSegmentLocked would strand
// the allocation point.
func (s *Store) SetSegmentState(id uint32, from, to SegmentState, repoint uint64) error {
	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return err
	}

	if sb.OpenSegment == id {
		return fmt.Errorf("%w: segment %d is open", ErrSegmentState, id)
	}

	block, slot, err := sb.segmentLocation(id)
	if err != nil {
		return err
	}

	return s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, present bool) (SegmentEntry, error) {
		if !present {
			return SegmentEntry{}, fmt.Errorf("catalog: segment %d is not in the table", id)
		}

		if existing.State != from {
			return SegmentEntry{}, fmt.Errorf("%w: segment %d is %s, not %s",
				ErrSegmentState, id, existing.State, from)
		}

		existing.State = to

		if repoint != 0 {
			existing.RepointGeneration = repoint
		}

		return existing, nil
	})
}

// ReclaimSegment returns a drained segment to service once the control plane
// has collected its extent.
//
// epoch is what the node's image device map publishes for the extent now. The
// reclaim only happens when that has run past the epoch the catalog recorded,
// because that is the single piece of evidence RACER offers that the pages are
// really gone: an epoch advance is what makes a trimmed page's slot free and
// its address writable again, and nothing else reports it.
//
// The entry is reset rather than deleted. Its capacity has not changed, and a
// segment that vanished from the table would have to be re-adopted before it
// could be opened.
func (s *Store) ReclaimSegment(id, epoch uint32) (bool, error) {
	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return false, err
	}

	if sb.OpenSegment == id {
		return false, fmt.Errorf("%w: segment %d is open", ErrSegmentState, id)
	}

	block, slot, err := sb.segmentLocation(id)
	if err != nil {
		return false, err
	}

	reclaimed := false

	err = s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, present bool) (SegmentEntry, error) {
		reclaimed = false

		if !present {
			return SegmentEntry{}, fmt.Errorf("catalog: segment %d is not in the table", id)
		}

		if epoch <= existing.Epoch {
			return existing, nil
		}

		// An epoch that moved while the segment was not draining is not this
		// catalog's doing, but the pages are gone either way and pretending
		// otherwise would hand a reader an address that reads as zeroes.
		reclaimed = true

		return SegmentEntry{
			ID:         id,
			State:      SegmentEmpty,
			TotalPages: existing.TotalPages,
			Epoch:      epoch,
		}, nil
	})
	if err != nil {
		return false, err
	}

	return reclaimed, nil
}

// Roll describes a segment rollover: the segment that was sealed because it
// could not hold the next blob, and the one appends moved to.
//
// It exists to be reported. Sealing a segment is the moment a chunk of the
// image volume stops being writable until the cleaner reclaims it, and an
// operator watching capacity should not have to infer it from a superblock.
type Roll struct {
	Sealed      uint32
	SealedPages uint32
	Opened      uint32
	OpenedPages uint32
}

// SetOpenSegment makes id the segment reservations append to.
//
// The previously open segment is sealed first, and its authoritative cursor is
// written back into the table on the way, which is the point at which the
// table's copy stops being an approximation.
func (s *Store) SetOpenSegment(id uint32) error {
	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return err
	}

	_, err = s.openSegmentLocked(sb.OpenSegment, id)

	return err
}

// openSegmentLocked seals outgoing and makes id the open segment, with s.io
// held. Reserve rolls the open segment from inside its own retry loop, which
// already holds the lock, and the lock is not reentrant.
//
// outgoing is the open segment the caller decided to replace, and it is
// rechecked on every attempt. If another node has moved appends somewhere else
// in the meantime, the caller's decision was made against a superblock that no
// longer exists and re-deciding is the caller's job: sealing whatever happens
// to be open now would throw away a segment another node had just opened.
//
// The superblock write is retried like a reservation's is. Rollovers now
// happen whenever a segment fills rather than once on an empty catalog, and
// they contend with every reservation in the cluster, so losing the
// compare-and-swap is ordinary rather than exceptional.
func (s *Store) openSegmentLocked(outgoing, id uint32) (Roll, error) {
	var lastErr error

	for attempt := range s.retries {
		if attempt > 0 {
			sleep(backoff(attempt))
		}

		sb, err := s.readSuperblock()
		if err != nil {
			return Roll{}, err
		}

		if sb.OpenSegment == id {
			// Already there: either it always was, or another node rolled
			// to the same segment, or our own write landed and we are
			// retrying a conflict we did not actually lose.
			return Roll{}, nil
		}

		if sb.OpenSegment != outgoing {
			return Roll{}, fmt.Errorf("%w: segment %d is open for appends, not %d",
				ErrConflict, sb.OpenSegment, outgoing)
		}

		entry, block, slot, err := s.successor(sb, id)
		if err != nil {
			return Roll{}, err
		}

		roll := Roll{Opened: id, OpenedPages: entry.TotalPages}

		// Seal the outgoing segment before the superblock stops tracking
		// its cursor, otherwise the authoritative cursor is lost. On a
		// retry this runs again against the cursor the superblock holds
		// now, which is how a reservation that landed in between is not
		// sealed away.
		if sb.OpenSegment != 0 {
			if err := s.sealLocked(sb); err != nil {
				return Roll{}, err
			}

			roll.Sealed = sb.OpenSegment
			roll.SealedPages = sb.OpenCursorPages
		}

		err = s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, _ bool) (SegmentEntry, error) {
			existing.State = SegmentOpen

			return existing, nil
		})
		if err != nil {
			return Roll{}, err
		}

		next := sb
		next.Generation++
		next.OpenSegment = id
		next.OpenCursorPages = 0
		next.OpenTotalPages = entry.TotalPages

		if err := s.writeSuperblock(next); err != nil {
			if errors.Is(err, ErrConflict) {
				lastErr = err

				continue
			}

			return Roll{}, err
		}

		s.state.Lock()
		s.sb = next
		s.state.Unlock()

		return roll, nil
	}

	return Roll{}, fmt.Errorf("catalog: opening segment %d lost %d compare-and-swaps: %w",
		id, s.retries, lastErr)
}

// successor reads the table entry of a segment that is about to be opened and
// checks it can be, returning where the entry lives so the caller can write it
// back without locating it again.
func (s *Store) successor(sb Superblock, id uint32) (SegmentEntry, uint64, int, error) {
	block, slot, err := sb.segmentLocation(id)
	if err != nil {
		return SegmentEntry{}, 0, 0, err
	}

	entry, err := s.readSegmentEntry(block, slot)
	if err != nil {
		return SegmentEntry{}, 0, 0, err
	}

	if entry.ID != id {
		return SegmentEntry{}, 0, 0, fmt.Errorf("catalog: segment %d is not in the table", id)
	}

	// An entry marked open that the superblock does not name is a rollover
	// that died between marking the successor and publishing it. Nothing was
	// ever appended to it, so it is picked up where it was left rather than
	// refused: refusing would strand the segment's whole capacity for good,
	// and a crash between two writes is not rare enough to pay that for.
	if entry.State != SegmentEmpty && entry.State != SegmentOpen {
		return SegmentEntry{}, 0, 0, fmt.Errorf(
			"catalog: segment %d is %s, only an empty segment can be opened", id, entry.State)
	}

	if entry.CursorPages != 0 {
		return SegmentEntry{}, 0, 0, fmt.Errorf("catalog: segment %d is %s but its cursor is at page %d",
			id, entry.State, entry.CursorPages)
	}

	return entry, block, slot, nil
}

// sealLocked writes the superblock's authoritative cursor into the open
// segment's table entry and marks it sealed.
func (s *Store) sealLocked(sb Superblock) error {
	block, slot, err := sb.segmentLocation(sb.OpenSegment)
	if err != nil {
		return err
	}

	return s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, present bool) (SegmentEntry, error) {
		if !present {
			return SegmentEntry{}, fmt.Errorf("catalog: open segment %d is not in the table", sb.OpenSegment)
		}

		existing.State = SegmentSealed
		existing.TotalPages = sb.OpenTotalPages

		// A cursor never moves backwards. Two nodes rolling at once both seal,
		// and the one whose superblock write loses read its cursor earlier than
		// the one that wins; taking the larger of the two keeps a losing sealer
		// from hiding pages the winner had already handed out, which would make
		// the later Account for a blob in those pages fail validation.
		if sb.OpenCursorPages > existing.CursorPages {
			existing.CursorPages = sb.OpenCursorPages
		}

		return existing, nil
	})
}

// rollOpenSegmentLocked seals the open segment because it cannot hold pages
// more, and opens the successor every node would choose.
//
// The successor is chosen from the catalog's own segment table rather than
// from this node's view of which devices it has mapped, so that two nodes
// rolling at the same instant pick the same segment instead of sealing one
// each. A segment in the table is part of the shared image volume by
// definition; a node that cannot map it has a local staging problem, and its
// own reconcile is what fixes that.
//
// A segment too small for the request is skipped rather than opened: moving
// into it would seal what is left of the open segment and still not fit.
func (s *Store) rollOpenSegmentLocked(sb Superblock, pages uint32) (Roll, error) {
	entries, err := s.segmentsLocked(sb)
	if err != nil {
		return Roll{}, err
	}

	for _, e := range entries {
		if e.ID == sb.OpenSegment || e.CursorPages != 0 || e.TotalPages < pages {
			continue
		}

		// SegmentOpen here is the partial rollover successor describes.
		if e.State != SegmentEmpty && e.State != SegmentOpen {
			continue
		}

		return s.openSegmentLocked(sb.OpenSegment, e.ID)
	}

	return Roll{}, fmt.Errorf("%w: open segment %d has %d of its %d pages free and no empty segment holds %d",
		ErrFull, sb.OpenSegment, sb.OpenFreePages(), sb.OpenTotalPages, pages)
}

// Account moves bytes between a segment's live and dead columns, which is what
// tells the cleaner which segment to pick.
//
// It is deliberately not part of the reservation's compare-and-swap. Losing an
// accounting update costs the cleaner some accuracy about which segment is
// emptiest; folding it into the allocation write would put every segment's
// table block on the allocation path.
func (s *Store) Account(id uint32, liveDelta, deadDelta int64) error {
	s.io.Lock()
	defer s.io.Unlock()

	sb, err := s.readSuperblock()
	if err != nil {
		return err
	}

	block, slot, err := sb.segmentLocation(id)
	if err != nil {
		return err
	}

	return s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, present bool) (SegmentEntry, error) {
		if !present {
			return SegmentEntry{}, fmt.Errorf("catalog: segment %d is not in the table", id)
		}

		if existing.ID == sb.OpenSegment {
			existing.CursorPages = sb.OpenCursorPages
			existing.TotalPages = sb.OpenTotalPages
		}

		live, err := addDelta(existing.LiveBytes, liveDelta)
		if err != nil {
			return SegmentEntry{}, fmt.Errorf("catalog: segment %d live bytes: %w", id, err)
		}

		dead, err := addDelta(existing.DeadBytes, deadDelta)
		if err != nil {
			return SegmentEntry{}, fmt.Errorf("catalog: segment %d dead bytes: %w", id, err)
		}

		existing.LiveBytes = live
		existing.DeadBytes = dead

		return existing, nil
	})
}

func addDelta(v uint64, delta int64) (uint64, error) {
	if delta < 0 {
		d := uint64(-delta)
		if d > v {
			return 0, fmt.Errorf("cannot subtract %d from %d", d, v)
		}

		return v - d, nil
	}

	return v + uint64(delta), nil
}

func (s *Store) readSegmentEntry(block uint64, slot int) (SegmentEntry, error) {
	buf := make([]byte, BlockBytes)

	if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
		return SegmentEntry{}, fmt.Errorf("read segment block %d: %w", block, err)
	}

	slots, err := UnmarshalSegmentBlock(buf)
	if err != nil {
		return SegmentEntry{}, fmt.Errorf("segment block %d: %w", block, err)
	}

	return slots[slot], nil
}

// mergeSegmentBlock applies update to one slot of a segment table block under
// the block's own compare-and-swap, retrying on conflict. Two writers touching
// different segments in the same block is the common case, and the retry
// re-reads the other's entry before re-applying its own change.
func (s *Store) mergeSegmentBlock(
	block uint64,
	slot int,
	update func(existing SegmentEntry, present bool) (SegmentEntry, error),
) error {
	buf := make([]byte, BlockBytes)

	var lastErr error

	for attempt := range s.retries {
		if attempt > 0 {
			sleep(backoff(attempt))
		}

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return fmt.Errorf("read segment block %d: %w", block, err)
		}

		slots, err := UnmarshalSegmentBlock(buf)
		if err != nil {
			return fmt.Errorf("segment block %d: %w", block, err)
		}

		existing, present := slots[slot]

		updated, err := update(existing, present)
		if err != nil {
			return err
		}

		if err := updated.Validate(); err != nil {
			return err
		}

		slots[slot] = updated

		merged, err := MarshalSegmentBlock(slots)
		if err != nil {
			return err
		}

		if _, err := s.vol.WriteAt(merged, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			if errors.Is(err, ErrConflict) {
				lastErr = err

				continue
			}

			return fmt.Errorf("write segment block %d: %w", block, err)
		}

		return nil
	}

	return fmt.Errorf("catalog: segment block %d lost %d compare-and-swaps: %w", block, s.retries, lastErr)
}

// SegmentBytes is a segment's capacity in bytes.
func (e SegmentEntry) SegmentBytes() uint64 { return uint64(e.TotalPages) * segment.PageBytes }
