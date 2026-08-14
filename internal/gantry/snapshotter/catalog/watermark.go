// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"time"
)

// NodeBytes is the length of the node identifier a watermark carries. It is a
// truncated sha256 of the node name rather than the name itself, so an entry is
// fixed width and a long node name cannot overflow a slot.
const NodeBytes = 16

// NodeKey is a node's identity in the watermark table.
type NodeKey [NodeBytes]byte

// NodeKeyFor derives a node's watermark identity from its name.
func NodeKeyFor(name string) NodeKey {
	sum := sha256.Sum256([]byte(name))

	var key NodeKey

	copy(key[:], sum[:NodeBytes])

	return key
}

// String renders a node key for logs and errors.
func (k NodeKey) String() string { return hex.EncodeToString(k[:]) }

// IsZero reports an unclaimed slot.
func (k NodeKey) IsZero() bool { return k == NodeKey{} }

// Watermark is one node's report of how far it has caught up.
//
// It exists for exactly one question, which the cleaner cannot answer any other
// way: has every node stopped resolving blobs to the segment I am about to
// destroy? A trimmed 4 MiB page reads back as zeroes rather than an error, so a
// node still holding an EROFS mount into a trimmed segment would serve garbage
// silently. The gate has to be positive evidence that every node has moved on,
// not the absence of evidence that one has not.
//
// The table lives in the catalog rather than in Kubernetes because the catalog
// is already the cluster-wide serialization point every node writes to, and
// because a gate that depended on the API server would make container start
// depend on it too.
type Watermark struct {
	// Node identifies the reporting node.
	Node NodeKey

	// Generation is the highest superblock generation this node has folded
	// into its index and pruned its mounts against. A record written at or
	// below it can no longer be resolved to a location this node has not
	// seen superseded.
	Generation uint64

	// Updated is when the node last refreshed the entry. A node that stops
	// refreshing has gone, and its slot is reclaimable; that is the only
	// liveness signal the table has, and it is why the entry carries a time
	// at all.
	Updated time.Time
}

// Expired reports whether the entry has gone stale, meaning the node behind it
// is presumed gone and no longer holds the cleaner up.
func (w Watermark) Expired(now time.Time, grace time.Duration) bool {
	return w.Updated.IsZero() || now.Sub(w.Updated) > grace
}

// MarshalTo writes the watermark into dst, which must be WatermarkEntryBytes
// long.
//
// Wire layout:
//
//	 0..16  node key
//	16..24  generation
//	24..32  updated, Unix seconds
func (w Watermark) MarshalTo(dst []byte) error {
	if len(dst) != WatermarkEntryBytes {
		return fmt.Errorf("watermark buffer is %d bytes, want %d", len(dst), WatermarkEntryBytes)
	}

	clear(dst)

	copy(dst[0:NodeBytes], w.Node[:])
	binary.LittleEndian.PutUint64(dst[16:24], w.Generation)

	if !w.Updated.IsZero() {
		binary.LittleEndian.PutUint64(dst[24:32], uint64(w.Updated.Unix())) //nolint:gosec // time since 1970 is positive
	}

	return nil
}

// UnmarshalWatermark decodes one watermark table entry. An all-zero slot
// decodes to the zero watermark with no error.
func UnmarshalWatermark(src []byte) (Watermark, error) {
	if len(src) != WatermarkEntryBytes {
		return Watermark{}, fmt.Errorf("watermark buffer is %d bytes, want %d",
			len(src), WatermarkEntryBytes)
	}

	if allZero(src) {
		return Watermark{}, nil
	}

	w := Watermark{Generation: binary.LittleEndian.Uint64(src[16:24])}
	copy(w.Node[:], src[0:NodeBytes])

	if secs := binary.LittleEndian.Uint64(src[24:32]); secs != 0 {
		w.Updated = time.Unix(int64(secs), 0).UTC() //nolint:gosec // written from a positive Unix time
	}

	if w.Node.IsZero() {
		return Watermark{}, fmt.Errorf("%w: watermark entry with no node", ErrCorrupt)
	}

	return w, nil
}

// MarshalWatermarkBlock packs watermarks into one block at their given slots.
// As with the segment table, slots may be filled out of order and with gaps, so
// that a node refreshing its own entry rewrites the block without needing every
// other node's entry in hand.
func MarshalWatermarkBlock(slots map[int]Watermark) ([]byte, error) {
	block := make([]byte, BlockBytes)
	binary.LittleEndian.PutUint16(block[4:6], Version)

	for slot, w := range slots {
		if slot < 0 || slot >= WatermarksPerBlock {
			return nil, fmt.Errorf("watermark slot %d is outside the %d that fit in a block",
				slot, WatermarksPerBlock)
		}

		off := watermarkPageHeaderBytes + slot*WatermarkEntryBytes
		if err := w.MarshalTo(block[off : off+WatermarkEntryBytes]); err != nil {
			return nil, err
		}
	}

	binary.LittleEndian.PutUint32(block[0:4], crc32.Checksum(block[4:], castagnoli))

	return block, nil
}

// UnmarshalWatermarkBlock decodes a watermark table block into its occupied
// slots.
func UnmarshalWatermarkBlock(block []byte) (map[int]Watermark, error) {
	if len(block) != BlockBytes {
		return nil, fmt.Errorf("watermark block is %d bytes, want %d", len(block), BlockBytes)
	}

	if allZero(block) {
		return map[int]Watermark{}, nil
	}

	want := binary.LittleEndian.Uint32(block[0:4])
	if got := crc32.Checksum(block[4:], castagnoli); got != want {
		return nil, fmt.Errorf("%w: watermark block checksum %08x, want %08x", ErrCorrupt, got, want)
	}

	if version := binary.LittleEndian.Uint16(block[4:6]); version > Version {
		return nil, fmt.Errorf("%w: watermark block version %d, this build reads %d",
			ErrCorrupt, version, Version)
	}

	slots := make(map[int]Watermark)

	for i := range WatermarksPerBlock {
		off := watermarkPageHeaderBytes + i*WatermarkEntryBytes

		w, err := UnmarshalWatermark(block[off : off+WatermarkEntryBytes])
		if err != nil {
			return nil, fmt.Errorf("watermark slot %d: %w", i, err)
		}

		if w.Node.IsZero() {
			continue
		}

		slots[i] = w
	}

	return slots, nil
}

// DefaultWatermarkGrace is how long a node's entry stands after its last
// refresh before the cleaner stops waiting for it.
//
// It is long relative to the refresh interval and short relative to how long a
// reclaim is allowed to take. Too short and a node that is merely slow gets its
// mounts trimmed out from under it; too long and one drained node stalls every
// reclaim in the cluster.
const DefaultWatermarkGrace = 10 * time.Minute

// Watermarks reads the whole node watermark table.
func (s *Store) Watermarks() ([]Watermark, error) {
	s.io.Lock()
	defer s.io.Unlock()

	sb := s.Superblock()

	return s.watermarksLocked(sb)
}

func (s *Store) watermarksLocked(sb Superblock) ([]Watermark, error) {
	buf := make([]byte, BlockBytes)
	out := make([]Watermark, 0, sb.WatermarkCapacity())

	for i := range uint64(sb.WatermarkBlocks) {
		block := sb.WatermarkBlockBase() + i

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return nil, fmt.Errorf("read watermark block %d: %w", block, err)
		}

		slots, err := UnmarshalWatermarkBlock(buf)
		if err != nil {
			return nil, fmt.Errorf("watermark block %d: %w", block, err)
		}

		for _, w := range slots {
			out = append(out, w)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].Node[:], out[j].Node[:]) < 0
	})

	return out, nil
}

// SetWatermark records how far this node has caught up, claiming a slot for it
// the first time.
//
// A node keeps its slot for as long as it keeps refreshing. When the table is
// full, an entry that has not been refreshed within grace is taken over, which
// is the only way a decommissioned node's slot ever comes back. Taking one over
// is safe precisely because the entry is stale: a node that is not refreshing
// is not resolving blobs either, and if it comes back it claims a fresh slot
// and starts from a generation it has actually seen.
func (s *Store) SetWatermark(node NodeKey, generation uint64, grace time.Duration) error {
	if node.IsZero() {
		return errors.New("catalog: watermark needs a node")
	}

	s.io.Lock()
	defer s.io.Unlock()

	sb := s.Superblock()
	if sb.WatermarkBlocks == 0 {
		return fmt.Errorf("%w: catalog has no watermark table", ErrCorrupt)
	}

	block, slot, err := s.findWatermarkSlot(sb, node, grace)
	if err != nil {
		return err
	}

	return s.mergeWatermarkBlock(block, slot, func(existing Watermark, present bool) (Watermark, error) {
		// Re-read under the block's own compare-and-swap. Another node may
		// have taken the slot between the scan and here, in which case this
		// one gives it up rather than overwriting a live entry.
		if present && existing.Node != node && !existing.Expired(now(), grace) {
			return Watermark{}, fmt.Errorf("%w: watermark slot taken by node %s", ErrConflict, existing.Node)
		}

		// A watermark only ever moves forward. A node that restarts and
		// re-reads from an older generation must not appear to have gone
		// backwards, because the cleaner has already counted it as past.
		if existing.Node == node && existing.Generation > generation {
			generation = existing.Generation
		}

		return Watermark{Node: node, Generation: generation, Updated: now()}, nil
	})
}

// findWatermarkSlot locates this node's slot, or picks one it may claim.
func (s *Store) findWatermarkSlot(sb Superblock, node NodeKey, grace time.Duration) (uint64, int, error) {
	buf := make([]byte, BlockBytes)

	free := -1
	at := now()

	for i := range uint64(sb.WatermarkBlocks) {
		block := sb.WatermarkBlockBase() + i

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return 0, 0, fmt.Errorf("read watermark block %d: %w", block, err)
		}

		slots, err := UnmarshalWatermarkBlock(buf)
		if err != nil {
			return 0, 0, fmt.Errorf("watermark block %d: %w", block, err)
		}

		for j := range WatermarksPerBlock {
			existing, present := slots[j]

			if present && existing.Node == node {
				return block, j, nil
			}

			// The first reusable slot is remembered but not taken yet: our
			// own entry may still be further along in the table, and
			// claiming a second one would count this node twice.
			if free < 0 && (!present || existing.Expired(at, grace)) {
				free = int(i)*WatermarksPerBlock + j
			}
		}
	}

	if free < 0 {
		return 0, 0, fmt.Errorf("catalog: watermark table is full at %d nodes", sb.WatermarkCapacity())
	}

	block, slot := sb.WatermarkLocation(free)

	return block, slot, nil
}

// mergeWatermarkBlock applies update to one slot of a watermark block under the
// block's own compare-and-swap, retrying on conflict. It mirrors
// mergeSegmentBlock, for the same reason: many nodes share a block and each
// only owns its own slot.
func (s *Store) mergeWatermarkBlock(
	block uint64,
	slot int,
	update func(existing Watermark, present bool) (Watermark, error),
) error {
	buf := make([]byte, BlockBytes)

	var lastErr error

	for attempt := range s.retries {
		if attempt > 0 {
			sleep(backoff(attempt))
		}

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return fmt.Errorf("read watermark block %d: %w", block, err)
		}

		slots, err := UnmarshalWatermarkBlock(buf)
		if err != nil {
			return fmt.Errorf("watermark block %d: %w", block, err)
		}

		existing, present := slots[slot]

		updated, err := update(existing, present)
		if err != nil {
			return err
		}

		slots[slot] = updated

		merged, err := MarshalWatermarkBlock(slots)
		if err != nil {
			return err
		}

		if _, err := s.vol.WriteAt(merged, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			if errors.Is(err, ErrConflict) {
				lastErr = err

				continue
			}

			return fmt.Errorf("write watermark block %d: %w", block, err)
		}

		return nil
	}

	return fmt.Errorf("catalog: watermark block %d lost %d compare-and-swaps: %w", block, s.retries, lastErr)
}

// DrainedPast reports whether every node that should be reading this catalog
// has caught up to generation, and names one that has not when they have not.
//
// This is the gate the cleaner opens before it trims, and it is answered from
// two directions. The expected set is who the cluster says is out there: a node
// in it holds the gate whether its watermark is behind, stale, or missing
// altogether, because none of those are evidence it has stopped resolving
// blobs into the segment about to be discarded. Entries not in the expected set
// are nodes the cluster no longer lists; they are waited for while their
// watermark is fresh, and written off once it goes stale, because a gate that
// waited for every node that ever existed would never open again after the
// first decommission.
//
// An empty expected set is refused rather than treated as nobody. Trimming is
// irreversible and a discarded page reads back as zeroes, so the absence of
// evidence about who is out there must not read as evidence of absence.
func (s *Store) DrainedPast(generation uint64, grace time.Duration, expect []NodeKey) (bool, NodeKey, error) {
	if len(expect) == 0 {
		return false, NodeKey{}, errors.New("catalog: no expected nodes, so nothing can be shown to have drained")
	}

	marks, err := s.Watermarks()
	if err != nil {
		return false, NodeKey{}, err
	}

	at := now()

	seen := make(map[NodeKey]Watermark, len(marks))
	for _, w := range marks {
		seen[w.Node] = w
	}

	// Expected nodes first, so the node named as holding the gate is the
	// one an operator can go and look at.
	for _, node := range expect {
		if node.IsZero() {
			return false, NodeKey{}, errors.New("catalog: expected node set contains a zero key")
		}

		w, ok := seen[node]
		if !ok || w.Expired(at, grace) || w.Generation < generation {
			return false, node, nil
		}
	}

	expected := make(map[NodeKey]struct{}, len(expect))
	for _, node := range expect {
		expected[node] = struct{}{}
	}

	for _, w := range marks {
		if _, ok := expected[w.Node]; ok {
			continue
		}

		if w.Expired(at, grace) {
			continue
		}

		if w.Generation < generation {
			return false, w.Node, nil
		}
	}

	return true, NodeKey{}, nil
}
