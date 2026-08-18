// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clean

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
)

// remarkCooldown is how long a segment is passed over after a mark round that
// retired nothing.
//
// A round is cheap but not free: every node has to answer it before anything
// else in the cluster can be reclaimed. Asking the same segment again on the
// next pass would produce the same answer and keep the cleaner from ever
// reaching the segment behind it, so a fruitless round buys the rest of the
// volume a turn. It is deliberately in memory: the cost of forgetting it is one
// extra round after a restart, and the cost of writing it down is another field
// in a format that is expensive to change.
const remarkCooldown = 30 * time.Minute

// coolState remembers segments whose last mark round found nothing to retire.
type coolState struct {
	mu    sync.Mutex
	until map[uint32]time.Time
}

// note puts a segment out of consideration until the cooldown expires.
func (c *coolState) note(segment uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.until == nil {
		c.until = make(map[uint32]time.Time)
	}

	c.until[segment] = time.Now().Add(remarkCooldown)
}

// cooling reports whether a segment is still being passed over.
func (c *coolState) cooling(segment uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	until, ok := c.until[segment]
	if !ok {
		return false
	}

	if time.Now().Before(until) {
		return true
	}

	delete(c.until, segment)

	return false
}

func (c *Cleaner) cooling(segment uint32) bool { return c.cool.cooling(segment) }

// openRound asks the cluster which of a sealed segment's blobs anybody still
// wants.
//
// This is the step that makes reclamation possible at all. Containerd tells
// each node when it stops using a layer, but no node can tell whether some
// other node still wants it, and the catalog only records that a blob was
// published. Without a round every blob stays live forever, every sealed
// segment stays entirely live, and a volume of fixed size fills up once and
// never recovers.
//
// The round is published in the segment table rather than sent to anybody: the
// state names the question and the generation names the moment it was asked at,
// so a node that was down for the whole round still answers the same question
// when it comes back, and a node that never answers holds the round rather than
// losing its claims.
func (c *Cleaner) openRound(victim catalog.SegmentEntry) (Result, error) {
	// A blob is one bit in a node's answer, and an answer is one block. A
	// segment holding more blobs than there are bits could not be answered
	// completely, and a partial answer would read as "nobody wants the rest".
	if blobs := len(c.cat.BlobsIn(victim.ID)); blobs > catalog.ClaimBits {
		c.log.Warn("segment holds more blobs than a mark round can carry",
			slog.Uint64("segment", uint64(victim.ID)),
			slog.Int("blobs", blobs),
			slog.Int("limit", catalog.ClaimBits),
		)

		c.cool.note(victim.ID)

		return Result{Phase: PhaseIdle}, nil
	}

	// The round is asked at the current generation, and a node answers only
	// once it has read at least that far. That is what makes the answers
	// comparable: every node is reporting against a view of this segment that
	// includes every record written before the question.
	mark := c.cat.Generation()
	if mark == 0 {
		return Result{Phase: PhaseIdle}, nil
	}

	if err := c.cat.SetSegmentState(victim.ID, catalog.SegmentSealed, catalog.SegmentMarking, mark); err != nil {
		if errors.Is(err, catalog.ErrSegmentState) {
			// Another node picked it in the same window.
			return Result{Phase: PhaseIdle}, nil
		}

		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: open a mark round on segment %d: %w", victim.ID, err)
	}

	c.log.Info("mark round opened",
		slog.Uint64("segment", uint64(victim.ID)),
		slog.Uint64("generation", mark),
		slog.Uint64("live_bytes", c.liveBytes(victim.ID)),
	)

	return Result{Phase: PhaseMarking, Segment: victim.ID}, nil
}

// collect concludes a mark round once every node has answered it.
//
// Every expected node has to answer before anything is retired, for the same
// reason the drain gate waits for them: a node that has not answered is not
// evidence that it has no claim, and a blob retired out from under a node that
// still wants it is a layer that stops resolving and, once its pages are
// handed out again, a mount serving somebody else's bytes.
func (c *Cleaner) collect(entry catalog.SegmentEntry) (Result, error) {
	if !c.elector.Elected(entry.ID) {
		return Result{Phase: PhaseIdle}, nil
	}

	expect := c.members()
	if len(expect) == 0 {
		if c.wait.note(entry.ID, catalog.NodeKey{}) {
			c.log.Warn("mark round held: the cluster membership view is empty",
				slog.Uint64("segment", uint64(entry.ID)),
			)
		}

		return Result{Phase: PhaseWaiting, Segment: entry.ID}, nil
	}

	blobs := c.cat.BlobsIn(entry.ID)
	ordering := catalog.MarkOrdering(blobs)

	claims, waiting, err := c.answers(entry, expect, ordering)
	if err != nil {
		return Result{Phase: PhaseIdle}, err
	}

	if claims == nil {
		if !waiting.IsZero() && c.wait.note(entry.ID, waiting) {
			c.log.Info("mark round held, waiting for a node to answer",
				slog.Uint64("segment", uint64(entry.ID)),
				slog.Uint64("generation", entry.RepointGeneration),
				slog.String("node", waiting.String()),
			)
		}

		if waiting.IsZero() {
			// A node answered a different ordering of the same round, so
			// the answers cannot be combined. Abandoning the round is the
			// only safe move: its bitmaps name different blobs.
			return c.abort(entry)
		}

		return Result{Phase: PhaseWaiting, Segment: entry.ID, Waiting: waiting}, nil
	}

	c.wait.clear()

	return c.retire(entry, blobs, claims)
}

// answers gathers the round's answers, returning the union of the claims when
// every node has agreed on the ordering and answered.
//
// A nil bitmap with a named node means the round is still waiting on that node.
// A nil bitmap with no node means an answer disagreed about what the segment
// holds, which cannot be waited out.
func (c *Cleaner) answers(
	entry catalog.SegmentEntry,
	expect []catalog.NodeKey,
	ordering catalog.Digest,
) (catalog.Claims, catalog.NodeKey, error) {
	nodes, err := c.cat.Nodes()
	if err != nil {
		return nil, catalog.NodeKey{}, fmt.Errorf("clean: read the node table: %w", err)
	}

	at := time.Now()

	seen := make(map[catalog.NodeKey]catalog.Node, len(nodes))
	for _, node := range nodes {
		seen[node.Key] = node
	}

	union := catalog.NewClaims()

	// Expected nodes first, so the node named as holding the round is one an
	// operator can go and look at.
	for _, key := range expect {
		node, ok := seen[key]
		if !ok || node.Expired(at, c.grace) || !node.Mark.For(entry.ID, entry.RepointGeneration) {
			return nil, key, nil
		}

		if node.Mark.Ordering != ordering {
			c.log.Warn("mark round abandoned, a node sees a different segment",
				slog.Uint64("segment", uint64(entry.ID)),
				slog.String("node", key.String()),
			)

			return nil, catalog.NodeKey{}, nil
		}

		union.Or(node.Mark.Claims)
	}

	expected := make(map[catalog.NodeKey]struct{}, len(expect))
	for _, key := range expect {
		expected[key] = struct{}{}
	}

	// A node the cluster no longer lists is still serving containers until
	// its block goes stale, exactly as at the drain gate, so it is waited for
	// while it is fresh and written off once it is not.
	for _, node := range nodes {
		if _, ok := expected[node.Key]; ok {
			continue
		}

		if node.Expired(at, c.grace) {
			continue
		}

		if !node.Mark.For(entry.ID, entry.RepointGeneration) {
			return nil, node.Key, nil
		}

		if node.Mark.Ordering != ordering {
			c.log.Warn("mark round abandoned, a node sees a different segment",
				slog.Uint64("segment", uint64(entry.ID)),
				slog.String("node", node.Key.String()),
			)

			return nil, catalog.NodeKey{}, nil
		}

		union.Or(node.Mark.Claims)
	}

	return union, catalog.NodeKey{}, nil
}

// retire tombstones the blobs no node claimed and decides what to do with what
// is left.
func (c *Cleaner) retire(entry catalog.SegmentEntry, blobs []catalog.Blob, claims catalog.Claims) (Result, error) {
	var (
		dead  []catalog.Blob
		bytes uint64
	)

	for i, blob := range blobs {
		if claims.Has(i) {
			continue
		}

		dead = append(dead, blob)
		bytes += blob.Address.Span()
	}

	if len(dead) > 0 {
		published, err := c.tombstone(entry.ID, dead, bytes)
		if err != nil {
			return Result{Phase: PhaseIdle}, err
		}

		// Nothing was written down, so nothing was retired. Reporting the
		// blobs a round would have retired as retired would show up as
		// progress in the metrics while the volume kept filling.
		if !published {
			dead, bytes = nil, 0
		}
	}

	res := Result{Phase: PhaseMarked, Segment: entry.ID, Retired: len(dead), Bytes: bytes}

	// What is left decides whether copying the survivors out is worth it. A
	// segment that is still mostly live would cost a read and a write of
	// nearly its whole capacity to reclaim nearly nothing, so it goes back to
	// sealed, keeping the tombstones this round did write.
	live := c.liveFraction(entry)
	if live > c.maxLive {
		c.cool.note(entry.ID)

		if err := c.cat.SetSegmentState(entry.ID, catalog.SegmentMarking, catalog.SegmentSealed, 0); err != nil {
			return Result{Phase: PhaseIdle}, fmt.Errorf("clean: reseal segment %d: %w", entry.ID, err)
		}

		c.log.Info("mark round concluded, segment is still too live to clean",
			slog.Uint64("segment", uint64(entry.ID)),
			slog.Int("retired", len(dead)),
			slog.Float64("live_fraction", live),
		)

		return res, nil
	}

	if err := c.cat.SetSegmentState(entry.ID, catalog.SegmentMarking, catalog.SegmentCleaning, 0); err != nil {
		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: open segment %d for cleaning: %w", entry.ID, err)
	}

	c.log.Info("mark round concluded, cleaning segment",
		slog.Uint64("segment", uint64(entry.ID)),
		slog.Int("retired", len(dead)),
		slog.Uint64("retired_bytes", bytes),
		slog.Float64("live_fraction", live),
	)

	return res, nil
}

// tombstone publishes the retirement of blobs nobody claimed.
//
// The records go in one reservation so the round is all or nothing: a partial
// retirement would leave the segment looking emptier than the next round would
// find it, and there is nothing to be gained by splitting it up. It reports
// whether the retirement was published at all.
func (c *Cleaner) tombstone(segment uint32, dead []catalog.Blob, bytes uint64) (bool, error) {
	res, err := c.cat.ReserveRecords(len(dead))
	if err != nil {
		if errors.Is(err, catalog.ErrFull) {
			// The record log is out of slots. Retiring is exactly what
			// would eventually make room, but it cannot be written down,
			// so the round waits for a repair or a larger catalog.
			c.log.Warn("mark round cannot retire, the catalog is full",
				slog.Uint64("segment", uint64(segment)),
				slog.Int("blobs", len(dead)),
			)

			return false, nil
		}

		return false, fmt.Errorf("clean: reserve %d tombstones: %w", len(dead), err)
	}

	published := false

	defer func() {
		if !published {
			if err := c.cat.Abandon(res); err != nil {
				c.log.Warn("could not abandon a tombstone reservation", slog.Any("err", err))
			}
		}
	}()

	records := make([]catalog.Record, 0, len(dead))
	for _, blob := range dead {
		records = append(records, catalog.Record{
			Type:       catalog.RecordTombstone,
			Generation: res.Generation,
			Key:        blob.DiffID,
		})
	}

	if err := c.cat.Append(res, records); err != nil {
		return false, fmt.Errorf("clean: append %d tombstones: %w", len(dead), err)
	}

	published = true

	// Accounting is a report, not a decision: what a segment holds is counted
	// from the index. A failed update is worth knowing about and not worth
	// undoing a retirement over.
	if err := c.cat.Account(segment, -int64(bytes), int64(bytes)); err != nil { //nolint:gosec // a segment is far smaller than the sign bit
		c.log.Warn("could not account a retirement",
			slog.Uint64("segment", uint64(segment)),
			slog.Any("err", err),
		)
	}

	return true, nil
}

// abort ends a round that cannot be concluded, leaving the segment sealed.
func (c *Cleaner) abort(entry catalog.SegmentEntry) (Result, error) {
	c.cool.note(entry.ID)

	if err := c.cat.SetSegmentState(entry.ID, catalog.SegmentMarking, catalog.SegmentSealed, 0); err != nil {
		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: reseal segment %d: %w", entry.ID, err)
	}

	return Result{Phase: PhaseIdle, Segment: entry.ID}, nil
}
