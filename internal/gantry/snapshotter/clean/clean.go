// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package clean reclaims segments of the image volume.
//
// The layer store is a log: blobs are appended to an open segment, and a blob
// that nothing references any more is dead weight that the append cursor never
// goes back over. Without reclamation the volume fills once and stays full, and
// the only recovery is to replace it and lose every layer.
//
// Reclamation has to be per segment because RACER's only reclaim primitive is
// an extent's tombstone epoch, which is per extent and destroys the whole
// extent when it advances. So a cycle moves one segment at a time through the
// states the catalog already defines:
//
//	Sealed   -> Cleaning   the survivors are being copied out
//	Cleaning -> Draining   nothing resolves here any more, but mounts may linger
//	Draining -> (trimmed)  every page discarded, waiting for the epoch to move
//	                       Empty, once the control plane has collected it
//
// The last step is not this package's to take. Advancing the epoch is the
// control plane's job, gated on every node reporting the extent holds no live
// pages, which is exactly the state a full discard produces. The catalog entry
// returns to Empty when racer-ctrl republishes the extent at its new epoch and
// the holder notices.
//
// Two things make the middle of that sequence safe. A copied blob gets a fresh
// record at a higher generation, and readers take the highest generation for a
// key, so the old location stays valid for as long as anything still resolves
// to it. And a trimmed 4 MiB page reads back as zeroes rather than an error, so
// the discard cannot happen until every node has caught up past the repoint and
// pruned its mounts. That is what the catalog's watermark table is for.
package clean

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// DefaultInterval is how often a cleaner looks for work. Reclamation is never
// urgent - the alternative to a slow cycle is a full volume, not a stalled one -
// and every pass reads the segment table off the device.
const DefaultInterval = time.Minute

// DefaultLowWater is the free capacity below which a cycle starts, as a
// fraction of the volume. Cleaning costs a read and a write of every survivor,
// so it is worth deferring until the space is actually wanted, but not until
// the volume is so full that ingest is already failing.
const DefaultLowWater = 0.25

// DefaultMaxLiveFraction is the most live data a segment may hold and still be
// worth cleaning. Copying a segment that is nine tenths live buys a tenth of a
// segment for nine tenths of a segment of I/O.
const DefaultMaxLiveFraction = 0.5

// ErrVerify reports a survivor that did not hash to what its record says. The
// copy is abandoned rather than published, so the original stays live.
var ErrVerify = errors.New("clean: blob verification failed")

// Catalog is the part of the catalog store a cleaner uses.
type Catalog interface {
	Sync() (bool, error)
	Hole() (uint64, time.Duration)
	Segments() ([]catalog.SegmentEntry, error)
	BlobsIn(id uint32) []catalog.Blob
	Generation() uint64
	Reserve(pages uint32, records int) (catalog.Reservation, error)
	Append(res catalog.Reservation, records []catalog.Record) error
	Abandon(res catalog.Reservation) error
	Account(id uint32, liveDelta, deadDelta int64) error
	SetSegmentState(id uint32, from, to catalog.SegmentState, repoint uint64) error
	DrainedPast(generation uint64, grace time.Duration, expect []catalog.NodeKey) (bool, catalog.NodeKey, error)
}

// Locator resolves cluster-wide addresses to this node's device.
type Locator interface {
	Locate(addr segment.Address) (device string, offset, length uint64, err error)
	SegmentRange(id uint32) (device string, offset, length uint64, err error)
}

// Discarder trims a byte range of a block device.
type Discarder interface {
	Discard(device string, offset, length uint64) error
}

// Elector decides whether this node drives a segment's cycle.
//
// The catalog's compare-and-swaps make a race harmless, so this is about not
// doing the same copy on a thousand nodes at once rather than about
// correctness.
type Elector interface {
	Elected(id uint32) bool
}

// Phase names what a pass did, for logging and metrics.
type Phase string

// The phases a pass can report.
const (
	PhaseIdle     Phase = "idle"
	PhaseSelected Phase = "selected"
	PhaseCopied   Phase = "copied"
	PhaseWaiting  Phase = "waiting"
	PhaseTrimmed  Phase = "trimmed"
)

// Result is what one pass did.
type Result struct {
	Phase   Phase
	Segment uint32

	// Blobs and Bytes count survivors copied by this pass.
	Blobs int
	Bytes uint64

	// Waiting is the node the drain gate is held up by, when Phase is
	// PhaseWaiting.
	Waiting catalog.NodeKey
}

// Options configures a Cleaner.
type Options struct {
	// Catalog is required.
	Catalog Catalog

	// Locator is required.
	Locator Locator

	// Discarder is required.
	Discarder Discarder

	// Elector defaults to Always, which is right on one node and wasteful on
	// many.
	Elector Elector

	// Members reports the nodes the cluster expects to be serving this
	// catalog, including this one. Required, and required to be
	// conservative: a node it omits is a node the drain gate will not wait
	// for, and trimming is not undoable.
	//
	// It is a function rather than a slice because membership changes while
	// the daemon runs, and the value that matters is the one at the moment
	// the gate is asked.
	Members func() []catalog.NodeKey

	// Open opens the image device. Defaults to ingest.OpenDirect, which is
	// what the copy needs: the page cache would hold a second copy of every
	// byte moved and RACER's own cache is already the cache.
	Open ingest.OpenFunc

	// Interval defaults to DefaultInterval.
	Interval time.Duration

	// LowWater defaults to DefaultLowWater.
	LowWater float64

	// MaxLiveFraction defaults to DefaultMaxLiveFraction.
	MaxLiveFraction float64

	// Grace defaults to catalog.DefaultWatermarkGrace.
	Grace time.Duration

	// Log defaults to the discard logger.
	Log *slog.Logger

	// OnCycle observes every pass that did something.
	OnCycle func(Result)
}

// Cleaner reclaims one segment at a time.
type Cleaner struct {
	cat      Catalog
	loc      Locator
	discard  Discarder
	elector  Elector
	members  func() []catalog.NodeKey
	open     ingest.OpenFunc
	interval time.Duration
	lowWater float64
	maxLive  float64
	grace    time.Duration
	log      *slog.Logger
	onCycle  func(Result)

	// wait records what the drain gate is currently stuck on, so a stall
	// can be measured rather than inferred from an absence of progress.
	wait waitState
}

// waitState is the drain gate's current stall, if any.
type waitState struct {
	mu      sync.Mutex
	segment uint32
	node    catalog.NodeKey
	since   time.Time
}

// note records a stall and reports whether it is a new one, which is what
// decides between logging it and staying quiet about a condition already
// reported.
func (w *waitState) note(segment uint32, node catalog.NodeKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.segment == segment && w.node == node && !w.since.IsZero() {
		return false
	}

	w.segment, w.node, w.since = segment, node, time.Now()

	return true
}

// clear forgets a stall that has ended.
func (w *waitState) clear() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.segment, w.node, w.since = 0, catalog.NodeKey{}, time.Time{}
}

// Waiting reports how long the drain gate has been held up and by whom. The
// duration is zero when nothing is waiting.
func (c *Cleaner) Waiting() (uint32, catalog.NodeKey, time.Duration) {
	c.wait.mu.Lock()
	defer c.wait.mu.Unlock()

	if c.wait.since.IsZero() {
		return 0, catalog.NodeKey{}, 0
	}

	return c.wait.segment, c.wait.node, time.Since(c.wait.since)
}

// New builds a Cleaner.
func New(opts Options) (*Cleaner, error) {
	if opts.Catalog == nil {
		return nil, errors.New("clean: catalog required")
	}

	if opts.Locator == nil {
		return nil, errors.New("clean: locator required")
	}

	if opts.Discarder == nil {
		return nil, errors.New("clean: discarder required")
	}

	if opts.Members == nil {
		return nil, errors.New("clean: members required, the drain gate cannot wait for nodes it cannot name")
	}

	c := &Cleaner{
		cat:      opts.Catalog,
		loc:      opts.Locator,
		discard:  opts.Discarder,
		elector:  opts.Elector,
		members:  opts.Members,
		open:     opts.Open,
		interval: opts.Interval,
		lowWater: opts.LowWater,
		maxLive:  opts.MaxLiveFraction,
		grace:    opts.Grace,
		log:      opts.Log,
		onCycle:  opts.OnCycle,
	}

	if c.elector == nil {
		c.elector = Always{}
	}

	if c.open == nil {
		c.open = ingest.OpenDirect
	}

	if c.interval <= 0 {
		c.interval = DefaultInterval
	}

	if c.lowWater <= 0 {
		c.lowWater = DefaultLowWater
	}

	if c.maxLive <= 0 {
		c.maxLive = DefaultMaxLiveFraction
	}

	if c.grace <= 0 {
		c.grace = catalog.DefaultWatermarkGrace
	}

	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}

	return c, nil
}

// Run drives passes until the context is cancelled.
func (c *Cleaner) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if _, err := c.Once(ctx); err != nil {
			// A failed pass is not a failed daemon. The states are
			// compare-and-swapped, so whatever did not finish is picked up
			// by the next pass or by another node.
			c.log.Warn("clean pass failed", slog.Any("err", err))
		}
	}
}

// Once runs a single pass and reports what it did.
//
// A pass advances at most one segment by at most one step. Reclamation is a
// background activity competing with ingest for the same device, and a cleaner
// that emptied the whole volume in one go would stall every container start on
// the node while it did.
func (c *Cleaner) Once(ctx context.Context) (Result, error) {
	// Every decision below is read out of the index: which blobs a segment
	// still holds, how much of its capacity is live, and whether there is
	// anything left to copy out. A stale index under-reports all three, and
	// the mistake that follows - repointing a segment whose survivors were
	// never copied, and then trimming it - cannot be undone, so a pass
	// starts by catching up and does nothing at all if it cannot.
	if _, err := c.cat.Sync(); err != nil {
		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: sync catalog: %w", err)
	}

	if hole, age := c.cat.Hole(); hole != 0 {
		// Reading stops at an unwritten slot, so the index is known to be
		// incomplete, and the blobs missing from it are exactly the ones a
		// copy would leave behind. Repair is what clears this.
		c.log.Debug("clean pass skipped, the catalog is stopped at a hole",
			slog.Uint64("record", hole),
			slog.Duration("age", age),
		)

		return Result{Phase: PhaseIdle}, nil
	}

	entries, err := c.cat.Segments()
	if err != nil {
		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: read segments: %w", err)
	}

	// Finishing what is already started comes first. A segment left mid-cycle
	// holds its space without serving anything, which is strictly worse than
	// a sealed one.
	for _, entry := range entries {
		if entry.State == catalog.SegmentDraining {
			return c.report(c.drain(ctx, entry))
		}
	}

	for _, entry := range entries {
		if entry.State == catalog.SegmentCleaning {
			return c.report(c.evacuate(ctx, entry))
		}
	}

	victim, ok := c.victim(entries)
	if !ok {
		return Result{Phase: PhaseIdle}, nil
	}

	if !c.elector.Elected(victim.ID) {
		return Result{Phase: PhaseIdle}, nil
	}

	if err := c.cat.SetSegmentState(victim.ID, catalog.SegmentSealed, catalog.SegmentCleaning, 0); err != nil {
		if errors.Is(err, catalog.ErrSegmentState) {
			// Another node picked it in the same window.
			return Result{Phase: PhaseIdle}, nil
		}

		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: open segment %d for cleaning: %w", victim.ID, err)
	}

	c.log.Info("cleaning segment",
		slog.Uint64("segment", uint64(victim.ID)),
		slog.Uint64("live_bytes", c.liveBytes(victim.ID)),
		slog.Uint64("accounted_live_bytes", victim.LiveBytes),
		slog.Uint64("accounted_dead_bytes", victim.DeadBytes),
	)

	return c.report(Result{Phase: PhaseSelected, Segment: victim.ID}, nil)
}

func (c *Cleaner) report(res Result, err error) (Result, error) {
	if err == nil && res.Phase != PhaseIdle && c.onCycle != nil {
		c.onCycle(res)
	}

	return res, err
}

// victim picks the sealed segment worth cleaning, if the volume is full enough
// to want the space.
//
// How much of a segment is still live is counted from the index rather than
// read out of the segment table. The table's live and dead columns are
// maintained by whoever publishes or moves a blob, and an update that is lost -
// a node that died between appending a record and accounting for it, a
// subtraction that could not be written - is never recovered, because nothing
// re-derives them. The index is not like that: every node rebuilds it from the
// record log, so what it says a segment holds is what a reader would actually
// resolve there. Choosing a victim from a number that can silently drift means
// eventually evacuating a segment that is nearly all live, or passing over one
// that is empty.
func (c *Cleaner) victim(entries []catalog.SegmentEntry) (catalog.SegmentEntry, bool) {
	var free, total uint64

	for _, entry := range entries {
		total += uint64(entry.TotalPages)

		if entry.State == catalog.SegmentEmpty || entry.State == catalog.SegmentOpen {
			free += uint64(entry.FreePages())
		}
	}

	if total == 0 || float64(free)/float64(total) >= c.lowWater {
		return catalog.SegmentEntry{}, false
	}

	var (
		best     catalog.SegmentEntry
		bestLive float64
		found    bool
	)

	for _, entry := range entries {
		if entry.State != catalog.SegmentSealed {
			continue
		}

		live := c.liveFraction(entry)
		if live > c.maxLive {
			continue
		}

		if !found || live < bestLive {
			best, bestLive, found = entry, live, true
		}
	}

	return best, found
}

// liveFraction is the share of a segment's capacity the index still resolves
// into, in [0, 1]. A segment with no capacity reports 0 rather than dividing by
// zero.
func (c *Cleaner) liveFraction(entry catalog.SegmentEntry) float64 {
	capacity := uint64(entry.TotalPages) * segment.PageBytes
	if capacity == 0 {
		return 0
	}

	return float64(c.liveBytes(entry.ID)) / float64(capacity)
}

// liveBytes is the padded size of the blobs the index still resolves into a
// segment. Padded, because a blob owns whole 4 MiB pages and the tail of its
// last page cannot be handed to anything else.
func (c *Cleaner) liveBytes(id uint32) uint64 {
	var live uint64

	for _, blob := range c.cat.BlobsIn(id) {
		live += blob.Address.Span()
	}

	return live
}

// evacuate copies one batch of survivors out of a segment being cleaned, and
// moves it to draining once there are none left.
func (c *Cleaner) evacuate(ctx context.Context, entry catalog.SegmentEntry) (Result, error) {
	if !c.elector.Elected(entry.ID) {
		return Result{Phase: PhaseIdle}, nil
	}

	blobs := c.cat.BlobsIn(entry.ID)
	if len(blobs) == 0 {
		// Nothing resolves here any more. The repoint generation is read
		// after the last copy was published, so a node that has reached it
		// has seen every one of them.
		repoint := c.cat.Generation()
		if repoint == 0 {
			// A store that has not caught up cannot name a generation the
			// gate would mean anything against.
			return Result{Phase: PhaseIdle}, nil
		}

		if err := c.cat.SetSegmentState(entry.ID, catalog.SegmentCleaning, catalog.SegmentDraining, repoint); err != nil {
			if errors.Is(err, catalog.ErrSegmentState) {
				return Result{Phase: PhaseIdle}, nil
			}

			return Result{Phase: PhaseIdle}, fmt.Errorf("clean: drain segment %d: %w", entry.ID, err)
		}

		c.log.Info("segment evacuated, waiting for readers to catch up",
			slog.Uint64("segment", uint64(entry.ID)),
			slog.Uint64("repoint_generation", repoint),
		)

		return Result{Phase: PhaseCopied, Segment: entry.ID}, nil
	}

	res := Result{Phase: PhaseCopied, Segment: entry.ID}

	for _, blob := range blobs {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		moved, err := c.move(entry.ID, blob)
		if err != nil {
			return res, err
		}

		if !moved {
			continue
		}

		res.Blobs++
		res.Bytes += blob.Address.ByteLength
	}

	if res.Blobs == 0 {
		// Every survivor was skipped, which means there was nowhere to put
		// one. Reporting a copy would say the segment moved a step it did
		// not, and the step that matters, sealing it as drained, is the one
		// branch above that is only reached when nothing resolves here.
		return Result{Phase: PhaseIdle}, nil
	}

	return res, nil
}

// move copies one blob out of the victim and publishes a record at a higher
// generation pointing at the copy.
//
// The record is the only thing that has to be atomic. If the copy lands and the
// record does not, the reservation is abandoned and the bytes are dead weight
// in the destination, which the next cycle collects. If both land, every reader
// converges on the copy the moment it reads the record, and the original stays
// readable until it does.
func (c *Cleaner) move(victim uint32, blob catalog.Blob) (bool, error) {
	pages := blob.Address.PageCount
	size := blob.Address.ByteLength

	res, err := c.cat.Reserve(pages, 1)
	if err != nil {
		if errors.Is(err, catalog.ErrFull) || errors.Is(err, catalog.ErrNoOpenSegment) {
			// Nowhere to put it. Cleaning is what eventually fixes that, but
			// not this cycle.
			return false, nil
		}

		return false, fmt.Errorf("clean: reserve for blob %s: %w", blob.DiffID.Short(), err)
	}

	published := false

	defer func() {
		if published {
			return
		}

		if err := c.cat.Abandon(res); err != nil {
			c.log.Warn("could not abandon a reservation",
				slog.String("blob", blob.DiffID.Short()), slog.Any("err", err))
		}
	}()

	if res.Segment == victim {
		// The victim is still the open segment, which means it was never
		// sealed. Copying it into itself would loop.
		return false, nil
	}

	dst := res.Address(size)

	if err := c.copy(blob, dst); err != nil {
		return false, err
	}

	record := catalog.Record{
		Type:       catalog.RecordBlob,
		Segment:    dst.Segment,
		PageOffset: dst.PageOffset,
		PageCount:  dst.PageCount,
		ByteLength: dst.ByteLength,
		Generation: res.Generation,
		Key:        blob.DiffID,
		Ref:        blob.Sum,
	}

	if err := c.cat.Append(res, []catalog.Record{record}); err != nil {
		return false, fmt.Errorf("clean: publish blob %s: %w", blob.DiffID.Short(), err)
	}

	published = true

	span := int64(dst.Span())

	if err := c.cat.Account(dst.Segment, span, 0); err != nil {
		c.log.Warn("could not account a copy", slog.Any("err", err))
	}

	// The original is dead the instant the record lands: nothing resolves to
	// it any more, it is just not reclaimable until the extent is.
	if err := c.cat.Account(victim, -span, span); err != nil {
		c.log.Warn("could not account an evacuation", slog.Any("err", err))
	}

	return true, nil
}

// copy moves a blob's pages within the image device, verifying as it goes.
//
// Reading it anyway makes the check free, and it is worth having: RACER's 4 MiB
// pages carry no data checksum, so a copy is the one point where a bit that
// rotted in place would otherwise be published under a fresh record and
// believed by the whole cluster.
func (c *Cleaner) copy(blob catalog.Blob, dst segment.Address) error {
	srcDevice, srcOffset, _, err := c.loc.Locate(blob.Address)
	if err != nil {
		return fmt.Errorf("clean: locate blob %s: %w", blob.DiffID.Short(), err)
	}

	dstDevice, dstOffset, _, err := c.loc.Locate(dst)
	if err != nil {
		return fmt.Errorf("clean: locate copy of %s: %w", blob.DiffID.Short(), err)
	}

	if srcDevice != dstDevice {
		return fmt.Errorf("clean: blob %s would move between devices %s and %s",
			blob.DiffID.Short(), srcDevice, dstDevice)
	}

	dev, err := c.open(srcDevice)
	if err != nil {
		return err
	}

	defer dev.Close() //nolint:errcheck // read-write of whole pages, nothing is buffered

	buf := ingest.AlignedBuffer(segment.PageBytes)
	hash := sha256.New()

	for done := uint64(0); done < blob.Address.ByteLength; {
		if _, err := dev.ReadAt(buf, int64(srcOffset+done)); err != nil { //nolint:gosec // offsets are page multiples of a device size
			return fmt.Errorf("clean: read blob %s at %d: %w", blob.DiffID.Short(), srcOffset+done, err)
		}

		n := blob.Address.ByteLength - done
		if n > segment.PageBytes {
			n = segment.PageBytes
		}

		// The tail of the last page is padding the record's byte length
		// excludes, so it is copied but not hashed.
		if _, err := hash.Write(buf[:n]); err != nil {
			return fmt.Errorf("clean: hash: %w", err)
		}

		if _, err := dev.WriteAt(buf, int64(dstOffset+done)); err != nil { //nolint:gosec // as above
			return fmt.Errorf("clean: write blob %s at %d: %w", blob.DiffID.Short(), dstOffset+done, err)
		}

		done += n
	}

	var sum catalog.Digest

	copy(sum[:], hash.Sum(nil))

	if sum != blob.Sum {
		return fmt.Errorf("%w: blob %s reads back as %s", ErrVerify, blob.Sum.Short(), sum.Short())
	}

	return nil
}

// drain trims a segment nothing resolves into, once every node has caught up
// past the repoint.
//
// This is the step that cannot be taken early. A trimmed 4 MiB page reads back
// as zeroes rather than failing, so a node still holding a mount into the
// segment would not see an error, it would see a corrupt filesystem. The gate
// is therefore asked against the set of nodes the cluster expects, not against
// whoever happens to have left a watermark behind: a node that is absent from
// the table has not proved anything, and the reason it is absent may be that it
// has only just started and is already mounting layers out of this segment.
func (c *Cleaner) drain(_ context.Context, entry catalog.SegmentEntry) (Result, error) {
	if !c.elector.Elected(entry.ID) {
		return Result{Phase: PhaseIdle}, nil
	}

	expect := c.members()
	if len(expect) == 0 {
		// Membership is unknown, not empty. This node is always in the
		// set, so an empty one means the view has not loaded or has
		// failed, and the gate stays shut until it says otherwise.
		if c.wait.note(entry.ID, catalog.NodeKey{}) {
			c.log.Warn("drain gate held: the cluster membership view is empty",
				slog.Uint64("segment", uint64(entry.ID)))
		}

		return Result{Phase: PhaseWaiting, Segment: entry.ID}, nil
	}

	drained, laggard, err := c.cat.DrainedPast(entry.RepointGeneration, c.grace, expect)
	if err != nil {
		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: drain gate for segment %d: %w", entry.ID, err)
	}

	if !drained {
		if c.wait.note(entry.ID, laggard) {
			c.log.Info("drain gate held, waiting for a node to catch up",
				slog.Uint64("segment", uint64(entry.ID)),
				slog.Uint64("repoint", entry.RepointGeneration),
				slog.String("node", laggard.String()),
			)
		}

		return Result{
			Phase:   PhaseWaiting,
			Segment: entry.ID,
			Waiting: laggard,
		}, nil
	}

	c.wait.clear()

	device, offset, length, err := c.loc.SegmentRange(entry.ID)
	if err != nil {
		// The segment is not published on this node, so it has nothing to
		// trim. Another node's cleaner will get to it.
		if errors.Is(err, segment.ErrUnknownSegment) {
			return Result{Phase: PhaseIdle}, nil
		}

		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: locate segment %d: %w", entry.ID, err)
	}

	if err := c.discard.Discard(device, offset, length); err != nil {
		return Result{Phase: PhaseIdle}, fmt.Errorf("clean: discard segment %d: %w", entry.ID, err)
	}

	c.log.Info("segment trimmed, waiting for the control plane to collect it",
		slog.Uint64("segment", uint64(entry.ID)),
		slog.Uint64("bytes", length),
	)

	// The entry stays Draining. Only the extent's epoch moving proves the
	// pages are really gone, and that is the holder's signal to reset it.
	return Result{Phase: PhaseTrimmed, Segment: entry.ID, Bytes: length}, nil
}
