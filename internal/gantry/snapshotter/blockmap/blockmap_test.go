// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package blockmap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// fakeDM is device mapper with the parts that matter kept: names are unique,
// a device exists until it is removed, and the table it was created with is
// remembered so a test can check what was mapped.
type fakeDM struct {
	mu      sync.Mutex
	tables  map[string]Table
	creates int
	removes int

	// createErr, if set, fails the next create.
	createErr error
}

func newFakeDM() *fakeDM { return &fakeDM{tables: make(map[string]Table)} }

func (d *fakeDM) Create(_ context.Context, name string, table Table) error {
	if err := validName(name); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.createErr != nil {
		err := d.createErr
		d.createErr = nil

		return err
	}

	if _, ok := d.tables[name]; ok {
		return fmt.Errorf("device %s exists", name)
	}

	d.tables[name] = table
	d.creates++

	return nil
}

func (d *fakeDM) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.tables[name]; ok {
		d.removes++
	}

	delete(d.tables, name)

	return nil
}

func (d *fakeDM) Exists(_ context.Context, name string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	_, ok := d.tables[name]

	return ok, nil
}

func (d *fakeDM) Names(_ context.Context, prefix string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var names []string

	for name := range d.tables {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names, nil
}

func (d *fakeDM) Path(name string) string { return "/dev/mapper/" + name }

type fakeMounter struct {
	mu     sync.Mutex
	mounts map[string]string
	count  int

	mountErr error
}

func newFakeMounter() *fakeMounter { return &fakeMounter{mounts: make(map[string]string)} }

func (m *fakeMounter) Mount(_ context.Context, source, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mountErr != nil {
		err := m.mountErr
		m.mountErr = nil

		return err
	}

	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("mount target: %w", err)
	}

	m.mounts[target] = source
	m.count++

	return nil
}

func (m *fakeMounter) Unmount(_ context.Context, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.mounts, target)

	return nil
}

func (m *fakeMounter) Mounted(target string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.mounts[target]

	return ok, nil
}

// locator is a fixed segment set, which is what a node with published image
// devices looks like.
type locator struct {
	set *segment.Set
	err error
}

func (l locator) Locate(addr segment.Address) (string, uint64, uint64, error) {
	if l.err != nil {
		return "", 0, 0, l.err
	}

	return l.set.Locate(addr)
}

func testSet(t *testing.T) *segment.Set {
	t.Helper()

	set, err := segment.Parse([]byte(`{
		"generation": 3,
		"universe": 7,
		"catalog": {"device": "/dev/ublkb1", "bytes": 268435456},
		"segments": [
			{"id": 1, "device": "/dev/ublkb2", "bytes": 17179869184},
			{"id": 2, "device": "/dev/ublkb3", "bytes": 17179869184}
		]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	return set
}

func newMap(t *testing.T) (*Map, *fakeDM, *fakeMounter) {
	t.Helper()

	dm := newFakeDM()
	mounter := newFakeMounter()

	m, err := New(Options{
		Root:      filepath.Join(t.TempDir(), "l"),
		Locator:   locator{set: testSet(t)},
		Devmapper: dm,
		Mounter:   mounter,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return m, dm, mounter
}

func digest(b byte) catalog.Digest {
	var d catalog.Digest

	for i := range d {
		d[i] = b
	}

	return d
}

// addr is a blob of n pages at page offset off of segment 1, sized so that the
// tail padding is less than a page.
func addr(off, n uint32) segment.Address {
	return segment.Address{
		Segment:    1,
		PageOffset: off,
		PageCount:  n,
		ByteLength: uint64(n)*segment.PageBytes - 4096,
	}
}

func TestTableFor(t *testing.T) {
	table, err := TableFor("/dev/ublkb2", 8*segment.PageBytes, 2*segment.PageBytes)
	if err != nil {
		t.Fatalf("TableFor: %v", err)
	}

	want := Table{Device: "/dev/ublkb2", StartSector: 8 * segment.PageBytes / SectorBytes, Sectors: 2 * segment.PageBytes / SectorBytes}
	if table != want {
		t.Fatalf("got %+v, want %+v", table, want)
	}

	if got := table.String(); got != "0 16384 linear /dev/ublkb2 65536" {
		t.Fatalf("table line is %q", got)
	}
}

func TestTableForRejects(t *testing.T) {
	if _, err := TableFor("/dev/ublkb2", 1, segment.PageBytes); err == nil {
		t.Fatal("want an error for an unaligned offset")
	}

	if _, err := TableFor("/dev/ublkb2", 0, 513); err == nil {
		t.Fatal("want an error for an unaligned length")
	}

	if _, err := TableFor("", 0, segment.PageBytes); err == nil {
		t.Fatal("want an error for no backing device")
	}

	if _, err := TableFor("/dev/ublkb2", 0, 0); err == nil {
		t.Fatal("want an error for a zero length mapping")
	}
}

func TestEnsureMapsAndMounts(t *testing.T) {
	m, dm, mounter := newMap(t)

	a := addr(4, 2)

	path, err := m.Ensure(t.Context(), digest(1), a)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	name := m.Name(digest(1), a)
	if path != filepath.Join(m.Root(), name) {
		t.Fatalf("mounted at %s", path)
	}

	// The mapping must cover the blob's whole page span, not just its bytes,
	// so readahead in the last page stays inside the device.
	table := dm.tables[name]
	want := Table{
		Device:      "/dev/ublkb2",
		StartSector: 4 * segment.PageBytes / SectorBytes,
		Sectors:     2 * segment.PageBytes / SectorBytes,
	}

	if table != want {
		t.Fatalf("table %+v, want %+v", table, want)
	}

	if mounter.mounts[path] != "/dev/mapper/"+name {
		t.Fatalf("mounted %q", mounter.mounts[path])
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	m, dm, mounter := newMap(t)

	a := addr(0, 1)

	first, err := m.Ensure(t.Context(), digest(1), a)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for range 4 {
		again, err := m.Ensure(t.Context(), digest(1), a)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}

		if again != first {
			t.Fatalf("got %s then %s", first, again)
		}
	}

	// Every container after the first has to find the work already done.
	if dm.creates != 1 || mounter.count != 1 {
		t.Fatalf("%d creates and %d mounts for five calls", dm.creates, mounter.count)
	}
}

func TestEnsureAdoptsAnOrphanedDevice(t *testing.T) {
	m, dm, mounter := newMap(t)

	a := addr(0, 1)
	name := m.Name(digest(1), a)

	// A crash between the device create and the mount leaves this behind.
	if err := dm.Create(t.Context(), name, Table{Device: "/dev/ublkb2", Sectors: 8192}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := m.Ensure(t.Context(), digest(1), a); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if dm.creates != 1 {
		t.Fatalf("the existing device was recreated: %d creates", dm.creates)
	}

	if mounter.count != 1 {
		t.Fatalf("%d mounts, want 1", mounter.count)
	}
}

func TestEnsureKeepsTheDeviceWhenMountFails(t *testing.T) {
	m, dm, mounter := newMap(t)

	mounter.mountErr = errors.New("no erofs support")

	a := addr(0, 1)

	if _, err := m.Ensure(t.Context(), digest(1), a); err == nil {
		t.Fatal("want an error when the mount fails")
	}

	// Tearing the device down here would race another goroutine that is
	// about to mount it, and the next Ensure retries against it anyway.
	if _, ok := dm.tables[m.Name(digest(1), a)]; !ok {
		t.Fatal("a failed mount tore down the device")
	}

	if _, err := m.Ensure(t.Context(), digest(1), a); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if dm.creates != 1 {
		t.Fatalf("%d creates, want 1", dm.creates)
	}
}

func TestEnsureUnknownSegment(t *testing.T) {
	m, _, _ := newMap(t)

	_, err := m.Ensure(t.Context(), digest(1), segment.Address{
		Segment: 9, PageOffset: 0, PageCount: 1, ByteLength: 4096,
	})
	if !errors.Is(err, segment.ErrUnknownSegment) {
		t.Fatalf("got %v, want ErrUnknownSegment", err)
	}
}

func TestEnsureWithoutImageDevices(t *testing.T) {
	dm := newFakeDM()

	m, err := New(Options{
		Root:      filepath.Join(t.TempDir(), "l"),
		Locator:   locator{err: ErrNoSegments},
		Devmapper: dm,
		Mounter:   newFakeMounter(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := m.Ensure(t.Context(), digest(1), addr(0, 1)); !errors.Is(err, ErrNoSegments) {
		t.Fatalf("got %v, want ErrNoSegments", err)
	}

	if dm.creates != 0 {
		t.Fatal("a device was created before the address could be resolved")
	}
}

func TestNameTracksTheAddress(t *testing.T) {
	m, _, _ := newMap(t)

	// The cleaner relocating a blob has to produce a different name, or the
	// stale mapping would be mistaken for the new one.
	moved := m.Name(digest(1), addr(0, 1))
	if same := m.Name(digest(1), addr(8, 1)); same == moved {
		t.Fatalf("both placements are named %s", same)
	}

	if other := m.Name(digest(2), addr(0, 1)); other == moved {
		t.Fatalf("two layers are named %s", other)
	}

	if err := validName(moved); err != nil {
		t.Fatalf("the generated name is not usable: %v", err)
	}

	if !strings.HasPrefix(moved, DefaultPrefix) {
		t.Fatalf("name %q does not carry the prefix Prune sweeps by", moved)
	}
}

func TestPrune(t *testing.T) {
	m, dm, mounter := newMap(t)

	keepAddr := addr(0, 1)
	dropAddr := addr(1, 1)

	if _, err := m.Ensure(t.Context(), digest(1), keepAddr); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if _, err := m.Ensure(t.Context(), digest(2), dropAddr); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	keep := map[string]struct{}{m.Name(digest(1), keepAddr): {}}

	if err := m.Prune(t.Context(), keep); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	dropped := m.Name(digest(2), dropAddr)

	if _, ok := dm.tables[dropped]; ok {
		t.Fatal("an unreferenced device survived")
	}

	if _, err := os.Stat(filepath.Join(m.Root(), dropped)); !os.IsNotExist(err) {
		t.Fatalf("the mount directory survived: %v", err)
	}

	kept := m.Name(digest(1), keepAddr)

	if _, ok := dm.tables[kept]; !ok {
		t.Fatal("a referenced device was removed")
	}

	if ok, _ := mounter.Mounted(m.Path(kept)); !ok {
		t.Fatal("a referenced layer was unmounted")
	}
}

func TestPruneSweepsOrphanDirectories(t *testing.T) {
	m, _, _ := newMap(t)

	// A directory with no device is what a crash between the mkdir and the
	// mount leaves behind. Nothing else will ever remove it.
	orphan := filepath.Join(m.Root(), DefaultPrefix+"deadbeef0000")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Something that is not ours must survive.
	foreign := filepath.Join(m.Root(), "other")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := m.Prune(t.Context(), nil); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("the orphan survived: %v", err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("Prune removed a directory it does not own: %v", err)
	}
}

func TestRelease(t *testing.T) {
	m, dm, mounter := newMap(t)

	a := addr(0, 1)

	path, err := m.Ensure(t.Context(), digest(1), a)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := m.Release(t.Context(), digest(1), a); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if ok, _ := mounter.Mounted(path); ok {
		t.Fatal("still mounted")
	}

	if _, ok := dm.tables[m.Name(digest(1), a)]; ok {
		t.Fatal("the device survived")
	}

	// Releasing twice is how a retried teardown behaves and must succeed.
	if err := m.Release(t.Context(), digest(1), a); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	// And the layer can come back.
	if _, err := m.Ensure(t.Context(), digest(1), a); err != nil {
		t.Fatalf("Ensure after Release: %v", err)
	}
}

func TestEnsureIsConcurrencySafe(t *testing.T) {
	m, dm, mounter := newMap(t)

	var wg sync.WaitGroup

	errs := make([]error, 16)

	for i := range errs {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, errs[i] = m.Ensure(t.Context(), digest(1), addr(0, 1))
		}()
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}

	// Sixteen containers starting at once on the same layer is one mapping.
	if dm.creates != 1 || mounter.count != 1 {
		t.Fatalf("%d creates and %d mounts", dm.creates, mounter.count)
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"", strings.Repeat("a", 128), "gsnap-a b", "gsnap-a/b", "gsnap-a;rm"} {
		if err := validName(name); err == nil {
			t.Fatalf("want an error for %q", name)
		}
	}

	if err := validName("gsnap-0123456789ab_.-A"); err != nil {
		t.Fatalf("validName: %v", err)
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "l")

	for name, opts := range map[string]Options{
		"no locator":   {Root: root, Devmapper: newFakeDM(), Mounter: newFakeMounter()},
		"no devmapper": {Root: root, Locator: locator{}, Mounter: newFakeMounter()},
		"no mounter":   {Root: root, Locator: locator{}, Devmapper: newFakeDM()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
