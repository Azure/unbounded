// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestURingLayout(t *testing.T) {
	if s := unsafe.Sizeof(sqe{}); s != 64 {
		t.Errorf("sqe size %d", s)
	}

	if o := unsafe.Offsetof(sqe{}.UserData); o != 32 {
		t.Errorf("user_data offset %d", o)
	}

	if o := unsafe.Offsetof(sqe{}.Addr3); o != sqeCmdOffset {
		t.Errorf("cmd offset %d", o)
	}

	if s := unsafe.Sizeof(cqe{}); s != cqeSize {
		t.Errorf("cqe size %d", s)
	}

	if s := unsafe.Sizeof(uringParams{}); s != 120 {
		t.Errorf("params size %d", s)
	}

	if o := unsafe.Offsetof(uringParams{}.CQOff); o != 80 {
		t.Errorf("cq_off offset %d", o)
	}
}

func newTestRing(t *testing.T, n uint32) *uring {
	t.Helper()

	r, err := newURing(n)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOSYS) {
		t.Skipf("io_uring unavailable: %v", err)
	}

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = r.close() })

	return r
}

func TestURingNop(t *testing.T) {
	r := newTestRing(t, 4)

	// Fill the whole SQ, then verify getSQE reports full.
	n := 0

	for {
		s := r.getSQE()
		if s == nil {
			break
		}

		s.Opcode = ioringOpNop
		s.UserData = uint64(100 + n)
		n++
	}

	if n != int(r.sqEntries) {
		t.Fatalf("filled %d, entries %d", n, r.sqEntries)
	}

	// Several rounds to exercise ring wraparound.
	for round := range 3 {
		if round > 0 {
			for i := range n {
				s := r.getSQE()
				if s == nil {
					t.Fatal("unexpected full ring")
				}

				s.Opcode = ioringOpNop
				s.UserData = uint64(100 + i)
			}
		}

		if err := r.submitAndWait(uint32(n)); err != nil {
			t.Fatal(err)
		}

		seen := map[uint64]bool{}

		for range n {
			c, err := r.waitCQE()
			if err != nil {
				t.Fatal(err)
			}

			if c.Res != 0 {
				t.Fatalf("nop res %d", c.Res)
			}

			seen[c.UserData] = true
		}

		if len(seen) != n {
			t.Fatalf("round %d: saw %v", round, seen)
		}

		if _, ok := r.peekCQE(); ok {
			t.Fatal("unexpected extra CQE")
		}
	}
}

func TestURingEventfdPoll(t *testing.T) {
	r := newTestRing(t, 2)

	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(efd)

	for range 3 {
		s := r.getSQE()
		s.Opcode = ioringOpPollAdd
		s.Fd = int32(efd)
		s.OpFlags = unix.POLLIN
		s.UserData = pollUserData

		if err := r.submitAndWait(0); err != nil {
			t.Fatal(err)
		}

		if _, ok := r.peekCQE(); ok {
			t.Fatal("poll completed before eventfd write")
		}

		one := [8]byte{1}
		if _, err := unix.Write(efd, one[:]); err != nil {
			t.Fatal(err)
		}

		c, err := r.waitCQE()
		if err != nil {
			t.Fatal(err)
		}

		if c.UserData != pollUserData || c.Res&unix.POLLIN == 0 {
			t.Fatalf("poll cqe %+v", c)
		}

		var b [8]byte
		if _, err := unix.Read(efd, b[:]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestURingCloseIdempotent(t *testing.T) {
	r := newTestRing(t, 1)
	if err := r.close(); err != nil {
		t.Fatal(err)
	}

	if err := r.close(); err != nil {
		t.Fatal(err)
	}
}
