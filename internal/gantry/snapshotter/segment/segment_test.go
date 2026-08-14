// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package segment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The catalog head is 16 MiB, so the first segment starts there and the
// second one 16 GiB after it. Offsets are what a segment is addressed by now,
// so the fixture carries the real arithmetic rather than round numbers.
const validMap = `{
  "generation": 7,
  "universe": 3,
  "device": "/dev/ublkb200",
  "catalogBytes": 16777216,
  "segments": [
    {"id": 2, "offset": 17196646400, "bytes": 17179869184, "epoch": 0},
    {"id": 1, "offset": 16777216, "bytes": 17179869184, "epoch": 4}
  ]
}`

func TestParseSortsAndIndexes(t *testing.T) {
	set, err := Parse([]byte(validMap))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if set.Generation != 7 || set.Universe != 3 {
		t.Fatalf("got generation %d universe %d, want 7 and 3", set.Generation, set.Universe)
	}

	if set.Segments[0].ID != 1 || set.Segments[1].ID != 2 {
		t.Fatalf("segments not sorted by id: %+v", set.Segments)
	}

	seg, err := set.Segment(2)
	if err != nil {
		t.Fatalf("Segment(2): %v", err)
	}

	if seg.Offset != 16<<20+16<<30 {
		t.Fatalf("got offset %d, want %d", seg.Offset, 16<<20+16<<30)
	}

	if got := seg.Pages(); got != 4096 {
		t.Fatalf("got %d pages, want 4096", got)
	}

	if got := seg.End(); got != seg.Offset+seg.Bytes {
		t.Fatalf("got end %d, want %d", got, seg.Offset+seg.Bytes)
	}

	if first, err := set.Segment(1); err != nil || first.Epoch != 4 {
		t.Fatalf("got segment %+v err %v, want epoch 4", first, err)
	}
}

func TestSegmentUnknown(t *testing.T) {
	set, err := Parse([]byte(validMap))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if _, err := set.Segment(9); !errors.Is(err, ErrUnknownSegment) {
		t.Fatalf("got %v, want ErrUnknownSegment", err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"not json":      `{`,
		"unknown field": `{"generation": 1, "segments": [], "surprise": true}`,
		"per-segment device": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"device":"/dev/y","offset":4194304,"bytes":4194304}]}`,
		"segment id zero": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":0,"offset":4194304,"bytes":4194304}]}`,
		"duplicate id": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"offset":4194304,"bytes":4194304},{"id":1,"offset":8388608,"bytes":4194304}]}`,
		"no device": `{"generation":1,"segments":[{"id":1,"offset":4194304,"bytes":4194304}]}`,
		"zero bytes": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"offset":4194304,"bytes":0}]}`,
		"unaligned bytes": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"offset":4194304,"bytes":4194305}]}`,
		"sub-page capacity": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"offset":4194304,"bytes":4096}]}`,
		"unaligned offset": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"offset":4096,"bytes":4194304}]}`,
		"inside the catalog": `{"generation":1,"device":"/dev/x","catalogBytes":8388608,"segments":` +
			`[{"id":1,"offset":4194304,"bytes":4194304}]}`,
		"overlapping": `{"generation":1,"device":"/dev/x","segments":` +
			`[{"id":1,"offset":4194304,"bytes":8388608},{"id":2,"offset":8388608,"bytes":4194304}]}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestCatalogDevice(t *testing.T) {
	set, err := Parse([]byte(validMap))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cat, err := set.CatalogDevice()
	if err != nil {
		t.Fatalf("CatalogDevice: %v", err)
	}

	// The catalog shares the image device with every segment: it is the
	// volume's mutable head, so it is always at offset zero.
	if cat.Device != "/dev/ublkb200" || cat.Bytes != 16<<20 {
		t.Fatalf("got %+v", cat)
	}

	empty, err := Parse([]byte(`{"generation":1,"segments":[]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if _, err := empty.CatalogDevice(); !errors.Is(err, ErrNoCatalog) {
		t.Fatalf("got %v, want ErrNoCatalog", err)
	}

	unaligned, err := Parse([]byte(`{"generation":1,"device":"/dev/x","catalogBytes":100,"segments":[]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if _, err := unaligned.CatalogDevice(); err == nil {
		t.Fatal("want error for an unaligned catalog, got nil")
	}
}

func TestSegmentRange(t *testing.T) {
	set, err := Parse([]byte(validMap))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	device, offset, length, err := set.SegmentRange(1)
	if err != nil {
		t.Fatalf("SegmentRange: %v", err)
	}

	if device != "/dev/ublkb200" || offset != 16<<20 || length != 16<<30 {
		t.Fatalf("got %q %d %d", device, offset, length)
	}

	if _, _, _, err := set.SegmentRange(9); !errors.Is(err, ErrUnknownSegment) {
		t.Fatalf("got %v, want ErrUnknownSegment", err)
	}
}

func TestPagesFor(t *testing.T) {
	cases := []struct {
		bytes uint64
		pages uint32
	}{
		{0, 0},
		{1, 1},
		{PageBytes - 1, 1},
		{PageBytes, 1},
		{PageBytes + 1, 2},
		{40 * PageBytes, 40},
	}

	for _, c := range cases {
		if got := PagesFor(c.bytes); got != c.pages {
			t.Fatalf("PagesFor(%d) = %d, want %d", c.bytes, got, c.pages)
		}

		if got := PaddedSize(c.bytes); got != uint64(c.pages)*PageBytes {
			t.Fatalf("PaddedSize(%d) = %d, want %d", c.bytes, got, uint64(c.pages)*PageBytes)
		}
	}
}

func TestFingerprintTracksTheGeneration(t *testing.T) {
	addr := Address{Segment: 1, PageOffset: 3, PageCount: 2, ByteLength: PageBytes + 1}

	// Reclamation hands the same pages to a different blob, so a name that
	// did not move with the generation would let a stale dm mapping be
	// adopted for the new occupant.
	want := addr.Fingerprint(1)
	if want == addr.Fingerprint(2) {
		t.Fatal("fingerprint did not change with the generation")
	}

	if again := addr.Fingerprint(1); again != want {
		t.Fatalf("fingerprint is not stable: %s then %s", want, again)
	}
}

func TestAddressValidate(t *testing.T) {
	ok := Address{Segment: 1, PageOffset: 0, PageCount: 2, ByteLength: PageBytes + 17}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	bad := map[string]Address{
		"no pages":       {Segment: 1, PageCount: 0, ByteLength: 10},
		"no bytes":       {Segment: 1, PageCount: 1, ByteLength: 0},
		"length > span":  {Segment: 1, PageCount: 1, ByteLength: PageBytes + 1},
		"padding page":   {Segment: 1, PageCount: 3, ByteLength: PageBytes + 1},
		"page overflow":  {Segment: 1, PageOffset: ^uint32(0), PageCount: 2, ByteLength: 10},
		"exact boundary": {Segment: 1, PageCount: 2, ByteLength: 2*PageBytes + 1},
	}

	for name, addr := range bad {
		t.Run(name, func(t *testing.T) {
			if err := addr.Validate(); err == nil {
				t.Fatalf("want error for %+v, got nil", addr)
			}
		})
	}
}

func TestLocate(t *testing.T) {
	set, err := Parse([]byte(validMap))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	device, offset, length, err := set.Locate(Address{
		Segment: 1, PageOffset: 3, PageCount: 2, ByteLength: PageBytes + 1,
	})
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}

	if device != "/dev/ublkb200" {
		t.Fatalf("got device %q", device)
	}

	// The segment's own offset plus the blob's offset within it: an address
	// is relative to its segment and the device is one flat space.
	if want := uint64(16<<20) + 3*PageBytes; offset != want {
		t.Fatalf("got offset %d, want %d", offset, want)
	}

	if length != 2*PageBytes {
		t.Fatalf("got length %d, want %d", length, 2*PageBytes)
	}
}

func TestLocatePastEndOfSegment(t *testing.T) {
	set, err := Parse([]byte(validMap))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	_, _, _, err = set.Locate(Address{
		Segment: 1, PageOffset: 4095, PageCount: 2, ByteLength: PageBytes + 1,
	})
	if err == nil {
		t.Fatal("want error for a blob running past the segment, got nil")
	}
}

func TestWatcherRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-devices.json")

	changes := 0
	w := NewWatcher(WatcherOptions{Path: path, OnChange: func(*Set) { changes++ }})

	if err := w.Refresh(); err == nil {
		t.Fatal("want an error for a missing file")
	}

	if !Missing(w.Err()) {
		t.Fatalf("want a not-exist error, got %v", w.Err())
	}

	if w.Current() != nil {
		t.Fatal("want no mapping before the first successful load")
	}

	writeFile(t, path, validMap)

	if err := w.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if w.Current() == nil || w.Current().Generation != 7 {
		t.Fatalf("got %+v", w.Current())
	}

	if changes != 1 {
		t.Fatalf("got %d change callbacks, want 1", changes)
	}

	// The same generation again must not re-publish: racer-ctrl rewrites by
	// rename, so identical content shows up repeatedly and every consumer
	// would redo its work for nothing.
	if err := w.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if changes != 1 {
		t.Fatalf("got %d change callbacks after a repeat load, want 1", changes)
	}
}

func TestWatcherRefusesOlderGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-devices.json")
	writeFile(t, path, validMap)

	w := NewWatcher(WatcherOptions{Path: path})
	if err := w.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	writeFile(t, path, `{"generation":3,"device":"/dev/other","catalogBytes":4096,"segments":[]}`)

	if err := w.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if got := w.Current().Generation; got != 7 {
		t.Fatalf("adopted generation %d, want to have kept 7", got)
	}
}

func TestWatcherRunStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image-devices.json")
	writeFile(t, path, validMap)

	w := NewWatcher(WatcherOptions{Path: path, Interval: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	if w.Current() == nil {
		t.Fatal("Run did not load the mapping before returning")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
