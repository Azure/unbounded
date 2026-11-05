// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"golang.org/x/sys/unix"
)

// syntheticTeardownDevice returns a never-started Device whose char fd is
// a harmless /dev/null fd and whose control channel has no real fds, so
// teardown cannot reach ublk or io_uring. Every control command fails in
// doPayload before touching the ring: poisoned channels are refused, and
// a channel with a nil ring is reported closed.
func syntheticTeardownDevice(t *testing.T, c *ctrl) (*Device, int) {
	t.Helper()

	fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}

	d := &Device{
		cfg:    Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		id:     7,
		ctrl:   c,
		charFd: fd,
		done:   make(chan struct{}),
	}
	d.init()
	d.hctx, d.hcancel = context.WithCancel(context.Background())

	return d, fd
}

// TestTeardownPoisonedCtrlKeepsCharFd checks that when the control channel
// already missed a completion, teardown keeps the char device open, since
// an earlier DEL_DEV may still be running and must not reach a reused id,
// and reports a failure.
func TestTeardownPoisonedCtrlKeepsCharFd(t *testing.T) {
	c := &ctrl{fd: -1, ring: &uring{fd: -1}, poisoned: true}
	d, fd := syntheticTeardownDevice(t, c)

	t.Cleanup(func() { _ = unix.Close(fd) })

	err := d.Close()
	if err == nil {
		t.Fatal("Close succeeded with a poisoned control channel")
	}

	if d.charFd != fd {
		t.Fatalf("charFd = %d, want %d kept", d.charFd, fd)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("char fd closed by teardown: %v", err)
	}

	if !c.poisoned {
		t.Fatal("control channel no longer poisoned")
	}
}

// TestTeardownClosedCtrlReleasesCharFd is the negative case: a control
// channel that failed cleanly, with nothing in flight, lets teardown
// release the char device and still report the failure.
func TestTeardownClosedCtrlReleasesCharFd(t *testing.T) {
	c := &ctrl{fd: -1}
	d, fd := syntheticTeardownDevice(t, c)

	closed := false

	t.Cleanup(func() {
		if !closed {
			_ = unix.Close(fd)
		}
	})

	err := d.Close()
	if err == nil || errors.Is(err, errPinnedDelUnknown) {
		t.Fatalf("Close = %v, want a known failure", err)
	}

	// Checking the fd number itself could see an unrelated fd that
	// reused it, so check that teardown dropped it.
	if d.charFd != -1 {
		t.Fatalf("charFd = %d, want released", d.charFd)
	}

	closed = true
}
