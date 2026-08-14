// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clean

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// segmentPages is small enough that a test device is a few megabytes and large
// enough that a segment holds more than one blob.
const segmentPages = 4

// fakeCatalog is the whole catalog contract the cleaner uses, backed by maps.
// The real store's guarantees that matter here are that a state change is a
// compare-and-swap and that a reservation never lands in the segment it was
// asked to evacuate, so both are enforced rather than assumed.
type fakeCatalog struct {
	entries    map[uint32]*catalog.SegmentEntry
	blobs      map[uint32][]catalog.Blob
	open       uint32
	cursor     uint32
	generation uint64
	drained    bool
	laggard    catalog.NodeKey

	appended  []catalog.Record
	abandoned int
	accounts  []account
}

type account struct {
	segment uint32
	live    int64
	dead    int64
}

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{
		entries:    map[uint32]*catalog.SegmentEntry{},
		blobs:      map[uint32][]catalog.Blob{},
		generation: 100,
		drained:    true,
	}
}

func (c *fakeCatalog) add(id uint32, state catalog.SegmentState, cursor uint32, live, dead uint64) {
	c.entries[id] = &catalog.SegmentEntry{
		ID:          id,
		State:       state,
		CursorPages: cursor,
		TotalPages:  segmentPages,
		LiveBytes:   live,
		DeadBytes:   dead,
	}

	if state == catalog.SegmentOpen {
		c.open = id
		c.cursor = cursor
	}
}

func (c *fakeCatalog) Segments() ([]catalog.SegmentEntry, error) {
	out := make([]catalog.SegmentEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, *e)
	}

	// Sorted so a test's expectations do not depend on map order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}

	return out, nil
}

func (c *fakeCatalog) BlobsIn(id uint32) []catalog.Blob { return c.blobs[id] }

func (c *fakeCatalog) Generation() uint64 { return c.generation }

func (c *fakeCatalog) Reserve(pages uint32, records int) (catalog.Reservation, error) {
	if c.open == 0 {
		return catalog.Reservation{}, catalog.ErrNoOpenSegment
	}

	if c.cursor+pages > segmentPages {
		return catalog.Reservation{}, catalog.ErrFull
	}

	res := catalog.Reservation{
		Segment:     c.open,
		PageOffset:  c.cursor,
		PageCount:   pages,
		RecordCount: records,
		Generation:  c.generation + 1,
	}

	c.cursor += pages
	c.generation++

	return res, nil
}

func (c *fakeCatalog) Append(_ catalog.Reservation, records []catalog.Record) error {
	c.appended = append(c.appended, records...)

	return nil
}

func (c *fakeCatalog) Abandon(catalog.Reservation) error {
	c.abandoned++

	return nil
}

func (c *fakeCatalog) Account(id uint32, live, dead int64) error {
	c.accounts = append(c.accounts, account{segment: id, live: live, dead: dead})

	return nil
}

func (c *fakeCatalog) SetSegmentState(id uint32, from, to catalog.SegmentState, repoint uint64) error {
	entry, ok := c.entries[id]
	if !ok {
		return errors.New("no such segment")
	}

	if entry.State != from {
		return catalog.ErrSegmentState
	}

	entry.State = to
	if repoint != 0 {
		entry.RepointGeneration = repoint
	}

	return nil
}

func (c *fakeCatalog) DrainedPast(uint64, time.Duration) (bool, catalog.NodeKey, error) {
	return c.drained, c.laggard, nil
}

// fakeLocator lays the segments out end to end in one device, which is the
// shape the real image volume has.
type fakeLocator struct {
	device string
}

func (l fakeLocator) offsetOf(id uint32) uint64 {
	return uint64(id-1) * segmentPages * segment.PageBytes
}

func (l fakeLocator) Locate(addr segment.Address) (string, uint64, uint64, error) {
	if addr.Segment == 0 || addr.Segment > 8 {
		return "", 0, 0, segment.ErrUnknownSegment
	}

	return l.device, l.offsetOf(addr.Segment) + addr.ByteOffset(), addr.Span(), nil
}

func (l fakeLocator) SegmentRange(id uint32) (string, uint64, uint64, error) {
	if id == 0 || id > 8 {
		return "", 0, 0, segment.ErrUnknownSegment
	}

	return l.device, l.offsetOf(id), segmentPages * segment.PageBytes, nil
}

type fakeDiscarder struct {
	calls []account
}

func (d *fakeDiscarder) Discard(_ string, offset, length uint64) error {
	d.calls = append(d.calls, account{live: int64(offset), dead: int64(length)}) //nolint:gosec // test sizes are small

	return nil
}

type never struct{}

func (never) Elected(uint32) bool { return false }

// testDevice is a file big enough for eight segments, standing in for the image
// device. The cleaner opens it without O_DIRECT so a temporary directory works.
func testDevice(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "image.img")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("create device: %v", err)
	}

	defer f.Close() //nolint:errcheck // test cleanup

	if err := f.Truncate(8 * segmentPages * segment.PageBytes); err != nil {
		t.Fatalf("size device: %v", err)
	}

	return path
}

func newCleaner(t *testing.T, cat *fakeCatalog, dev string, discard Discarder, elector Elector) *Cleaner {
	t.Helper()

	c, err := New(Options{
		Catalog:         cat,
		Locator:         fakeLocator{device: dev},
		Discarder:       discard,
		Elector:         elector,
		Open:            func(path string) (ingest.Device, error) { return os.OpenFile(path, os.O_RDWR, 0) },
		LowWater:        0.5,
		Grace:           time.Minute,
		OnCycle:         func(Result) {},
		MaxLiveFraction: 0.5,
	})
	if err != nil {
		t.Fatalf("new cleaner: %v", err)
	}

	return c
}

// writeBlob puts a recognisable page into a segment and returns the record the
// catalog would hold for it.
func writeBlob(t *testing.T, dev string, loc fakeLocator, seg, page uint32, fill byte) catalog.Blob {
	t.Helper()

	addr := segment.Address{Segment: seg, PageOffset: page, PageCount: 1, ByteLength: segment.PageBytes}

	buf := make([]byte, segment.PageBytes)
	for i := range buf {
		buf[i] = fill
	}

	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open device: %v", err)
	}

	defer f.Close() //nolint:errcheck // test cleanup

	if _, err := f.WriteAt(buf, int64(loc.offsetOf(seg)+addr.ByteOffset())); err != nil { //nolint:gosec // test sizes are small
		t.Fatalf("write blob: %v", err)
	}

	var (
		sum    catalog.Digest
		diffID catalog.Digest
	)

	copy(sum[:], func() []byte { h := sha256.Sum256(buf); return h[:] }())

	diffID[0] = fill

	return catalog.Blob{DiffID: diffID, Address: addr, Sum: sum, Generation: 10}
}

func TestNewRequiresItsCollaborators(t *testing.T) {
	t.Parallel()

	if _, err := New(Options{Locator: fakeLocator{}, Discarder: &fakeDiscarder{}}); err == nil {
		t.Fatal("a cleaner with no catalog was accepted")
	}

	if _, err := New(Options{Catalog: newFakeCatalog(), Discarder: &fakeDiscarder{}}); err == nil {
		t.Fatal("a cleaner with no locator was accepted")
	}

	if _, err := New(Options{Catalog: newFakeCatalog(), Locator: fakeLocator{}}); err == nil {
		t.Fatal("a cleaner with no discarder was accepted")
	}
}

func TestOnceIsIdleWithRoomToSpare(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentSealed, segmentPages, segment.PageBytes, 3*segment.PageBytes)
	cat.add(2, catalog.SegmentOpen, 0, 0, 0)
	cat.add(3, catalog.SegmentEmpty, 0, 0, 0)

	c := newCleaner(t, cat, testDevice(t), &fakeDiscarder{}, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	// Two of three segments are free, which is well over the low-water mark.
	// Copying bytes around to reclaim space nobody needs is pure cost.
	if res.Phase != PhaseIdle {
		t.Fatalf("phase = %s, want idle", res.Phase)
	}
}

func TestOnceSelectsTheEmptiestSealedSegment(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentSealed, segmentPages, 3*segment.PageBytes, segment.PageBytes)
	cat.add(2, catalog.SegmentSealed, segmentPages, segment.PageBytes, 3*segment.PageBytes)
	cat.add(3, catalog.SegmentOpen, segmentPages-1, 0, 0)

	c := newCleaner(t, cat, testDevice(t), &fakeDiscarder{}, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseSelected || res.Segment != 2 {
		t.Fatalf("selected %d in phase %s, want segment 2 selected", res.Segment, res.Phase)
	}

	if state := cat.entries[2].State; state != catalog.SegmentCleaning {
		t.Fatalf("segment 2 is %s, want cleaning", state)
	}
}

func TestOnceLeavesAFullSegmentAlone(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	// Under pressure, but every sealed segment is still mostly live. Copying
	// one out would cost as much space as it recovered.
	cat.add(1, catalog.SegmentSealed, segmentPages, 4*segment.PageBytes, 0)
	cat.add(2, catalog.SegmentOpen, segmentPages-1, 0, 0)

	c := newCleaner(t, cat, testDevice(t), &fakeDiscarder{}, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseIdle {
		t.Fatalf("phase = %s, want idle", res.Phase)
	}
}

func TestOnceDefersToTheElectedNode(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentSealed, segmentPages, 0, 4*segment.PageBytes)
	cat.add(2, catalog.SegmentOpen, segmentPages-1, 0, 0)

	c := newCleaner(t, cat, testDevice(t), &fakeDiscarder{}, never{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseIdle {
		t.Fatalf("phase = %s, want idle", res.Phase)
	}

	if state := cat.entries[1].State; state != catalog.SegmentSealed {
		t.Fatalf("segment 1 is %s, want sealed: a node that lost the election must not touch it", state)
	}
}

func TestEvacuateCopiesSurvivorsAndRepoints(t *testing.T) {
	t.Parallel()

	dev := testDevice(t)
	loc := fakeLocator{device: dev}

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentCleaning, segmentPages, segment.PageBytes, 3*segment.PageBytes)
	cat.add(2, catalog.SegmentOpen, 0, 0, 0)

	blob := writeBlob(t, dev, loc, 1, 2, 0xAB)
	cat.blobs[1] = []catalog.Blob{blob}

	c := newCleaner(t, cat, dev, &fakeDiscarder{}, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseCopied || res.Blobs != 1 || res.Bytes != segment.PageBytes {
		t.Fatalf("result = %+v, want one blob of one page copied", res)
	}

	if len(cat.appended) != 1 {
		t.Fatalf("appended %d records, want 1", len(cat.appended))
	}

	// One record, not two: a chain resolves through the blob's diff ID, so
	// republishing the blob at a higher generation repoints every chain that
	// named it without any of them being rewritten.
	rec := cat.appended[0]
	if rec.Type != catalog.RecordBlob || rec.Key != blob.DiffID || rec.Ref != blob.Sum {
		t.Fatalf("record = %+v, want a blob record for the same layer", rec)
	}

	if rec.Segment == 1 {
		t.Fatal("the survivor was rewritten into the segment being evacuated")
	}

	if rec.Generation <= blob.Generation {
		t.Fatalf("record generation %d does not advance past %d", rec.Generation, blob.Generation)
	}

	// The bytes have to be there, not just the record.
	got := make([]byte, segment.PageBytes)

	f, err := os.Open(dev)
	if err != nil {
		t.Fatalf("open device: %v", err)
	}

	defer f.Close() //nolint:errcheck // test cleanup

	moved := segment.Address{Segment: rec.Segment, PageOffset: rec.PageOffset, PageCount: rec.PageCount}
	if _, err := f.ReadAt(got, int64(loc.offsetOf(rec.Segment)+moved.ByteOffset())); err != nil { //nolint:gosec // test sizes are small
		t.Fatalf("read back: %v", err)
	}

	for i, b := range got {
		if b != 0xAB {
			t.Fatalf("byte %d of the copy is %#x, want 0xab", i, b)
		}
	}

	// The victim's live bytes move to its dead column, which is what makes it
	// the next obvious victim rather than an apparently busy segment.
	var credited, debited bool

	for _, a := range cat.accounts {
		if a.segment == rec.Segment && a.live == segment.PageBytes {
			credited = true
		}

		if a.segment == 1 && a.live == -segment.PageBytes && a.dead == segment.PageBytes {
			debited = true
		}
	}

	if !credited || !debited {
		t.Fatalf("accounting = %+v, want a credit to the destination and a debit to the victim", cat.accounts)
	}
}

func TestEvacuateFinishesWhenNothingIsLeft(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentCleaning, segmentPages, 0, 4*segment.PageBytes)
	cat.add(2, catalog.SegmentOpen, 0, 0, 0)

	c := newCleaner(t, cat, testDevice(t), &fakeDiscarder{}, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseCopied {
		t.Fatalf("phase = %s, want copied", res.Phase)
	}

	entry := cat.entries[1]
	if entry.State != catalog.SegmentDraining {
		t.Fatalf("segment 1 is %s, want draining", entry.State)
	}

	// The repoint generation is what every node's watermark has to pass. A
	// zero would let the drain gate open immediately.
	if entry.RepointGeneration != cat.generation {
		t.Fatalf("repoint generation = %d, want %d", entry.RepointGeneration, cat.generation)
	}
}

func TestDrainWaitsForEveryNode(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentDraining, segmentPages, 0, 4*segment.PageBytes)
	cat.entries[1].RepointGeneration = 90
	cat.drained = false
	cat.laggard = catalog.NodeKeyFor("slow-node")

	discard := &fakeDiscarder{}
	c := newCleaner(t, cat, testDevice(t), discard, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseWaiting || res.Waiting != cat.laggard {
		t.Fatalf("result = %+v, want to be waiting on the slow node", res)
	}

	// A trimmed page reads back as zeroes rather than an error, so trimming
	// under a node that can still resolve a blob here is silent corruption.
	if len(discard.calls) != 0 {
		t.Fatalf("discarded %d ranges while a node was still behind", len(discard.calls))
	}
}

func TestDrainTrimsTheWholeSegment(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentDraining, segmentPages, 0, 4*segment.PageBytes)
	cat.entries[1].RepointGeneration = 90

	discard := &fakeDiscarder{}
	c := newCleaner(t, cat, testDevice(t), discard, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseTrimmed || res.Segment != 1 {
		t.Fatalf("result = %+v, want segment 1 trimmed", res)
	}

	if len(discard.calls) != 1 {
		t.Fatalf("discarded %d ranges, want 1", len(discard.calls))
	}

	if got := discard.calls[0]; got.live != 0 || got.dead != segmentPages*segment.PageBytes {
		t.Fatalf("discarded offset %d for %d bytes, want the whole segment", got.live, got.dead)
	}

	// Still draining: only the extent's epoch moving proves the pages are
	// gone, and that arrives from the control plane, not from here.
	if state := cat.entries[1].State; state != catalog.SegmentDraining {
		t.Fatalf("segment 1 is %s, want it left draining", state)
	}
}

func TestDrainComesBeforeEvacuation(t *testing.T) {
	t.Parallel()

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentDraining, segmentPages, 0, 4*segment.PageBytes)
	cat.entries[1].RepointGeneration = 90
	cat.add(2, catalog.SegmentCleaning, segmentPages, 0, 4*segment.PageBytes)
	cat.add(3, catalog.SegmentOpen, 0, 0, 0)

	discard := &fakeDiscarder{}
	c := newCleaner(t, cat, testDevice(t), discard, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	// Finishing a cycle frees space; starting another only consumes it.
	if res.Phase != PhaseTrimmed || res.Segment != 1 {
		t.Fatalf("result = %+v, want the draining segment finished first", res)
	}

	if state := cat.entries[2].State; state != catalog.SegmentCleaning {
		t.Fatalf("segment 2 is %s, want it untouched this pass", state)
	}
}

func TestMoveAbandonsAReservationItCannotUse(t *testing.T) {
	t.Parallel()

	dev := testDevice(t)
	loc := fakeLocator{device: dev}

	cat := newFakeCatalog()
	// The only open segment is the one being evacuated, so a reservation
	// would land back where it started.
	cat.add(1, catalog.SegmentCleaning, 0, segment.PageBytes, 0)
	cat.open = 1
	cat.cursor = 0

	cat.blobs[1] = []catalog.Blob{writeBlob(t, dev, loc, 1, 0, 0xCD)}

	c := newCleaner(t, cat, dev, &fakeDiscarder{}, Always{})

	// Not an error: the cleaner simply cannot make progress this pass, and
	// the next roll will seal the victim and give it somewhere to go.
	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Blobs != 0 {
		t.Fatalf("moved %d blobs into the segment being evacuated", res.Blobs)
	}

	if cat.abandoned != 1 {
		t.Fatalf("abandoned %d reservations, want 1", cat.abandoned)
	}

	if len(cat.appended) != 0 {
		t.Fatalf("published %d records for a move that did not happen", len(cat.appended))
	}
}

func TestEvacuateStopsWhenThereIsNowhereToPutASurvivor(t *testing.T) {
	t.Parallel()

	dev := testDevice(t)
	loc := fakeLocator{device: dev}

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentCleaning, segmentPages, segment.PageBytes, 0)
	cat.blobs[1] = []catalog.Blob{writeBlob(t, dev, loc, 1, 0, 0xEF)}

	// No open segment at all: the cleaner has to stop rather than fail, and
	// above all must not declare the victim drained.
	c := newCleaner(t, cat, dev, &fakeDiscarder{}, Always{})

	res, err := c.Once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}

	if res.Phase != PhaseIdle {
		t.Fatalf("phase = %s, want idle: the victim still holds a survivor", res.Phase)
	}

	if state := cat.entries[1].State; state != catalog.SegmentCleaning {
		t.Fatalf("segment 1 is %s, want it still cleaning", state)
	}
}

func TestCopyDetectsRot(t *testing.T) {
	t.Parallel()

	dev := testDevice(t)
	loc := fakeLocator{device: dev}

	cat := newFakeCatalog()
	cat.add(1, catalog.SegmentCleaning, segmentPages, segment.PageBytes, 0)
	cat.add(2, catalog.SegmentOpen, 0, 0, 0)

	blob := writeBlob(t, dev, loc, 1, 0, 0x11)
	blob.Sum[0] ^= 0xFF
	cat.blobs[1] = []catalog.Blob{blob}

	c := newCleaner(t, cat, dev, &fakeDiscarder{}, Always{})

	// A 4 MiB RACER page carries no data checksum of its own, so the copy is
	// the only place a corrupted layer can be caught before it is published
	// to the whole cluster under a fresh record.
	if _, err := c.Once(context.Background()); !errors.Is(err, ErrVerify) {
		t.Fatalf("once: %v, want a verification failure", err)
	}
}

// testMembers builds a peer view the rendezvous can rank.
func testMembers(names ...string) func() []ifaces.Node {
	nodes := make([]ifaces.Node, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, ifaces.Node{ID: ifaces.NodeID(name)})
	}

	return func() []ifaces.Node { return nodes }
}

func nodeID(name string) ifaces.NodeID { return ifaces.NodeID(name) }

func TestHRWElectsExactlyOneNode(t *testing.T) {
	t.Parallel()

	members := testMembers("a", "b", "c")

	var elected int

	for _, self := range []string{"a", "b", "c"} {
		h := HRW{Self: nodeID(self), Members: members}
		if h.Elected(7) {
			elected++
		}
	}

	if elected != 1 {
		t.Fatalf("%d of three nodes claimed segment 7, want 1", elected)
	}
}

func TestHRWRunsUnelectedWithoutAView(t *testing.T) {
	t.Parallel()

	// One node with no membership view has nobody to lose a rendezvous to,
	// and refusing to clean would mean a single-node cluster never reclaims.
	h := HRW{Self: nodeID("a")}
	if !h.Elected(1) {
		t.Fatal("a node with no peer view refused to clean")
	}

	h = HRW{Self: nodeID("a"), Members: testMembers()}
	if !h.Elected(1) {
		t.Fatal("a node with an empty peer view refused to clean")
	}
}
