// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// syncBuffer is a bytes.Buffer that is safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// withoutDelDevAsync makes DEL_DEV_ASYNC fail as on kernels that lack it
// (before Linux 6.9) by swapping in a control command number the ublk
// UAPI does not define. This injects the fault on the running kernel; it
// does not exercise an older kernel. No test in this package runs in
// parallel, so overriding the package variable is safe.
func withoutDelDevAsync(t *testing.T) {
	t.Helper()

	orig := uCmdDelDevAsync
	uCmdDelDevAsync = ior(0x7f, uint32(unsafe.Sizeof(ctrlCmd{})))

	t.Cleanup(func() { uCmdDelDevAsync = orig })
}

// pinnedDelRecord is what teardown saw when its pinned DEL_DEV returned.
type pinnedDelRecord struct {
	calls int
	err   error
	// rdev is the char device number seen while the device was running.
	rdev   uint64
	fdOpen bool
	sameID bool
}

// recordPinnedDel checks, at the moment the pinned DEL_DEV returns, that
// the char device fd is still open and still refers to the same char
// device number recorded by pin, so the id stayed pinned for the whole
// DEL_DEV.
func recordPinnedDel(t *testing.T) *pinnedDelRecord {
	t.Helper()

	rec := &pinnedDelRecord{}
	testHooks.pinnedDelDone = func(d *Device, err error) {
		rec.calls++
		rec.err = err

		var st unix.Stat_t

		rec.fdOpen = d.charFd >= 0 && unix.Fstat(d.charFd, &st) == nil
		// The node is gone after removal, so compare the fd's full
		// device number (major and minor) with the one pin recorded.
		rec.sameID = rec.fdOpen && st.Mode&unix.S_IFMT == unix.S_IFCHR && rec.rdev != 0 && st.Rdev == rec.rdev
	}

	t.Cleanup(func() { testHooks.pinnedDelDone = nil })

	return rec
}

// pin records the running device's char device number, whose minor must
// be the device id.
func (r *pinnedDelRecord) pin(t *testing.T, d *Device) {
	t.Helper()

	var st unix.Stat_t
	if err := unix.Fstat(d.charFd, &st); err != nil {
		t.Fatalf("fstat char device: %v", err)
	}

	if st.Mode&unix.S_IFMT != unix.S_IFCHR || int(unix.Minor(st.Rdev)) != d.ID() {
		t.Fatalf("char fd is not /dev/ublkc%d: mode %#o rdev %#x", d.ID(), st.Mode, st.Rdev)
	}

	r.rdev = st.Rdev
}

func (r *pinnedDelRecord) check(t *testing.T) {
	t.Helper()

	if r.calls != 1 {
		t.Fatalf("pinned DEL_DEV ran %d times, want 1", r.calls)
	}

	if r.err != nil {
		t.Errorf("pinned DEL_DEV: %v", r.err)
	}

	if !r.fdOpen || !r.sameID {
		t.Errorf("char device fd not held for DEL_DEV: open=%v sameID=%v", r.fdOpen, r.sameID)
	}
}

// startFallbackDevice starts an owned test device whose debug log is
// captured so the test can confirm the DEL_DEV fallback ran.
func startFallbackDevice(t *testing.T) (*Device, *syncBuffer) {
	t.Helper()

	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := startTestDevice(t, context.Background(), newPatternHandler(1<<20), Config{ID: -1, Size: 1 << 20, Queues: 2, Logger: logger})

	return d, logs
}

func checkFallback(t *testing.T, d *Device, logs *syncBuffer) {
	t.Helper()

	if !strings.Contains(logs.String(), "falling back to DEL_DEV") {
		t.Errorf("DEL_DEV fallback not logged; logs:\n%s", logs)
	}

	for _, p := range []string{"/dev/ublkc", "/dev/ublkb"} {
		if _, err := os.Stat(p + strconv.Itoa(d.ID())); !os.IsNotExist(err) {
			t.Errorf("%s%d still present: %v", p, d.ID(), err)
		}
	}
}

func TestIntegrationDeleteFallbackClose(t *testing.T) {
	requireUblk(t)
	withoutDelDevAsync(t)

	rec := recordPinnedDel(t)
	d, logs := startFallbackDevice(t)
	rec.pin(t, d)

	select {
	case <-d.Done():
		t.Fatal("Done closed before Close")
	default:
	}

	closed := make(chan error, 1)

	go func() { closed <- d.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return")
	}

	select {
	case <-d.Done():
	default:
		t.Fatal("Done not closed after Close")
	}

	if err := d.Err(); err != nil {
		t.Fatalf("Err = %v, want nil after clean Close", err)
	}

	if err := d.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	checkFallback(t, d, logs)
	rec.check(t)
}

func TestIntegrationDeleteFallbackExternalStop(t *testing.T) {
	requireUblk(t)
	withoutDelDevAsync(t)

	rec := recordPinnedDel(t)
	d, logs := startFallbackDevice(t)
	rec.pin(t, d)

	c, err := openCtrl()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = c.close() }()

	// STOP_DEV leaves the device allocated; teardown must delete it via
	// the DEL_DEV fallback.
	if err := c.stopDev(d.ID()); err != nil {
		t.Fatalf("STOP_DEV: %v", err)
	}

	waitStopped(t, d)
	checkFallback(t, d, logs)
	rec.check(t)
}

// TestIntegrationDeleteFallbackCancelFirst cancels each DEL_DEV as soon as
// it is submitted, so the kernel may cancel it before dispatch. Either
// way teardown must finish and remove the device with the fd held.
func TestIntegrationDeleteFallbackCancelFirst(t *testing.T) {
	requireUblk(t)
	withoutDelDevAsync(t)

	testHooks.pinnedDelPause = func(int) {}

	t.Cleanup(func() { testHooks.pinnedDelPause = nil })

	rec := recordPinnedDel(t)
	d, logs := startFallbackDevice(t)
	rec.pin(t, d)

	if err := closeWithin(d, 30*time.Second); err != nil {
		t.Fatal(err)
	}

	checkFallback(t, d, logs)
	rec.check(t)
}

// TestIntegrationDeleteFallbackExternalDelete deletes the device with a
// synchronous DEL_DEV on a separate control channel, which blocks until the
// device releases its char device. Teardown's own pinned DEL_DEV finds it already
// marked deleted, is interrupted, and only then closes the fd, which lets
// the external DEL_DEV return.
func TestIntegrationDeleteFallbackExternalDelete(t *testing.T) {
	requireUblk(t)
	withoutDelDevAsync(t)

	rec := recordPinnedDel(t)
	d, logs := startFallbackDevice(t)
	rec.pin(t, d)

	delErr := directDelete(t, d.ID())

	waitStopped(t, d)

	select {
	case err := <-delErr:
		if err != nil {
			t.Fatalf("external DEL_DEV: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("external DEL_DEV did not return")
	}

	checkFallback(t, d, logs)
	rec.check(t)
}

func closeWithin(d *Device, limit time.Duration) error {
	closed := make(chan error, 1)

	go func() { closed <- d.Close() }()

	select {
	case err := <-closed:
		return err
	case <-time.After(limit):
		return errors.New("Close did not return")
	}
}
