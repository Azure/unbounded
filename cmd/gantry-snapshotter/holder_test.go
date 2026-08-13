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
		log:    slog.New(slog.DiscardHandler),
		format: format,
		adopt:  adopt,
		blocks: catalog.DefaultSegmentBlocks,
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

// testSet writes a device description covering one catalog and segments of the
// given page counts, and returns the parsed set.
func testSet(t *testing.T, dir string, catalogBytes uint64, pages ...uint64) *segment.Set {
	t.Helper()

	catPath := deviceFile(t, dir, "catalog.img", catalogBytes)

	body := fmt.Sprintf(`{"generation":1,"universe":7,"catalog":{"device":%q,"bytes":%d},"segments":[`, catPath, catalogBytes)

	for i, n := range pages {
		bytes := n * segment.PageBytes
		path := deviceFile(t, dir, fmt.Sprintf("seg%d.img", i+1), bytes)

		if i > 0 {
			body += ","
		}

		body += fmt.Sprintf(`{"id":%d,"device":%q,"bytes":%d}`, i+1, path, bytes)
	}

	body += "]}"

	set, err := segment.Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse device set: %v", err)
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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4, 6)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

	h := newHolder(t, true, true)
	defer h.close() //nolint:errcheck // test cleanup

	if err := h.reconcile(set); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	grown := testSet(t, dir, 64*catalog.BlockBytes, 4, 8)
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

func TestReconcileReportsAMissingDevice(t *testing.T) {
	dir := t.TempDir()
	set := testSet(t, dir, 64*catalog.BlockBytes)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

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
	set := testSet(t, dir, 64*catalog.BlockBytes, 4)

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
