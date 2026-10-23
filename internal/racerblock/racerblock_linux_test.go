// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package racerblock

// The end-to-end test needs root and the ublk_drv module. It is skipped
// otherwise. Run it by hand with:
//
//	sudo modprobe ublk_drv
//	go test -c -o /tmp/opencode/racerblock.test ./internal/racerblock
//	sudo /tmp/opencode/racerblock.test -test.v -test.run EndToEnd

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/racerblock/backend"
	"github.com/Azure/unbounded/internal/racerblock/ublk"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func requireUblk(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skipf("/dev/ublk-control unavailable: %v", err)
	}
}

// alignedBuf returns a 4096-aligned buffer of n bytes for O_DIRECT.
func alignedBuf(n int) []byte {
	raw := make([]byte, n+4096)
	off := (4096 - int(uintptr(unsafe.Pointer(&raw[0]))&4095)) & 4095

	return raw[off : off+n]
}

// wantBytes is the device content for [off, off+n): the object pattern,
// then zeros past objectSize.
func wantBytes(off int64, n int, objectSize int64) []byte {
	want := make([]byte, n)
	if off < objectSize {
		fillPattern(want[:min(int64(n), objectSize-off)], off)
	}

	return want
}

func TestEndToEndDevice(t *testing.T) {
	requireUblk(t)

	// Not a multiple of 512, so the device has a zero-filled tail.
	const objectSize = 40<<20 + 123

	client := racersdktest.NewClient(t, patternOrigin(objectSize))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	paths := make(chan string, 1)
	done := make(chan error, 1)

	go func() {
		done <- Run(ctx, Config{
			Source: backend.FromClient(client, racersdk.Request{Key: testKey}),
			Backend: backend.Config{
				ChunkSize:    256 << 10,
				CacheBytes:   4 << 20,
				ReadaheadMax: 1 << 20,
			},
			Device: ublk.Config{ID: -1, Queues: 2, Depth: 16},
			Ready:  func(path string) { paths <- path },
		})
	}()

	var path string

	select {
	case path = <-paths:
	case err := <-done:
		t.Fatalf("Run returned before ready: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("device not ready after 30s")
	}

	deviceSize, err := DeviceSize(objectSize)
	if err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECT|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}

	var size uint64
	if err := ioctlGetSize64(fd, &size); err != nil {
		t.Fatalf("BLKGETSIZE64: %v", err)
	}

	if int64(size) != deviceSize {
		t.Fatalf("device size = %d, want %d", size, deviceSize)
	}

	check := func(off int64, n int) {
		t.Helper()

		buf := alignedBuf(n)

		got, err := unix.Pread(fd, buf, off)
		if err != nil {
			t.Fatalf("pread off=%d len=%d: %v", off, n, err)
		}

		if want := int(min(int64(n), deviceSize-off)); got != want {
			t.Fatalf("pread off=%d len=%d returned %d bytes, want %d", off, n, got, want)
		}

		if !bytes.Equal(buf[:got], wantBytes(off, got, objectSize)) {
			t.Fatalf("data mismatch off=%d len=%d", off, n)
		}
	}

	// Sequential pass over the whole device, including the zero tail.
	for off := int64(0); off < deviceSize; off += 1 << 20 {
		check(off, 1<<20)
	}

	// Random small reads, most of them evicted from the 4 MiB cache.
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		check(rng.Int64N(deviceSize/4096)*4096, 4096)
	}

	check(deviceSize-512, 512)

	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// udev removes the node asynchronously after the kernel deletes the device.
	deadline := time.Now().Add(10 * time.Second)

	for {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s still present after Run returned: %v", path, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func ioctlGetSize64(fd int, size *uint64) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(size)))
	if errno != 0 {
		return errno
	}

	return nil
}
