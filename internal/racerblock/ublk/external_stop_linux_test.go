// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// waitStopped waits for d to stop on its own and checks that it reports an
// external stop, that Close is clean and idempotent and that the device
// nodes are gone.
func waitStopped(t *testing.T, d *Device) {
	t.Helper()

	select {
	case <-d.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done not closed after external stop")
	}

	if err := d.Err(); !errors.Is(err, errStoppedExternally) || !errors.Is(err, syscall.ENODEV) {
		t.Fatalf("Err = %v, want external stop", err)
	}

	for range 2 {
		if err := d.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	for _, p := range []string{"/dev/ublkc", "/dev/ublkb"} {
		if _, err := os.Stat(p + strconv.Itoa(d.ID())); !os.IsNotExist(err) {
			t.Errorf("%s%d still present: %v", p, d.ID(), err)
		}
	}
}

// directDelete issues a bare DEL_DEV for id on a separate control channel,
// without the STOP_DEV that Delete sends first, so the device's own teardown
// cannot remove the ID before the DEL_DEV arrives. The goroutine owns the
// channel and closes it only after DEL_DEV returns, so a failing test never
// closes a ring still in use. The result is sent on the returned channel.
func directDelete(t *testing.T, id int) <-chan error {
	t.Helper()

	c, err := openCtrl()
	if err != nil {
		t.Fatal(err)
	}

	res := make(chan error, 1)

	go func() {
		err := c.delDev(id)
		if cerr := c.close(); err == nil {
			err = cerr
		}

		res <- err
	}()

	return res
}

func TestIntegrationExternalDelete(t *testing.T) {
	requireUblk(t)

	d := startTestDevice(t, context.Background(), newPatternHandler(1<<20), Config{ID: -1, Size: 1 << 20, Queues: 2})

	// DEL_DEV on a separate control channel waits for the device to release
	// the char device, so it only returns once the device has noticed and
	// torn itself down.
	delErr := directDelete(t, d.ID())

	waitStopped(t, d)

	select {
	case err := <-delErr:
		if err != nil {
			t.Fatalf("DEL_DEV: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("external DEL_DEV did not return")
	}
}

func TestIntegrationExternalStop(t *testing.T) {
	requireUblk(t)

	d := startTestDevice(t, context.Background(), newPatternHandler(1<<20), Config{ID: -1, Size: 1 << 20, Queues: 2})

	c, err := openCtrl()
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = c.close() }()

	// STOP_DEV alone leaves the device allocated; teardown must delete it.
	if err := c.stopDev(d.ID()); err != nil {
		t.Fatalf("STOP_DEV: %v", err)
	}

	waitStopped(t, d)

	if err := Delete(d.ID()); err == nil {
		t.Fatal("device still exists after teardown")
	}
}
