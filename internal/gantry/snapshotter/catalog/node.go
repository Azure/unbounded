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

// NodeBytes is the length of the node identifier the node table carries. It is
// a truncated sha256 of the node name rather than the name itself, so an entry
// is fixed width and a long node name cannot overflow its block.
const NodeBytes = 16

// NodeKey is a node's identity in the node table.
type NodeKey [NodeBytes]byte

// NodeKeyFor derives a node's catalog identity from its name.
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

// Node is one node's block in the node table: what it has caught up to, and
// what it says about the mark round in flight.
//
// A node owns its whole block. That is what makes both halves writable without
// coordination: two nodes never contend, and the two things a node publishes
// are merged into one block rather than into one another's.
//
// The table lives in the catalog rather than in Kubernetes because the catalog
// is already the cluster-wide serialization point every node writes to, and
// because a gate that depended on the API server would make container start
// depend on it too.
type Node struct {
	// Key identifies the reporting node.
	Key NodeKey

	// Generation is the highest superblock generation this node has folded
	// into its index and pruned its mounts against. A record written at or
	// below it can no longer be resolved to a location this node has not
	// seen superseded.
	//
	// It exists for exactly one question, which the cleaner cannot answer
	// any other way: has every node stopped resolving blobs to the segment
	// I am about to destroy? A trimmed 4 MiB page reads back as zeroes
	// rather than an error, so a node still holding an EROFS mount into a
	// trimmed segment would serve garbage silently. The gate has to be
	// positive evidence that every node has moved on, not the absence of
	// evidence that one has not.
	Generation uint64

	// Updated is when the node last refreshed the block. A node that stops
	// refreshing has gone, and its block is reclaimable; that is the only
	// liveness signal the table has, and it is why the entry carries a time
	// at all.
	Updated time.Time

	// Mark is this node's answer to the mark round in flight, or the zero
	// Mark if it has not answered one.
	Mark Mark
}

// Expired reports whether the block has gone stale, meaning the node behind it
// is presumed gone and no longer holds the cleaner up.
func (n Node) Expired(now time.Time, grace time.Duration) bool {
	return n.Updated.IsZero() || now.Sub(n.Updated) > grace
}

// Mark is a node's answer to one mark round.
//
// A mark round is how a blob is ever found to be unreferenced. Nothing else in
// the system knows: containerd tells each node when it drops a snapshot, but no
// node can tell whether some other node still wants the layer, and the catalog
// records only that a blob exists. So the elected cleaner names a sealed
// segment, every node answers with the blobs in it that its own snapshots
// still reference, and the union is what survives. A blob nobody claims is
// retired with a tombstone, which is what turns live bytes into dead bytes and
// makes reclamation possible at all.
type Mark struct {
	// Segment is the segment being marked. Zero means this node has not
	// answered a round.
	Segment uint32

	// Generation is the catalog generation the round was opened at. A node
	// answers only once its index has caught up to it, so that what it
	// claims is measured against the same record set the cleaner used to
	// build the ordering.
	Generation uint64

	// Ordering is the sha256 of the segment's diff IDs in the order the
	// claim bitmap indexes them. The cleaner compares it against its own
	// before it believes a single bit: two nodes that disagree about the
	// ordering would be claiming different blobs with the same bit.
	Ordering Digest

	// Claims is a bit per blob in that ordering, set when this node still
	// references the blob.
	Claims Claims
}

// Answered reports whether the node has published an answer at all.
func (m Mark) Answered() bool { return m.Segment != 0 }

// For reports whether the answer is the one a round wants: the right segment,
// at the right generation. An unanswered mark is for no round at all, which
// matters because segment 0 is not a segment and a zero mark would otherwise
// look like an answer to it.
func (m Mark) For(segment uint32, generation uint64) bool {
	return m.Answered() && m.Segment == segment && m.Generation == generation
}

// Claims is a claim bitmap, one bit per blob in a mark round's ordering.
//
// It is a bitmap rather than a list of digests because the answer has to fit in
// the node's own block, and because a fixed-width answer means a node with many
// layers cannot fail to answer at all. A blob's bit is its index in the
// ordering, which every node computes from the same record set.
type Claims []byte

// NewClaims returns a bitmap with room for every blob a segment can hold.
func NewClaims() Claims { return make(Claims, ClaimBytes) }

// Set claims the blob at index i. Indexes past the bitmap are ignored; a round
// is never opened on a segment with more blobs than bits.
func (c Claims) Set(i int) {
	if i < 0 || i/8 >= len(c) {
		return
	}

	c[i/8] |= 1 << (i % 8) //nolint:gosec // i%8 is in range
}

// Has reports whether the blob at index i is claimed.
func (c Claims) Has(i int) bool {
	if i < 0 || i/8 >= len(c) {
		return false
	}

	return c[i/8]&(1<<(i%8)) != 0 //nolint:gosec // i%8 is in range
}

// Or folds another node's claims into these. The survivors of a round are the
// union of every node's answer, never one node's view alone.
func (c Claims) Or(other Claims) {
	for i := range c {
		if i >= len(other) {
			return
		}

		c[i] |= other[i]
	}
}

// DefaultWatermarkGrace is how long a node's block stands after its last
// refresh before the cleaner stops waiting for it.
//
// It is long relative to the refresh interval and short relative to how long a
// reclaim is allowed to take. Too short and a node that is merely slow gets its
// mounts trimmed out from under it; too long and one drained node stalls every
// reclaim in the cluster.
const DefaultWatermarkGrace = 10 * time.Minute

// MarshalTo writes the node's block into dst, which must be BlockBytes long.
//
// Wire layout:
//
//	 0..4   CRC32C over bytes 4..4096
//	 4..6   version
//	 6..8   flags
//	 8..24  node key
//	24..32  updated, Unix seconds
//	32..40  watermark generation
//	40..44  mark segment
//	44..48  reserved
//	48..56  mark generation
//	56..64  reserved
//	64..96  mark ordering digest
//	96..4096 claim bitmap
func (n Node) MarshalTo(dst []byte) error {
	if len(dst) != BlockBytes {
		return fmt.Errorf("node block buffer is %d bytes, want %d", len(dst), BlockBytes)
	}

	if len(n.Mark.Claims) > ClaimBytes {
		return fmt.Errorf("claim bitmap is %d bytes, the block holds %d",
			len(n.Mark.Claims), ClaimBytes)
	}

	clear(dst)

	binary.LittleEndian.PutUint16(dst[4:6], Version)
	copy(dst[8:8+NodeBytes], n.Key[:])

	if !n.Updated.IsZero() {
		binary.LittleEndian.PutUint64(dst[24:32], uint64(n.Updated.Unix())) //nolint:gosec // time since 1970 is positive
	}

	binary.LittleEndian.PutUint64(dst[32:40], n.Generation)
	binary.LittleEndian.PutUint32(dst[40:44], n.Mark.Segment)
	binary.LittleEndian.PutUint64(dst[48:56], n.Mark.Generation)
	copy(dst[64:96], n.Mark.Ordering[:])
	copy(dst[NodeHeaderBytes:], n.Mark.Claims)

	binary.LittleEndian.PutUint32(dst[0:4], crc32.Checksum(dst[4:], castagnoli))

	return nil
}

// UnmarshalNode decodes one node table block. An all-zero block decodes to the
// zero Node, whose key reports the slot as unclaimed.
func UnmarshalNode(block []byte) (Node, error) {
	if len(block) != BlockBytes {
		return Node{}, fmt.Errorf("node block is %d bytes, want %d", len(block), BlockBytes)
	}

	if allZero(block) {
		return Node{}, nil
	}

	want := binary.LittleEndian.Uint32(block[0:4])
	if got := crc32.Checksum(block[4:], castagnoli); got != want {
		return Node{}, fmt.Errorf("%w: node block checksum %08x, want %08x", ErrCorrupt, got, want)
	}

	if version := binary.LittleEndian.Uint16(block[4:6]); version > Version {
		return Node{}, fmt.Errorf("%w: node block version %d, this build reads %d",
			ErrCorrupt, version, Version)
	}

	n := Node{Generation: binary.LittleEndian.Uint64(block[32:40])}
	copy(n.Key[:], block[8:8+NodeBytes])

	if secs := binary.LittleEndian.Uint64(block[24:32]); secs != 0 {
		n.Updated = time.Unix(int64(secs), 0).UTC() //nolint:gosec // written from a positive Unix time
	}

	if n.Key.IsZero() {
		return Node{}, fmt.Errorf("%w: node block with no node", ErrCorrupt)
	}

	n.Mark.Segment = binary.LittleEndian.Uint32(block[40:44])
	if n.Mark.Segment == 0 {
		return n, nil
	}

	n.Mark.Generation = binary.LittleEndian.Uint64(block[48:56])
	copy(n.Mark.Ordering[:], block[64:96])

	n.Mark.Claims = make(Claims, ClaimBytes)
	copy(n.Mark.Claims, block[NodeHeaderBytes:])

	return n, nil
}

// Nodes reads the whole node table.
func (s *Store) Nodes() ([]Node, error) {
	s.io.Lock()
	defer s.io.Unlock()

	return s.nodesLocked(s.Superblock())
}

func (s *Store) nodesLocked(sb Superblock) ([]Node, error) {
	buf := make([]byte, BlockBytes)
	out := make([]Node, 0, sb.NodeCapacity())

	for i := range uint64(sb.NodeBlocks) {
		block := sb.NodeBlockBase() + i

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return nil, fmt.Errorf("read node block %d: %w", block, err)
		}

		n, err := UnmarshalNode(buf)
		if err != nil {
			return nil, fmt.Errorf("node block %d: %w", block, err)
		}

		if n.Key.IsZero() {
			continue
		}

		out = append(out, n)
	}

	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].Key[:], out[j].Key[:]) < 0
	})

	return out, nil
}

// SetWatermark records how far this node has caught up, claiming a block for it
// the first time.
//
// A node keeps its block for as long as it keeps refreshing. When the table is
// full, a block that has not been refreshed within grace is taken over, which
// is the only way a decommissioned node's block ever comes back. Taking one
// over is safe precisely because it is stale: a node that is not refreshing is
// not resolving blobs either, and if it comes back it claims a fresh block and
// starts from a generation it has actually seen.
func (s *Store) SetWatermark(node NodeKey, generation uint64, grace time.Duration) error {
	return s.publish(node, grace, func(existing Node) (Node, error) {
		// A watermark only ever moves forward. A node that restarts and
		// re-reads from an older generation must not appear to have gone
		// backwards, because the cleaner has already counted it as past.
		if existing.Key == node && existing.Generation > generation {
			generation = existing.Generation
		}

		// The mark answer is carried through untouched. The two halves of
		// the block have different writers on different schedules, and a
		// watermark refresh that dropped an answer would restart every
		// round in flight.
		return Node{Key: node, Generation: generation, Updated: now(), Mark: existing.Mark}, nil
	})
}

// SetMark publishes this node's answer to a mark round, carrying its watermark
// through unchanged.
//
// Answering is a claim about what this node still references, so it is only
// ever additive to what survives: the cleaner unions every node's answer and
// retires what nobody claimed.
func (s *Store) SetMark(node NodeKey, mark Mark, grace time.Duration) error {
	if !mark.Answered() {
		return errors.New("catalog: a mark answer needs a segment")
	}

	return s.publish(node, grace, func(existing Node) (Node, error) {
		return Node{
			Key:        node,
			Generation: existing.Generation,
			Updated:    now(),
			Mark:       mark,
		}, nil
	})
}

// publish applies update to this node's own block under the block's
// compare-and-swap, claiming a block first if the node does not have one.
func (s *Store) publish(node NodeKey, grace time.Duration, update func(Node) (Node, error)) error {
	if node.IsZero() {
		return errors.New("catalog: the node table needs a node")
	}

	s.io.Lock()
	defer s.io.Unlock()

	sb := s.Superblock()
	if sb.NodeBlocks == 0 {
		return fmt.Errorf("%w: catalog has no node table", ErrCorrupt)
	}

	block, err := s.findNodeBlock(sb, node, grace)
	if err != nil {
		return err
	}

	return s.mergeNodeBlock(block, func(existing Node) (Node, error) {
		// Re-read under the block's own compare-and-swap. Another node may
		// have taken the block between the scan and here, in which case
		// this one gives it up rather than overwriting a live entry.
		if !existing.Key.IsZero() && existing.Key != node && !existing.Expired(now(), grace) {
			return Node{}, fmt.Errorf("%w: node block taken by node %s", ErrConflict, existing.Key)
		}

		// A block being taken over from another node carries nothing
		// forward: its watermark and its mark answer were that node's.
		if existing.Key != node {
			existing = Node{}
		}

		return update(existing)
	})
}

// findNodeBlock locates this node's block, or picks one it may claim.
func (s *Store) findNodeBlock(sb Superblock, node NodeKey, grace time.Duration) (uint64, error) {
	buf := make([]byte, BlockBytes)

	free := -1
	at := now()

	for i := range uint64(sb.NodeBlocks) {
		block := sb.NodeBlockBase() + i

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return 0, fmt.Errorf("read node block %d: %w", block, err)
		}

		existing, err := UnmarshalNode(buf)
		if err != nil {
			return 0, fmt.Errorf("node block %d: %w", block, err)
		}

		if existing.Key == node {
			return block, nil
		}

		// The first reusable block is remembered but not taken yet: our
		// own block may still be further along in the table, and claiming
		// a second one would count this node twice.
		if free < 0 && (existing.Key.IsZero() || existing.Expired(at, grace)) {
			free = int(i) //nolint:gosec // bounded by the block count
		}
	}

	if free < 0 {
		return 0, fmt.Errorf("catalog: node table is full at %d nodes", sb.NodeCapacity())
	}

	return sb.NodeBlockAt(free), nil
}

// mergeNodeBlock applies update to a node block under the block's own
// compare-and-swap, retrying on conflict.
func (s *Store) mergeNodeBlock(block uint64, update func(existing Node) (Node, error)) error {
	buf := make([]byte, BlockBytes)
	merged := make([]byte, BlockBytes)

	var lastErr error

	for attempt := range s.retries {
		if attempt > 0 {
			sleep(backoff(attempt))
		}

		if _, err := s.vol.ReadAt(buf, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			return fmt.Errorf("read node block %d: %w", block, err)
		}

		existing, err := UnmarshalNode(buf)
		if err != nil {
			return fmt.Errorf("node block %d: %w", block, err)
		}

		updated, err := update(existing)
		if err != nil {
			return err
		}

		if err := updated.MarshalTo(merged); err != nil {
			return err
		}

		if _, err := s.vol.WriteAt(merged, int64(block*BlockBytes)); err != nil { //nolint:gosec // bounded by the extent size
			if errors.Is(err, ErrConflict) {
				lastErr = err

				continue
			}

			return fmt.Errorf("write node block %d: %w", block, err)
		}

		return nil
	}

	return fmt.Errorf("catalog: node block %d lost %d compare-and-swaps: %w", block, s.retries, lastErr)
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

	nodes, err := s.Nodes()
	if err != nil {
		return false, NodeKey{}, err
	}

	at := now()

	seen := make(map[NodeKey]Node, len(nodes))
	for _, n := range nodes {
		seen[n.Key] = n
	}

	// Expected nodes first, so the node named as holding the gate is the
	// one an operator can go and look at.
	for _, node := range expect {
		if node.IsZero() {
			return false, NodeKey{}, errors.New("catalog: expected node set contains a zero key")
		}

		n, ok := seen[node]
		if !ok || n.Expired(at, grace) || n.Generation < generation {
			return false, node, nil
		}
	}

	expected := make(map[NodeKey]struct{}, len(expect))
	for _, node := range expect {
		expected[node] = struct{}{}
	}

	for _, n := range nodes {
		if _, ok := expected[n.Key]; ok {
			continue
		}

		if n.Expired(at, grace) {
			continue
		}

		if n.Generation < generation {
			return false, n.Key, nil
		}
	}

	return true, NodeKey{}, nil
}
