// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func noDirect() *bool {
	off := false
	return &off
}

func newDeviceFile(t *testing.T, blocks int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "catalog.img")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.Truncate(int64(blocks * BlockBytes)); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	return path
}

func TestAlignedBlock(t *testing.T) {
	for range 32 {
		b := alignedBlock()
		if len(b) != BlockBytes || cap(b) != BlockBytes {
			t.Fatalf("len=%d cap=%d, want %d", len(b), cap(b), BlockBytes)
		}
	}
}

func TestOpenDeviceRequiresAPath(t *testing.T) {
	if _, err := OpenDevice("", DeviceOptions{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOpenDeviceMissingFile(t *testing.T) {
	if _, err := OpenDevice(filepath.Join(t.TempDir(), "absent"), DeviceOptions{Direct: noDirect()}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDeviceRoundTrip(t *testing.T) {
	dev, err := OpenDevice(newDeviceFile(t, 4), DeviceOptions{Direct: noDirect()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dev.Close() //nolint:errcheck // test cleanup

	if dev.Name() == "" {
		t.Fatal("expected a device name")
	}

	want := make([]byte, BlockBytes)
	for i := range want {
		want[i] = byte(i)
	}

	if _, err := dev.WriteAt(want, 2*BlockBytes); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, BlockBytes)
	if _, err := dev.ReadAt(got, 2*BlockBytes); err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(got) != string(want) {
		t.Fatal("read back different bytes")
	}

	// The block before must be untouched: a bounce buffer that leaked
	// across accesses would show up here.
	if _, err := dev.ReadAt(got, BlockBytes); err != nil {
		t.Fatalf("read: %v", err)
	}

	for _, b := range got {
		if b != 0 {
			t.Fatal("neighbouring block was modified")
		}
	}
}

func TestDeviceRejectsUnalignedAccess(t *testing.T) {
	dev, err := OpenDevice(newDeviceFile(t, 4), DeviceOptions{Direct: noDirect()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dev.Close() //nolint:errcheck // test cleanup

	block := make([]byte, BlockBytes)

	cases := []struct {
		name string
		buf  []byte
		off  int64
	}{
		{"short buffer", make([]byte, 512), 0},
		{"long buffer", make([]byte, 2*BlockBytes), 0},
		{"unaligned offset", block, 512},
		{"negative offset", block, -BlockBytes},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := dev.ReadAt(tc.buf, tc.off); err == nil {
				t.Fatal("expected a read error")
			}

			if _, err := dev.WriteAt(tc.buf, tc.off); err == nil {
				t.Fatal("expected a write error")
			}
		})
	}
}

func TestDeviceReadPastTheEnd(t *testing.T) {
	dev, err := OpenDevice(newDeviceFile(t, 1), DeviceOptions{Direct: noDirect()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dev.Close() //nolint:errcheck // test cleanup

	if _, err := dev.ReadAt(make([]byte, BlockBytes), 8*BlockBytes); err == nil {
		t.Fatal("expected a short read error")
	}
}

func TestDeviceTranslatesConflicts(t *testing.T) {
	dev := &Device{conflicts: DefaultConflictErrnos}

	if err := dev.translate(unix.EAGAIN); !errors.Is(err, ErrConflict) {
		t.Fatalf("EAGAIN: got %v, want a conflict", err)
	}

	if err := dev.translate(unix.EBUSY); !errors.Is(err, ErrConflict) {
		t.Fatalf("EBUSY: got %v, want a conflict", err)
	}

	if err := dev.translate(unix.EIO); errors.Is(err, ErrConflict) {
		t.Fatal("EIO must not be a conflict under the defaults")
	}

	plain := errors.New("boom")
	if err := dev.translate(plain); !errors.Is(err, plain) {
		t.Fatalf("got %v, want the original error", err)
	}
}

func TestParseConflictErrnos(t *testing.T) {
	got, err := ParseConflictErrnos("")
	if err != nil {
		t.Fatalf("empty: %v", err)
	}

	if len(got) != len(DefaultConflictErrnos) {
		t.Fatalf("empty: got %v, want the defaults", got)
	}

	got, err = ParseConflictErrnos(" eio , 11 ,")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(got) != 2 || got[0] != unix.EIO || got[1] != unix.Errno(11) {
		t.Fatalf("got %v", got)
	}

	if _, err := ParseConflictErrnos("ENOSUCHTHING"); err == nil {
		t.Fatal("expected an error")
	}

	if _, err := ParseConflictErrnos("-3"); err == nil {
		t.Fatal("expected an error")
	}

	// A list that is only separators falls back rather than yielding a
	// device that never recognises a conflict.
	got, err = ParseConflictErrnos(",,")
	if err != nil {
		t.Fatalf("separators: %v", err)
	}

	if len(got) != len(DefaultConflictErrnos) {
		t.Fatalf("separators: got %v, want the defaults", got)
	}
}

func TestDeviceCloseIsSafe(t *testing.T) {
	var dev *Device
	if err := dev.Close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}

	if dev.Name() != "" {
		t.Fatal("expected an empty name")
	}
}

func TestDeviceWithAStoreOnTop(t *testing.T) {
	noSleep(t)

	path := newDeviceFile(t, 64)

	dev, err := OpenDevice(path, DeviceOptions{Direct: noDirect()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dev.Close() //nolint:errcheck // test cleanup

	if err := Format(dev, FormatOptions{Bytes: 64 * BlockBytes}); err != nil {
		t.Fatalf("format: %v", err)
	}

	store, err := Open(dev)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	if err := store.AddSegment(1, 64, 0); err != nil {
		t.Fatalf("add segment: %v", err)
	}

	if err := store.SetOpenSegment(1); err != nil {
		t.Fatalf("open segment: %v", err)
	}

	res, err := store.Reserve(1, 2)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	diff, chain := Digest{1}, Digest{2}
	addr := res.Address(1 << 20)

	records := []Record{
		{
			Type: RecordBlob, Segment: addr.Segment, PageOffset: addr.PageOffset,
			PageCount: addr.PageCount, ByteLength: addr.ByteLength,
			Generation: res.Generation, Key: diff, Ref: Digest{3},
		},
		{Type: RecordChain, Generation: res.Generation, Key: chain, Ref: diff},
	}

	if err := store.Append(res, records); err != nil {
		t.Fatalf("append: %v", err)
	}

	// A second Store over the same file is another node: it must see the
	// records after a Sync and nothing before one.
	other, err := OpenDevice(path, DeviceOptions{Direct: noDirect()})
	if err != nil {
		t.Fatalf("open other: %v", err)
	}
	defer other.Close() //nolint:errcheck // test cleanup

	peer, err := Open(other)
	if err != nil {
		t.Fatalf("open peer: %v", err)
	}

	if _, err := peer.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	blob, ok := peer.Resolve(chain)
	if !ok {
		t.Fatal("peer did not see the chain")
	}

	if blob.Address != addr {
		t.Fatalf("got %+v, want %+v", blob.Address, addr)
	}
}
