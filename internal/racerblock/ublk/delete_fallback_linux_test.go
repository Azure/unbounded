// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
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

	d, logs := startFallbackDevice(t)

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
}

func TestIntegrationDeleteFallbackExternalStop(t *testing.T) {
	requireUblk(t)
	withoutDelDevAsync(t)

	d, logs := startFallbackDevice(t)

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
}
