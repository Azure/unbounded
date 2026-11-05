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

const testCookie = 0x5eed

// pinFixture returns a never-started Device with id 7 and testCookie,
// whose control channel has no fds, so no control command can reach the
// kernel. The char device is faked with the read end of a pipe: once that
// is closed, writing to the returned write end fails with EPIPE, which
// shows the fd was closed without trusting fd numbers.
func pinFixture(t *testing.T, cookie uint64, cookieErr error) (d *Device, w int, events *[]string) {
	t.Helper()

	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = unix.Close(p[1]) })

	var log []string

	testHooks.openChar = func(path string) (int, error) {
		log = append(log, "open "+path)

		return p[0], nil
	}
	testHooks.devCookie = func(id int) (uint64, error) {
		log = append(log, "GET_DEV_INFO")
		return cookie, cookieErr
	}
	testHooks.ctrlSent = func(op, devID uint32) {
		switch op {
		case uCmdSetParams:
			log = append(log, "SET_PARAMS")
		case uCmdStartDev:
			log = append(log, "START_DEV")
		case uCmdStopDev:
			log = append(log, "STOP_DEV")
		case uCmdDelDev, uCmdDelDevAsync:
			log = append(log, "DEL_DEV")
		default:
			log = append(log, "other")
		}
	}

	t.Cleanup(func() {
		testHooks.openChar, testHooks.devCookie, testHooks.ctrlSent = nil, nil, nil
	})

	d = &Device{
		cfg:    Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Queues: 1, Depth: 1},
		id:     7,
		cookie: testCookie,
		ctrl:   &ctrl{fd: -1},
		charFd: -1,
		done:   make(chan struct{}),
	}
	d.init()
	d.hctx, d.hcancel = context.WithCancel(context.Background())

	return d, p[1], &log
}

func fdClosed(t *testing.T, w int) bool {
	t.Helper()

	_, err := unix.Write(w, []byte{0})

	return errors.Is(err, unix.EPIPE)
}

// TestStartVerifiesCookieBeforeMutating checks that start pins the char
// device and matches the cookie before sending SET_PARAMS, and that the
// following teardown stops and deletes the verified device.
func TestStartVerifiesCookieBeforeMutating(t *testing.T) {
	d, w, log := pinFixture(t, testCookie, nil)

	err := d.abortStart(d.runStartup(d.start))
	if err == nil {
		t.Fatal("start succeeded with a closed control channel")
	}

	if errors.Is(err, errUnpinnedLeft) {
		t.Fatalf("verified device reported as left in place: %v", err)
	}

	want := []string{"open /dev/ublkc7", "GET_DEV_INFO", "SET_PARAMS", "STOP_DEV", "DEL_DEV"}
	if len(*log) != len(want) {
		t.Fatalf("events = %v, want %v", *log, want)
	}

	for i := range want {
		if (*log)[i] != want[i] {
			t.Fatalf("events = %v, want %v", *log, want)
		}
	}

	if !fdClosed(t, w) {
		t.Fatal("char fd not closed by teardown")
	}
}

// TestStartRejectsUnverifiedDevice checks that a cookie mismatch, which
// means another device reused the id, or a failed query closes the fd and
// sends no command by id at all, while the error keeps the cause and
// reports the device for manual cleanup.
func TestStartRejectsUnverifiedDevice(t *testing.T) {
	queryErr := errors.New("GET_DEV_INFO failed")

	for _, tc := range []struct {
		name   string
		cookie uint64
		err    error
	}{
		{"mismatch", testCookie + 1, nil},
		{"zero", 0, nil},
		{"query error", 0, queryErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, w, log := pinFixture(t, tc.cookie, tc.err)

			startErr := d.runStartup(d.start)
			if startErr == nil {
				t.Fatal("start accepted an unverified device")
			}

			if tc.err != nil && !errors.Is(startErr, tc.err) {
				t.Fatalf("start = %v, want query error kept", startErr)
			}

			if d.charFd != -1 {
				t.Fatalf("charFd = %d, want unset", d.charFd)
			}

			if !fdClosed(t, w) {
				t.Fatal("unverified char fd left open")
			}

			err := d.abortStart(startErr)
			if !errors.Is(err, startErr) || !errors.Is(err, errUnpinnedLeft) {
				t.Fatalf("abortStart = %v, want start error and manual cleanup", err)
			}

			want := []string{"open /dev/ublkc7", "GET_DEV_INFO"}
			if len(*log) != len(want) || (*log)[0] != want[0] || (*log)[1] != want[1] {
				t.Fatalf("events = %v, want only %v", *log, want)
			}
		})
	}
}

// TestNewCookieNonzero checks that cookies are nonzero and differ.
func TestNewCookieNonzero(t *testing.T) {
	a, err := newCookie()
	if err != nil {
		t.Fatal(err)
	}

	b, err := newCookie()
	if err != nil {
		t.Fatal(err)
	}

	if a == 0 || b == 0 || a == b {
		t.Fatalf("cookies %#x, %#x: want distinct nonzero", a, b)
	}
}
