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
func (s *Store) AddSegment(id, totalPages uint32) error {
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

		return SegmentEntry{ID: id, State: SegmentEmpty, TotalPages: totalPages}, nil
	})
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

	if sb.OpenSegment == id {
		return nil
	}

	block, slot, err := sb.segmentLocation(id)
	if err != nil {
		return err
	}

	entry, err := s.readSegmentEntry(block, slot)
	if err != nil {
		return err
	}

	if entry.ID != id {
		return fmt.Errorf("catalog: segment %d is not in the table", id)
	}

	if entry.State != SegmentEmpty {
		return fmt.Errorf("catalog: segment %d is %s, only an empty segment can be opened",
			id, entry.State)
	}

	if entry.CursorPages != 0 {
		return fmt.Errorf("catalog: segment %d is empty but its cursor is at page %d",
			id, entry.CursorPages)
	}

	// Seal the outgoing segment before the superblock stops tracking its
	// cursor, otherwise the authoritative cursor is lost.
	if sb.OpenSegment != 0 {
		oldBlock, oldSlot, err := sb.segmentLocation(sb.OpenSegment)
		if err != nil {
			return err
		}

		sealed := sb

		err = s.mergeSegmentBlock(oldBlock, oldSlot, func(existing SegmentEntry, present bool) (SegmentEntry, error) {
			if !present {
				return SegmentEntry{}, fmt.Errorf("catalog: open segment %d is not in the table", sealed.OpenSegment)
			}

			existing.State = SegmentSealed
			existing.CursorPages = sealed.OpenCursorPages
			existing.TotalPages = sealed.OpenTotalPages

			return existing, nil
		})
		if err != nil {
			return err
		}
	}

	err = s.mergeSegmentBlock(block, slot, func(existing SegmentEntry, _ bool) (SegmentEntry, error) {
		existing.State = SegmentOpen

		return existing, nil
	})
	if err != nil {
		return err
	}

	next := sb
	next.Generation++
	next.OpenSegment = id
	next.OpenCursorPages = 0
	next.OpenTotalPages = entry.TotalPages

	if err := s.writeSuperblock(next); err != nil {
		return err
	}

	s.state.Lock()
	s.sb = next
	s.state.Unlock()

	return nil
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
