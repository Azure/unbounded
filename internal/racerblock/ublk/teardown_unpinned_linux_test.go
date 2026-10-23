// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestStartFailureWithoutCharFdSendsNothingByID drives the real startup
// failure wiring (runStartup, abortStart, teardown) for a device whose
// char device was never opened, as when SET_PARAMS or the open of
// /dev/ublkc<ID> fails. Nothing pins the id then, so teardown must not
// send STOP_DEV or DEL_DEV by id, which could hit another device that
// reused it. The error must keep the startup cause and name the device
// left for manual cleanup. The control channel has no fds, so no command
// could reach the kernel even if one were sent.
func TestStartFailureWithoutCharFdSendsNothingByID(t *testing.T) {
	var sent []uint32

	testHooks.ctrlSent = func(op, devID uint32) { sent = append(sent, op) }

	t.Cleanup(func() { testHooks.ctrlSent = nil })

	d := &Device{
		cfg:    Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		id:     7,
		ctrl:   &ctrl{fd: -1},
		charFd: -1,
		done:   make(chan struct{}),
	}
	d.init()
	d.hctx, d.hcancel = context.WithCancel(context.Background())

	startErr := errors.New("open /dev/ublkc7: permission denied")

	err := d.runStartup(func() error { return startErr })
	if !errors.Is(err, startErr) {
		t.Fatalf("runStartup = %v, want start error", err)
	}

	err = d.abortStart(err)

	if len(sent) != 0 {
		t.Fatalf("teardown sent control commands %v by id without a char device pin", sent)
	}

	if !errors.Is(err, startErr) {
		t.Fatalf("abortStart = %v, want original start error kept", err)
	}

	if !errors.Is(err, errUnpinnedLeft) || !strings.Contains(err.Error(), "device 7") {
		t.Fatalf("abortStart = %v, want device 7 reported for manual cleanup", err)
	}

	if !errors.Is(d.Close(), errUnpinnedLeft) {
		t.Fatal("Close after failed start does not report the device left in place")
	}
}

// TestTeardownWithCharFdStillDeletes is the contrast: with the char device
// held, teardown does send the deletion by id and does not report the
// device as left in place.
func TestTeardownWithCharFdStillDeletes(t *testing.T) {
	var sent []uint32

	testHooks.ctrlSent = func(op, devID uint32) { sent = append(sent, op) }

	t.Cleanup(func() { testHooks.ctrlSent = nil })

	d, _ := syntheticTeardownDevice(t, &ctrl{fd: -1})

	err := d.Close()
	if errors.Is(err, errUnpinnedLeft) {
		t.Fatalf("Close = %v, pinned device reported as left in place", err)
	}

	found := false

	for _, op := range sent {
		if op == uCmdDelDevAsync {
			found = true
		}
	}

	if !found {
		t.Fatalf("sent %v, want DEL_DEV_ASYNC", sent)
	}

	if d.charFd != -1 {
		t.Fatalf("charFd = %d, want released", d.charFd)
	}
}
