// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"bytes"
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// newTestCtrl returns a ctrl whose control fd is /dev/null. URING_CMD on it
// completes with an error CQE, which is enough to run the payload path
// without root or a ublk device.
func newTestCtrl(t *testing.T) *ctrl {
	t.Helper()

	fd, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}

	c, err := newCtrl(fd)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM) {
			t.Skipf("io_uring unavailable: %v", err)
		}

		t.Fatalf("newCtrl: %v", err)
	}

	return c
}

// lastCmd returns the control command in the most recently used SQE slot.
func lastCmd(c *ctrl) *ctrlCmd {
	idx := (c.ring.sqLocalTail - 1) & c.ring.sqMask
	s := (*sqe)(unsafe.Pointer(&c.ring.sqes[int(idx)*sqe128Size]))

	return (*ctrlCmd)(unsafe.Pointer(&cmdArea(s)[0]))
}

// growStack forces stack growth so that any stack-held payload would move.
func growStack(n int) byte {
	var pad [1024]byte

	pad[n%len(pad)] = byte(n)
	if n == 0 {
		return pad[0]
	}

	return growStack(n-1) + pad[n%len(pad)]
}

func TestCtrlPayloadUsesStablePage(t *testing.T) {
	c := newTestCtrl(t)
	defer func() {
		if err := c.close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	page := uintptr(unsafe.Pointer(&c.payload[0]))
	info := ctrlDevInfo{NrHwQueues: 3, QueueDepth: 64, DevID: 7}
	in := structBytes(&info)
	out := make([]byte, len(in))

	for i := range 3 {
		growStack(64)

		if _, err := c.doPayload(uCmdAddDev, 7, 0, in, out); err != nil {
			t.Fatalf("doPayload %d: %v", i, err)
		}

		cmd := lastCmd(c)
		if cmd.Addr != uint64(page) {
			t.Fatalf("addr = %#x, want payload page %#x", cmd.Addr, page)
		}

		if int(cmd.Len) != len(in) {
			t.Fatalf("len = %d, want %d", cmd.Len, len(in))
		}

		if got := uintptr(unsafe.Pointer(&c.payload[0])); got != page {
			t.Fatalf("payload moved: %#x -> %#x", page, got)
		}
	}

	// The kernel rejected the command, so out holds what was copied in.
	if !bytes.Equal(out, in) {
		t.Fatalf("out = %x, want %x", out, in)
	}
}

func TestCtrlPayloadNoBufferHasNoAddr(t *testing.T) {
	c := newTestCtrl(t)
	defer c.close()

	if _, err := c.do(uCmdStopDev, 1, 0, nil, 0); err != nil {
		t.Fatalf("do: %v", err)
	}

	if cmd := lastCmd(c); cmd.Addr != 0 || cmd.Len != 0 {
		t.Fatalf("addr/len = %#x/%d, want 0/0", cmd.Addr, cmd.Len)
	}
}

func TestCtrlPayloadRejectsRawPointer(t *testing.T) {
	c := newTestCtrl(t)
	defer c.close()

	var x uint64

	if _, err := c.do(uCmdSetParams, 1, 0, unsafe.Pointer(&x), 8); err == nil {
		t.Fatal("do accepted a raw pointer payload")
	}

	if _, err := c.doPayload(uCmdSetParams, 1, 0, make([]byte, ctrlPayloadSize+1), nil); err == nil {
		t.Fatal("doPayload accepted an oversize payload")
	}

	if c.poisoned {
		t.Fatal("argument errors must not poison the channel")
	}
}

func TestCtrlPayloadRetainedAfterWaitError(t *testing.T) {
	c := newTestCtrl(t)

	payload := c.payload
	info := ctrlDevInfo{DevID: 9}

	// Break the ring fd so io_uring_enter fails after the SQE is published.
	ringFd := c.ring.fd
	if err := unix.Close(ringFd); err != nil {
		t.Fatalf("close ring fd: %v", err)
	}

	c.ring.fd = -1

	_, err := c.doPayload(uCmdAddDev, 9, 0, structBytes(&info), nil)
	if err == nil {
		t.Fatal("doPayload succeeded on a broken ring")
	}

	if !c.poisoned {
		t.Fatal("wait error did not poison the channel")
	}

	if _, err := c.do(uCmdStopDev, 9, 0, nil, 0); err == nil {
		t.Fatal("poisoned channel accepted a command")
	}

	if err := c.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if c.payload != nil {
		t.Fatal("close kept a reference to the payload")
	}

	// The page must still be mapped and writable. This faults if close
	// unmapped it. Munmap of a mapped range also confirms it was retained.
	payload[0] ^= 0xff

	if err := unix.Munmap(payload); err != nil {
		t.Fatalf("munmap: %v", err)
	}
}

func TestCtrlCloseReleasesPayload(t *testing.T) {
	c := newTestCtrl(t)

	if err := c.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if c.payload != nil {
		t.Fatal("close kept the payload")
	}

	if err := c.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
