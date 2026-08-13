// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/gantry/snapshotter"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// errNotReady reports that the catalog device has not been opened yet.
//
// It is not a fault. racer-ctrl may not have staged this node's image device,
// the operator may not have allocated one, or the catalog may simply not exist
// yet in a cluster that has never ingested a layer. Every one of those states
// has to degrade into "this node unpacks images the ordinary way", not into a
// snapshotter that refuses to run containers.
var errNotReady = errors.New("catalog is not open")

// holder owns the catalog store and swaps it as the node's image devices come
// and go.
//
// It exists because of a hard ordering constraint: containerd will not start
// pods until the snapshotter socket answers, and the snapshotter cannot reach
// RACER until racer-ctrl has published a device, which on a fresh node happens
// after containerd is up. So the daemon serves immediately with no catalog and
// attaches one later. Until then every lookup misses and the snapshotter
// behaves exactly like the overlayfs snapshotter, which is the correct
// fallback rather than a degraded one.
type holder struct {
	log     *slog.Logger
	format  bool
	adopt   bool
	blocks  uint32
	errnos  []unix.Errno
	current atomicStore

	// open opens the catalog device. It is a field only so tests can attach
	// a catalog to an ordinary file: the production path uses O_DIRECT,
	// which the filesystem a test's temporary directory lives on may not
	// support.
	open func(path string) (*catalog.Device, error)
}

// openDevice opens the catalog device with the configured conflict errnos.
func (h *holder) openDevice(path string) (*catalog.Device, error) {
	if h.open != nil {
		return h.open(path)
	}

	return catalog.OpenDevice(path, catalog.DeviceOptions{ConflictErrnos: h.errnos})
}

// Ensure the holder satisfies both of the interfaces the stack needs. The read
// side is on the container start path and the write side is on the ingest
// path, and both have to survive a missing device.
var (
	_ snapshotter.Catalog = (*holder)(nil)
	_ ingest.Catalog      = (*holder)(nil)
)

// atomicStore is the attached catalog and the device under it, swapped as a
// unit so a reader never sees a store whose device has been closed.
type atomicStore struct {
	mu    sync.RWMutex
	store *catalog.Store
	dev   *catalog.Device
	path  string
}

func (a *atomicStore) load() (*catalog.Store, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return a.store, a.path
}

// swap installs a new store and closes the device the old one used. The old
// store is dropped rather than drained: its only state is an in-memory index
// rebuilt from the new device.
func (a *atomicStore) swap(store *catalog.Store, dev *catalog.Device, path string) {
	a.mu.Lock()
	old := a.dev
	a.store, a.dev, a.path = store, dev, path
	a.mu.Unlock()

	if old != nil {
		_ = old.Close() //nolint:errcheck // best effort on a replaced device
	}
}

func (a *atomicStore) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	dev := a.dev
	a.store, a.dev, a.path = nil, nil, ""

	if dev == nil {
		return nil
	}

	return dev.Close()
}

// reconcile attaches the catalog described by set, replacing whatever is
// attached now if the device changed.
//
// It is safe to call repeatedly with the same set: reopening the same device
// is refused early, and every catalog mutation it performs is idempotent.
func (h *holder) reconcile(set *segment.Set) error {
	if set == nil {
		return errNotReady
	}

	desc, err := set.CatalogDevice()
	if err != nil {
		return fmt.Errorf("catalog device: %w", err)
	}

	if store, path := h.current.load(); store != nil && path == desc.Device {
		return h.adoptSegments(store, set)
	}

	dev, err := h.openDevice(desc.Device)
	if err != nil {
		return err
	}

	store, err := h.openStore(dev, desc)
	if err != nil {
		_ = dev.Close() //nolint:errcheck // the open already failed

		return err
	}

	h.log.Info("catalog attached",
		slog.String("device", desc.Device),
		slog.Uint64("generation", store.Superblock().Generation),
		slog.Int("records", store.Len()),
	)

	h.current.swap(store, dev, desc.Device)

	return h.adoptSegments(store, set)
}

// openStore opens the catalog on dev, formatting it first when it is blank and
// this node is allowed to.
func (h *holder) openStore(dev *catalog.Device, desc segment.Catalog) (*catalog.Store, error) {
	store, err := catalog.Open(dev)
	if err == nil {
		return store, nil
	}

	if !errors.Is(err, catalog.ErrUnformatted) {
		return nil, err
	}

	if !h.format {
		return nil, fmt.Errorf("%w (pass -format-catalog to create one)", err)
	}

	h.log.Info("formatting a blank catalog",
		slog.String("device", desc.Device),
		slog.Uint64("bytes", desc.Bytes),
	)

	// Two nodes that boot together can both find the device blank. That is
	// fine and needs no coordination: the format is a compare-and-swap on
	// block zero, so exactly one of them lands and the loser is told it
	// conflicted and simply opens what the winner wrote.
	if err := catalog.Format(dev, catalog.FormatOptions{Bytes: desc.Bytes, SegmentBlocks: h.blocks}); err != nil {
		if !errors.Is(err, catalog.ErrConflict) {
			return nil, err
		}

		h.log.Info("another node formatted the catalog first")
	}

	return catalog.Open(dev)
}

// adoptSegments registers the segments this node can see in the catalog's
// segment table and makes sure one of them is open for appends.
//
// The operator allocates extents and publishes them; something still has to
// tell the catalog how large each segment is and which one ingest appends to.
// Doing it from every node looks reckless and is not: AddSegment refuses to
// change a segment it already knows, SetOpenSegment is a no-op when the
// segment is already open, and both are compare-and-swaps, so the worst
// outcome of a race is a retry.
func (h *holder) adoptSegments(store *catalog.Store, set *segment.Set) error {
	if !h.adopt {
		return nil
	}

	segments := make([]segment.Segment, len(set.Segments))
	copy(segments, set.Segments)
	sort.Slice(segments, func(i, j int) bool { return segments[i].ID < segments[j].ID })

	known, err := store.Segments()
	if err != nil {
		return fmt.Errorf("read segment table: %w", err)
	}

	have := make(map[uint32]catalog.SegmentEntry, len(known))
	for _, e := range known {
		have[e.ID] = e
	}

	for _, seg := range segments {
		pages := seg.Pages()
		if pages == 0 || pages > uint64(^uint32(0)) {
			continue
		}

		if _, ok := have[seg.ID]; ok {
			continue
		}

		if err := store.AddSegment(seg.ID, uint32(pages)); err != nil { //nolint:gosec // bounded above
			return fmt.Errorf("add segment %d: %w", seg.ID, err)
		}

		h.log.Info("segment registered", slog.Uint64("segment", uint64(seg.ID)), slog.Uint64("pages", pages))
	}

	if store.Superblock().OpenSegment != 0 {
		return nil
	}

	// Nothing is open, so ingest has nowhere to append. Pick the lowest
	// segment with room. Every node picks the same one, so the losers of the
	// compare-and-swap find it already open and stop.
	known, err = store.Segments()
	if err != nil {
		return fmt.Errorf("read segment table: %w", err)
	}

	for _, e := range known {
		if e.State != catalog.SegmentEmpty || e.FreePages() == 0 {
			continue
		}

		if err := store.SetOpenSegment(e.ID); err != nil {
			if errors.Is(err, catalog.ErrConflict) {
				return nil
			}

			return fmt.Errorf("open segment %d: %w", e.ID, err)
		}

		h.log.Info("segment opened for ingest", slog.Uint64("segment", uint64(e.ID)))

		return nil
	}

	return nil
}

// close releases the attached device, if any.
func (h *holder) close() error {
	return h.current.close()
}

// Resolve implements the snapshotter's read path. A missing catalog is a miss,
// never an error: the caller then unpacks the layer locally.
func (h *holder) Resolve(chainID catalog.Digest) (catalog.Blob, bool) {
	store, _ := h.current.load()
	if store == nil {
		return catalog.Blob{}, false
	}

	return store.Resolve(chainID)
}

// Blob implements the snapshotter's read path.
func (h *holder) Blob(diffID catalog.Digest) (catalog.Blob, bool) {
	store, _ := h.current.load()
	if store == nil {
		return catalog.Blob{}, false
	}

	return store.Blob(diffID)
}

// Sync refreshes the in-memory index from the device.
func (h *holder) Sync() (bool, error) {
	store, _ := h.current.load()
	if store == nil {
		return false, errNotReady
	}

	return store.Sync()
}

// Repair retires a hole in the catalog's record slots whose writer never came
// back. See catalog.Store.Repair; this is the crash case Abandon cannot cover.
func (h *holder) Repair(grace time.Duration) (int, error) {
	store, _ := h.current.load()
	if store == nil {
		return 0, errNotReady
	}

	return store.Repair(grace)
}

// Reserve implements the ingest write path.
func (h *holder) Reserve(pages uint32, records int) (catalog.Reservation, error) {
	store, _ := h.current.load()
	if store == nil {
		return catalog.Reservation{}, errNotReady
	}

	return store.Reserve(pages, records)
}

// ReserveRecords implements the ingest write path.
func (h *holder) ReserveRecords(records int) (catalog.Reservation, error) {
	store, _ := h.current.load()
	if store == nil {
		return catalog.Reservation{}, errNotReady
	}

	return store.ReserveRecords(records)
}

// Append implements the ingest write path.
func (h *holder) Append(res catalog.Reservation, records []catalog.Record) error {
	store, _ := h.current.load()
	if store == nil {
		return errNotReady
	}

	return store.Append(res, records)
}

// Abandon implements the ingest write path.
//
// If the store has been swapped out from under an in-flight ingest, the
// reservation belonged to a catalog this node no longer has, and there is
// nothing useful to write. Whoever attaches that catalog next inherits the
// hole; that is the crash case, and the reconcile scan is what covers it.
func (h *holder) Abandon(res catalog.Reservation) error {
	store, _ := h.current.load()
	if store == nil {
		return errNotReady
	}

	return store.Abandon(res)
}

// Account implements the ingest write path.
func (h *holder) Account(id uint32, liveDelta, deadDelta int64) error {
	store, _ := h.current.load()
	if store == nil {
		return errNotReady
	}

	return store.Account(id, liveDelta, deadDelta)
}

// Len reports how many keys the attached catalog resolves, for logging.
func (h *holder) Len() int {
	store, _ := h.current.load()
	if store == nil {
		return 0
	}

	return store.Len()
}
