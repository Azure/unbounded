// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package blockmap turns a blob's address in a RACER segment into a mounted
// read-only filesystem on the node.
//
// A layer's bytes live at some page range of some segment, and a segment is a
// whole RACER device. Two steps get from one to the other:
//
//  1. a dm-linear target over the segment's device, offset to the blob, which
//     gives the layer its own block device without copying anything, and
//  2. a read-only EROFS mount of that device.
//
// Both steps are idempotent and both are keyed by a name derived from the
// layer's digest and its address, so the state on the host describes itself.
// That matters because dm devices and mounts outlive the process: after a
// restart there is nothing to reload, the wanted set is recomputed from the
// snapshot metadata and Prune removes whatever is not in it.
//
// Neither step touches the layer's contents, so the cost of the first container
// to use a layer is one ioctl and one mount, and the cost of every container
// after that is nothing at all: Ensure finds the mount already there.
package blockmap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// SectorBytes is the unit device mapper tables are written in.
const SectorBytes = 512

// DefaultRoot is where layer mounts live. It is deliberately short: every
// mounted layer contributes its path to the overlay mount's option string,
// and that string has to fit in a page.
const DefaultRoot = "/run/gantry-snapshotter/l"

// DefaultPrefix names the device mapper devices this package owns. Anything
// under it is fair game for Prune, so it must not collide with another
// component's devices.
const DefaultPrefix = "gsnap-"

// ErrNoSegments reports that the node does not yet know which devices carry
// which segments, which is the normal state before racer-ctrl has published
// the image devices.
var ErrNoSegments = errors.New("blockmap: no image devices published yet")

// Table is a single-target dm-linear table: the whole logical device maps to a
// contiguous run of sectors on one backing device.
type Table struct {
	// Device is the backing block device, the RACER device carrying the
	// segment.
	Device string

	// StartSector is where the blob starts on the backing device.
	StartSector uint64

	// Sectors is the length of the mapping.
	Sectors uint64
}

// Validate checks that the table describes a mapping device mapper will accept.
func (t Table) Validate() error {
	if t.Device == "" {
		return errors.New("blockmap: table with no backing device")
	}

	if t.Sectors == 0 {
		return errors.New("blockmap: table with no sectors")
	}

	return nil
}

// String renders the table in dmsetup's syntax.
func (t Table) String() string {
	return fmt.Sprintf("0 %d linear %s %d", t.Sectors, t.Device, t.StartSector)
}

// TableFor is the table that exposes a blob at the given byte range of a
// device.
//
// The mapping covers the blob's whole page span rather than just its bytes.
// The tail of the last page is padding, but leaving it out would put the end of
// the device in the middle of a RACER page, and readahead running off the end
// of a device costs a failed read on the container start path.
func TableFor(device string, offset, length uint64) (Table, error) {
	if offset%SectorBytes != 0 || length%SectorBytes != 0 {
		return Table{}, fmt.Errorf("blockmap: blob at %d for %d bytes is not sector aligned", offset, length)
	}

	t := Table{
		Device:      device,
		StartSector: offset / SectorBytes,
		Sectors:     length / SectorBytes,
	}

	return t, t.Validate()
}

// Devmapper is the subset of device mapper this package needs. It is an
// interface so the mapping logic is testable without root.
type Devmapper interface {
	// Create makes a read-only device with the given table. It reports an
	// error if the name is taken.
	Create(ctx context.Context, name string, table Table) error

	// Remove tears a device down. Removing a device that does not exist
	// succeeds.
	Remove(ctx context.Context, name string) error

	// Exists reports whether a device of that name is present.
	Exists(ctx context.Context, name string) (bool, error)

	// Names lists existing devices whose names start with prefix.
	Names(ctx context.Context, prefix string) ([]string, error)

	// Path is where the named device appears in the filesystem.
	Path(name string) string
}

// Mounter is the subset of mounting this package needs.
type Mounter interface {
	// Mount mounts source at target read-only.
	Mount(ctx context.Context, source, target string) error

	// Unmount unmounts target. Unmounting something that is not mounted
	// succeeds.
	Unmount(ctx context.Context, target string) error

	// Mounted reports whether target already carries this package's
	// filesystem.
	Mounted(target string) (bool, error)
}

// Locator resolves a blob's address to a device and byte range.
type Locator interface {
	Locate(addr segment.Address) (device string, offset, length uint64, err error)
}

// WatcherLocator resolves addresses against whatever segment set the node last
// published, so a device added while the process is running is picked up
// without a restart.
type WatcherLocator struct {
	Watcher *segment.Watcher
}

// Locate implements Locator.
func (l WatcherLocator) Locate(addr segment.Address) (string, uint64, uint64, error) {
	set := l.Watcher.Current()
	if set == nil {
		if err := l.Watcher.Err(); err != nil {
			return "", 0, 0, fmt.Errorf("%w: %w", ErrNoSegments, err)
		}

		return "", 0, 0, ErrNoSegments
	}

	return set.Locate(addr)
}

// Options configures a Map.
type Options struct {
	// Root is the directory layer mounts are made under.
	Root string

	// Prefix names the device mapper devices the Map owns.
	Prefix string

	// Locator resolves addresses to devices.
	Locator Locator

	// Devmapper and Mounter are the host interfaces.
	Devmapper Devmapper
	Mounter   Mounter
}

// Map keeps the node's layer mounts in the state the snapshotter asks for.
type Map struct {
	root    string
	prefix  string
	locator Locator
	dm      Devmapper
	mounter Mounter

	// keys serializes work per layer rather than globally. Pulling an image
	// asks for forty layers at once, and making them queue behind each other
	// would turn forty parallel ioctls into a serial chain on the container
	// start path.
	mu   sync.Mutex
	keys map[string]*sync.Mutex
}

// New builds a Map.
func New(opts Options) (*Map, error) {
	if opts.Locator == nil {
		return nil, errors.New("blockmap: no locator")
	}

	if opts.Devmapper == nil {
		return nil, errors.New("blockmap: no devmapper")
	}

	if opts.Mounter == nil {
		return nil, errors.New("blockmap: no mounter")
	}

	m := &Map{
		root:    orDefault(opts.Root, DefaultRoot),
		prefix:  orDefault(opts.Prefix, DefaultPrefix),
		locator: opts.Locator,
		dm:      opts.Devmapper,
		mounter: opts.Mounter,
		keys:    make(map[string]*sync.Mutex),
	}

	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return nil, fmt.Errorf("blockmap: create %s: %w", m.root, err)
	}

	return m, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}

	return v
}

// Name is the device mapper name and mount directory for a layer at an address.
//
// The address is part of the name on purpose. If the cleaner relocates a blob,
// the name changes, so the stale device cannot be mistaken for the new one and
// Prune removes it on the next pass. Names are kept short because each one ends
// up in an overlay mount's option string.
func (m *Map) Name(layer catalog.Digest, addr segment.Address) string {
	return m.prefix + layer.Short() + addr.Fingerprint()
}

// Path is where a named device is mounted.
func (m *Map) Path(name string) string { return filepath.Join(m.root, name) }

// Root is the directory layer mounts live under.
func (m *Map) Root() string { return m.root }

// Ensure maps and mounts a layer, returning the directory it is mounted at.
//
// It is idempotent and cheap when the layer is already mounted, which is the
// common case: this runs once per layer per node, and every container after the
// first finds the work already done.
func (m *Map) Ensure(ctx context.Context, layer catalog.Digest, addr segment.Address) (string, error) {
	device, offset, length, err := m.locator.Locate(addr)
	if err != nil {
		return "", err
	}

	table, err := TableFor(device, offset, length)
	if err != nil {
		return "", err
	}

	name := m.Name(layer, addr)
	target := m.Path(name)

	unlock := m.lock(name)
	defer unlock()

	// Check the mount first. In steady state it is there and this is the
	// only syscall the whole call makes.
	mounted, err := m.mounter.Mounted(target)
	if err != nil {
		return "", err
	}

	if mounted {
		return target, nil
	}

	exists, err := m.dm.Exists(ctx, name)
	if err != nil {
		return "", err
	}

	if !exists {
		if err := m.dm.Create(ctx, name, table); err != nil {
			return "", fmt.Errorf("blockmap: map layer %s: %w", layer.Short(), err)
		}
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", fmt.Errorf("blockmap: create %s: %w", target, err)
	}

	if err := m.mounter.Mount(ctx, m.dm.Path(name), target); err != nil {
		// Leave the device in place. Either the next Ensure retries the
		// mount against it, or Prune removes it; tearing it down here
		// would race another goroutine that is about to use it.
		return "", fmt.Errorf("blockmap: mount layer %s: %w", layer.Short(), err)
	}

	return target, nil
}

// Prune removes every mapping this package owns that is not in keep.
//
// keep is the set of names the snapshotter still has snapshots for, which is
// recomputed from durable metadata rather than tracked in memory, so a restart
// converges instead of leaking. A layer that is busy is left alone and picked
// up by a later pass.
func (m *Map) Prune(ctx context.Context, keep map[string]struct{}) error {
	names, err := m.dm.Names(ctx, m.prefix)
	if err != nil {
		return err
	}

	// Directories under the root are mappings too, and are what is left
	// behind if the process died between the mount and the device teardown.
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return fmt.Errorf("blockmap: read %s: %w", m.root, err)
	}

	seen := make(map[string]struct{}, len(names))

	for _, name := range names {
		seen[name] = struct{}{}
	}

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), m.prefix) {
			continue
		}

		if _, ok := seen[e.Name()]; !ok {
			names = append(names, e.Name())
		}
	}

	var errs []error

	for _, name := range names {
		if _, wanted := keep[name]; wanted {
			continue
		}

		if err := m.release(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Release tears down one layer's mapping.
func (m *Map) Release(ctx context.Context, layer catalog.Digest, addr segment.Address) error {
	return m.release(ctx, m.Name(layer, addr))
}

func (m *Map) release(ctx context.Context, name string) error {
	unlock := m.lock(name)
	defer unlock()

	target := m.Path(name)

	if err := m.mounter.Unmount(ctx, target); err != nil {
		return fmt.Errorf("blockmap: unmount %s: %w", target, err)
	}

	// The device goes only after the mount is gone, otherwise the removal
	// fails on a busy device and leaves a mount with no backing.
	if err := m.dm.Remove(ctx, name); err != nil {
		return fmt.Errorf("blockmap: remove %s: %w", name, err)
	}

	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blockmap: remove %s: %w", target, err)
	}

	return nil
}

func (m *Map) lock(name string) func() {
	m.mu.Lock()

	mu, ok := m.keys[name]
	if !ok {
		mu = &sync.Mutex{}
		m.keys[name] = mu
	}

	m.mu.Unlock()

	mu.Lock()

	return mu.Unlock
}
