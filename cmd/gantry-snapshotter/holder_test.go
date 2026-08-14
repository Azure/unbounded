// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// newHolder builds a holder that opens catalog devices without O_DIRECT, so
// the test can back one with an ordinary file.
func newHolder(t *testing.T, format, adopt bool) *holder {
	t.Helper()

	direct := false

	return &holder{
		log:        slog.New(slog.DiscardHandler),
		format:     format,
		adopt:      adopt,
		blocks:     catalog.DefaultSegmentBlocks,
		nodeBlocks: catalog.DefaultNodeBlocks,
		grace:      catalog.DefaultWatermarkGrace,
		node:       catalog.NodeKeyFor("test-node"),
		open: func(path string) (*catalog.Device, error) {
			return catalog.OpenDevice(path, catalog.DeviceOptions{Direct: &direct})
		},
	}
}

// deviceFile creates a sparse file of the requested size and returns its path.
// An existing file is grown rather than truncated, so a test can rebuild a
// device description over devices it already populated.
func deviceFile(t *testing.T, dir, name string, size uint64) string {
	t.Helper()

	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}

	if uint64(info.Size()) < size { //nolint:gosec // test sizes are small
		if err := f.Truncate(int64(size)); err != nil { //nolint:gosec // test sizes are small
			t.Fatalf("truncate %s: %v", name, err)
		}
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}

	return path
}

// testSet builds an image device map over a single backing file, which is what
// the real thing looks like: the catalog extent at offset zero, then the
// immutable extents concatenated after it.
//
// The first segment starts a whole 4 MiB in even when the catalog is smaller
// than that, because an immutable extent's base has to be page aligned and the
// page here is 4 MiB.
func testSet(t *testing.T, dir string, catalogBytes uint64, pages ...uint64) *segment.Set {
	t.Helper()

	offset := segment.PaddedSize(catalogBytes)

	entries := make([]string, 0, len(pages))

	for i, n := range pages {
		bytes := n * segment.PageBytes
		entries = append(entries, fmt.Sprintf(`{"id":%d,"offset":%d,"bytes":%d,"epoch":0}`, i+1, offset, bytes))
		offset += bytes
	}

	path := deviceFile(t, dir, "image.img", offset)

	body := fmt.Sprintf(
		`{"generation":1,"universe":7,"device":%q,"catalogBytes":%d,"segments":[%s]}`,
		path, catalogBytes, strings.Join(entries, ","),
	)

	set, err := segment.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse device map: %v", err)
	}

	return set
}

func TestHolderWithoutADeviceIsAMiss(t *testing.T) {
	h := newHolder(t, false, true)

	if _, ok := h.Resolve(catalog.Digest{1}); ok {
		t.Error("Resolve should miss with no catalog attached")
	}

	if _, ok := h.Blob(catalog.Digest{1}); ok {
		t.Error("Blob should miss with no catalog attached")
	}

	if h.Len() != 0 {
		t.Errorf("Len = %d, want 0", h.Len())
	}
}

// The write path is different: it has to report that it cannot proceed, so the
// ingest queue retries rather than silently dropping a layer.
func TestHolderWriteSideReportsNotReady(t *testing.T) {
	h := newHolder(t, false, true)

	if _, err := h.Sync(); !errors.Is(err, errNotReady) {
		t.Errorf("Sync = %v, want errNotReady", err)
	}

	if _, err := h.Reserve(1, 1); !errors.Is(err, errNotReady) {
		t.Errorf("Reserve = %v, want errNotReady", err)
	}

	if _, err := h.ReserveRecords(1); !errors.Is(err, errNotReady) {
		t.Errorf("ReserveRecords = %v, want errNotReady", err)
	}

	if err := h.Append(catalog.Reservation{}, nil); !errors.Is(err, errNotReady) {
		t.Errorf("Append = %v, want errNotReady", err)
	}

	if err := h.Account(1, 0, 0); !errors.Is(err, errNotReady) {
		t.Errorf("Account = %v, want errNotReady", err)
	}

	if err := h.Abandon(catalog.Reservation{}); !errors.Is(err, errNotReady) {
		t.Errorf("Abandon = %v, want errNotReady", err)
	}

	if _, err := h.Repair(time.Hour); !errors.Is(err, errNotReady) {
		t.Errorf("Repair = %v, want errNotReady", err)
	}
}

func TestReconcileWithoutASet(t *testing.T) {
	h := newHolder(t, true, true)
	if err := h.reconcile(nil); !errors.Is(err, errNotReady) {
		t.Errorf("reconcile(nil) = %v, want errNotReady", err)
	}
}

func TestReconcileWithoutACatalogDevice(t *testing.T) {
	set, err := segment.Parse([]byte(`{"generation":1,"segments":[]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	h := newHolder(t, true, true)
	if err := h.reconcile(set); !errors.Is(err, segment.ErrNoCatalog) {
		t.Errorf("reconcile = %v, want ErrNoCatalog", err)
	}
}

// A blank catalog device must not be formatted unless the operator said so:
// formatting the wrong device would erase the cluster's image index.
func TestReconcileRefusesToFormatByDefault(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, false, true)

	err := h.reconcile(set)
	if !errors.Is(err, catalog.ErrUnformatted) {
		t.Fatalf("reconcile = %v, want ErrUnformatted", err)
	}

	if store, _ := h.current.load(); store != nil {
		t.Error("no catalog should have been attached")
	}
}

func TestReconcileFormatsAndAdopts(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4, 6)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	store, path := h.current.load()
	if store == nil {
		t.Fatal("no catalog attached")
	}

	if want, _ := set.CatalogDevice(); path != want.Device {
		t.Errorf("attached %q, want %q", path, want.Device)
	}

	segments, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if len(segments) != 2 {
		t.Fatalf("registered %d segments, want 2", len(segments))
	}

	if segments[0].TotalPages != 4 || segments[1].TotalPages != 6 {
		t.Errorf("pages = %d, %d; want 4, 6", segments[0].TotalPages, segments[1].TotalPages)
	}

	// Ingest needs somewhere to append, so the lowest empty segment is
	// opened.
	if got := store.Superblock().OpenSegment; got != 1 {
		t.Errorf("open segment = %d, want 1", got)
	}
}

// Reconciling the same set again must change nothing and must not reopen the
// device: it runs on every device poll.
func TestReconcileIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	opens := 0
	h := newHolder(t, true, true)
	inner := h.open
	h.open = func(path string) (*catalog.Device, error) {
		opens++

		return inner(path)
	}

	defer h.close() //nolint:errcheck // test cleanup

	for range 3 {
		if err := h.reconcile(set); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	if opens != 1 {
		t.Errorf("opened the device %d times, want 1", opens)
	}

	store, _ := h.current.load()

	segments, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if len(segments) != 1 {
		t.Errorf("registered %d segments, want 1", len(segments))
	}
}

// A node that is told not to adopt still attaches the catalog: it reads the
// cluster's images, it just does not write the segment table.
func TestReconcileWithoutAdoption(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, false)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	store, _ := h.current.load()

	segments, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if len(segments) != 0 {
		t.Errorf("registered %d segments, want none", len(segments))
	}

	if got := store.Superblock().OpenSegment; got != 0 {
		t.Errorf("open segment = %d, want none", got)
	}
}

// A second node attaching to a catalog another node already formatted and
// populated must find the same segments and leave them alone.
func TestReconcileAdoptsAnExistingCatalog(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	first := newHolder(t, true, true)
	if err := first.reconcile(set); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	if err := first.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The second node is not allowed to format, which proves it found a
	// catalog rather than making one.
	second := newHolder(t, false, true)
	defer second.close() //nolint:errcheck // test cleanup

	if err := second.reconcile(set); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	store, _ := second.current.load()
	if got := store.Superblock().OpenSegment; got != 1 {
		t.Errorf("open segment = %d, want 1", got)
	}

	segments, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if len(segments) != 1 || segments[0].TotalPages != 4 {
		t.Errorf("segments = %+v", segments)
	}
}

// A segment device that appears after the catalog was attached is registered
// on the next reconcile, without disturbing the segment already open.
func TestReconcilePicksUpANewSegment(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	grown := testSet(t, dir, 256*catalog.BlockBytes, 4, 8)
	if err := h.reconcile(grown); err != nil {
		t.Fatalf("reconcile grown: %v", err)
	}

	store, _ := h.current.load()

	segments, err := store.Segments()
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	if len(segments) != 2 {
		t.Fatalf("registered %d segments, want 2", len(segments))
	}

	if got := store.Superblock().OpenSegment; got != 1 {
		t.Errorf("open segment = %d, want the original 1", got)
	}
}

// racer-ctrl publishes a set with no catalog when the image volume goes away.
// The descriptor has to be let go of: an open file on a deleted ublk device
// pins the minor in the kernel, and racer can never export at that number
// again.
func TestReconcileReleasesTheDeviceOnARetraction(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	opens := 0
	h := newHolder(t, true, true)
	inner := h.open
	h.open = func(path string) (*catalog.Device, error) {
		opens++

		return inner(path)
	}

	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	retracted, err := segment.Parse([]byte(`{"generation":2,"segments":[]}`))
	if err != nil {
		t.Fatalf("parse retraction: %v", err)
	}

	if err := h.reconcile(retracted); !errors.Is(err, segment.ErrNoCatalog) {
		t.Fatalf("reconcile retraction = %v, want ErrNoCatalog", err)
	}

	if store, path := h.current.load(); store != nil || path != "" {
		t.Fatalf("still attached to %q after a retraction", path)
	}

	// The volume coming back has to reopen the device rather than serve a
	// closed one.
	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile after retraction: %v", err)
	}

	if opens != 2 {
		t.Errorf("opened the device %d times, want 2", opens)
	}
}

// Detaching when nothing is attached is what every poll does before the image
// volume exists, so it must stay quiet.
func TestReconcileRetractionWithoutAnAttachmentIsQuiet(t *testing.T) {
	retracted, err := segment.Parse([]byte(`{"generation":1,"segments":[]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	h := newHolder(t, true, true)
	if err := h.reconcile(retracted); !errors.Is(err, segment.ErrNoCatalog) {
		t.Errorf("reconcile = %v, want ErrNoCatalog", err)
	}

	if had, err := h.current.detach(); had || err != nil {
		t.Errorf("detach = %v, %v; want false, nil", had, err)
	}
}

func TestReconcileReportsAMissingDevice(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes)

	desc, err := set.CatalogDevice()
	if err != nil {
		t.Fatalf("catalog device: %v", err)
	}

	if err := os.Remove(desc.Device); err != nil {
		t.Fatalf("remove: %v", err)
	}

	h := newHolder(t, true, true)
	if err := h.reconcile(set); err == nil {
		t.Fatal("want an error for a missing device")
	}
}

// Once attached, the read and write sides go through to the store.
func TestHolderDelegates(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	res, err := h.Reserve(1, 2)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	diffID, chainID := catalog.Digest{0xaa}, catalog.Digest{0xbb}
	addr := res.Address(1 << 20)

	records := []catalog.Record{
		{
			Type: catalog.RecordBlob, Segment: addr.Segment, PageOffset: addr.PageOffset,
			PageCount: addr.PageCount, ByteLength: addr.ByteLength,
			Generation: res.Generation, Key: diffID, Ref: catalog.Digest{0xcc},
		},
		{Type: catalog.RecordChain, Generation: res.Generation, Key: chainID, Ref: diffID},
	}

	if err := h.Append(res, records); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := h.Account(addr.Segment, int64(addr.Span()), 0); err != nil { //nolint:gosec // test sizes are small
		t.Fatalf("account: %v", err)
	}

	blob, ok := h.Resolve(chainID)
	if !ok {
		t.Fatal("chain should resolve")
	}

	if blob.Address != addr {
		t.Errorf("address = %+v, want %+v", blob.Address, addr)
	}

	if _, ok := h.Blob(diffID); !ok {
		t.Error("blob should resolve")
	}

	if h.Len() == 0 {
		t.Error("Len should count the published keys")
	}

	if _, err := h.Sync(); err != nil {
		t.Errorf("sync: %v", err)
	}
}

// Closing detaches the catalog, and closing twice is safe: shutdown runs it
// from a defer that may follow an explicit close.
func TestHolderCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if err := h.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := h.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	if _, ok := h.Resolve(catalog.Digest{1}); ok {
		t.Error("a closed holder should miss")
	}
}

// watermarkFor returns this node's entry in the node table, if it has one.
func watermarkFor(t *testing.T, store *catalog.Store, node catalog.NodeKey) (catalog.Node, bool) {
	t.Helper()

	nodes, err := store.Nodes()
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}

	for _, n := range nodes {
		if n.Key == node {
			return n, true
		}
	}

	return catalog.Node{}, false
}

// Attaching a catalog claims this node's slot in the drain gate before the
// store is published, so the node is never able to resolve a blob out of a
// catalog whose cleaner has not been told to wait for it.
func TestReconcileClaimsAWatermark(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	store, _ := h.current.load()
	if store == nil {
		t.Fatal("no catalog attached")
	}

	mark, ok := watermarkFor(t, store, h.node)
	if !ok {
		t.Fatal("attaching did not claim a watermark slot")
	}

	// Claimed at zero: the node is visible, but it has not yet promised to
	// have pruned anything, so every trim waits for it.
	if mark.Generation != 0 {
		t.Errorf("claimed at generation %d, want 0", mark.Generation)
	}

	if mark.Updated.IsZero() {
		t.Error("claimed slot has no timestamp, so it reads as expired")
	}
}

// A daemon that cannot claim a slot must not attach the catalog at all: an
// invisible reader is one the cleaner will happily trim pages out from under.
func TestReconcileWithoutANodeNameRefusesToAttach(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	h.node = catalog.NodeKey{}

	defer h.close() //nolint:errcheck // test cleanup

	err := h.reconcile(set)
	if err == nil {
		t.Fatal("reconcile without a node name should fail")
	}

	if !strings.Contains(err.Error(), "watermark") {
		t.Errorf("reconcile = %v, want a watermark claim failure", err)
	}

	if store, _ := h.current.load(); store != nil {
		t.Error("a catalog was published despite the failed claim")
	}

	// And the daemon degrades to local unpack rather than serving pages it
	// cannot protect.
	if _, ok := h.Resolve(catalog.Digest{1}); ok {
		t.Error("Resolve should miss with no catalog published")
	}
}

// Watermark records progress; Refresh repeats it. Liveness and progress share
// one entry, but only the sweep may raise the generation.
func TestWatermarkAndRefresh(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 256*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	store, _ := h.current.load()

	if err := h.Watermark(7); err != nil {
		t.Fatalf("Watermark: %v", err)
	}

	if mark, _ := watermarkFor(t, store, h.node); mark.Generation != 7 {
		t.Errorf("generation = %d, want 7", mark.Generation)
	}

	if err := h.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if mark, _ := watermarkFor(t, store, h.node); mark.Generation != 7 {
		t.Errorf("refresh moved the generation to %d, want 7", mark.Generation)
	}

	// A sweep that finds less than the last one did cannot retract what
	// was already promised, and neither can the refresh that follows it.
	if err := h.Watermark(3); err != nil {
		t.Fatalf("Watermark: %v", err)
	}

	if err := h.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if mark, _ := watermarkFor(t, store, h.node); mark.Generation != 7 {
		t.Errorf("generation fell back to %d, want 7", mark.Generation)
	}
}

// A refresh that the catalog rejects must not be remembered as published, or
// the next one would repeat a promise the gate never accepted.
func TestWatermarkWithoutACatalog(t *testing.T) {
	h := newHolder(t, true, true)

	if err := h.Watermark(4); !errors.Is(err, errNotReady) {
		t.Errorf("Watermark = %v, want errNotReady", err)
	}

	if err := h.Refresh(); !errors.Is(err, errNotReady) {
		t.Errorf("Refresh = %v, want errNotReady", err)
	}

	if got := h.published.Load(); got != 0 {
		t.Errorf("published = %d after a failed write, want 0", got)
	}
}

func TestWatermarkWithoutANodeName(t *testing.T) {
	h := newHolder(t, true, true)
	h.node = catalog.NodeKey{}

	if err := h.Watermark(4); err == nil || errors.Is(err, errNotReady) {
		t.Errorf("Watermark = %v, want a node name failure", err)
	}

	if err := h.Refresh(); err == nil || errors.Is(err, errNotReady) {
		t.Errorf("Refresh = %v, want a node name failure", err)
	}
}
