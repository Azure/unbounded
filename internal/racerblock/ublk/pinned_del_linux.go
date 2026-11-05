// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// Pinned DEL_DEV, for kernels without DEL_DEV_ASYNC (Linux 6.8).
//
// The kernel frees a device id only when the last reference to its char
// device goes away, so while the caller holds the char device open the id
// cannot be reused and a DEL_DEV by id can only reach the caller's own
// device. DEL_DEV removes the device, marks it deleted and drops its
// reference, then waits interruptibly for the id to be freed. That wait
// cannot end while the char device is held, so the command is interrupted
// with IORING_OP_ASYNC_CANCEL instead:
//
//   - The control command returns -EAGAIN when issued nonblocking, and
//     IOSQE_ASYNC sends it to an io-wq worker either way.
//   - Canceling pending io-wq work completes it with -ECANCELED before the
//     driver runs, so nothing was deleted and DEL_DEV is sent again.
//   - Canceling running work sets TIF_NOTIFY_SIGNAL on the worker, which
//     ends the interruptible wait; DEL_DEV then completes with -EINTR. The
//     only earlier step that can fail with -EINTR is mutex_lock_killable,
//     which fails only on a fatal signal, when this process is exiting.
//     So -EINTR means the device was removed and marked deleted.
//
// The caller must wait for this function before closing the char device,
// and must keep it open if the outcome is unknown.

// User data for the pinned DEL_DEV and its cancel. Other control commands
// use their opcode, which never has bit 62 set.
const (
	pinnedDelUserData    = 1<<62 | 1
	pinnedCancelUserData = 1<<62 | 2
)

// Retry limits for a pinned DEL_DEV: how many times DEL_DEV is resent after
// a cancel before dispatch, and how many cancels one DEL_DEV gets. They cap
// retries only. Each wait for a completion still lasts as long as the
// kernel takes, so a stalled kernel can stall the delete. No outcome
// depends on timing.
const (
	pinnedDelSubmits = 8
	pinnedDelCancels = 16
)

// errPinnedDelUnknown marks a pinned DEL_DEV whose completion was not seen.
// The command may still be running, so the char device must stay open.
var errPinnedDelUnknown = errors.New("ublk: DEL_DEV outcome unknown")

// pinnedDelRing is the io_uring access a pinned DEL_DEV needs. Tests
// replace it with a scripted fake.
type pinnedDelRing interface {
	// submitDel submits DEL_DEV with pinnedDelUserData and IOSQE_ASYNC.
	submitDel() error
	// submitCancel submits an ASYNC_CANCEL for pinnedDelUserData with
	// pinnedCancelUserData.
	submitCancel() error
	// poll returns a completion if one is ready, without blocking.
	poll() (cqe, bool, error)
	// wait blocks for the next completion.
	wait() (cqe, error)
	// pause waits before a cancel so the worker can reach the wait.
	pause(round int)
}

// pinnedDel deletes a device with DEL_DEV while its char device is held.
// It returns nil once the device is removed, the DEL_DEV error if the
// kernel rejected it, or an error wrapping errPinnedDelUnknown if a
// command may still be outstanding.
func pinnedDel(r pinnedDelRing) error {
	for round := range pinnedDelSubmits {
		res, err := pinnedDelOnce(r, round)
		if err != nil {
			return err
		}

		switch res {
		case 0, -int32(syscall.EINTR):
			// Removed. -EINTR is the interrupted wait for the id.
			return nil
		case -int32(syscall.ECANCELED):
			// Canceled before the driver ran; nothing was deleted.
			continue
		default:
			return resErr("DEL_DEV", res)
		}
	}

	return fmt.Errorf("ublk: DEL_DEV: canceled before dispatch %d times", pinnedDelSubmits)
}

// pinnedDelOnce sends one DEL_DEV and returns its result once both it and
// any cancel have completed, so the ring is left with nothing in flight.
func pinnedDelOnce(r pinnedDelRing, round int) (int32, error) {
	if err := r.submitDel(); err != nil {
		return 0, fmt.Errorf("%w: %w", errPinnedDelUnknown, err)
	}

	var (
		delRes  int32
		delDone bool
	)

	take := func(c cqe) error {
		switch c.UserData {
		case pinnedDelUserData:
			if delDone {
				return fmt.Errorf("%w: duplicate DEL_DEV completion", errPinnedDelUnknown)
			}

			delRes, delDone = c.Res, true

			return nil
		default:
			return fmt.Errorf("%w: unexpected completion %#x", errPinnedDelUnknown, c.UserData)
		}
	}

	for cancels := 0; !delDone; cancels++ {
		if cancels == pinnedDelCancels {
			return 0, fmt.Errorf("%w: DEL_DEV not cancellable after %d tries", errPinnedDelUnknown, cancels)
		}

		r.pause(round + cancels)

		// DEL_DEV may already have failed without waiting.
		for !delDone {
			c, ok, err := r.poll()
			if err != nil {
				return 0, fmt.Errorf("%w: %w", errPinnedDelUnknown, err)
			}

			if !ok {
				break
			}

			if err := take(c); err != nil {
				return 0, err
			}
		}

		if delDone {
			break
		}

		if err := r.submitCancel(); err != nil {
			return 0, fmt.Errorf("%w: %w", errPinnedDelUnknown, err)
		}

		// Reap until the cancel completes; DEL_DEV may complete first.
		var cancelRes int32

		for cancelDone := false; !cancelDone; {
			c, err := r.wait()
			if err != nil {
				return 0, fmt.Errorf("%w: %w", errPinnedDelUnknown, err)
			}

			if c.UserData == pinnedCancelUserData {
				cancelRes, cancelDone = c.Res, true
				continue
			}

			if err := take(c); err != nil {
				return 0, err
			}
		}

		switch cancelRes {
		case 0, -int32(syscall.EALREADY):
			// Found pending (completes -ECANCELED) or running (signaled):
			// DEL_DEV now completes on its own.
			for !delDone {
				c, err := r.wait()
				if err != nil {
					return 0, fmt.Errorf("%w: %w", errPinnedDelUnknown, err)
				}

				if err := take(c); err != nil {
					return 0, err
				}
			}
		case -int32(syscall.ENOENT):
			// Not found: either completed, which the next poll sees, or
			// not yet visible to the cancel. Try again.
		default:
			return 0, fmt.Errorf("%w: ASYNC_CANCEL: %w", errPinnedDelUnknown, syscall.Errno(-cancelRes))
		}
	}

	return delRes, nil
}

// ctrlPinnedDelRing runs a pinned DEL_DEV on a control ring. The caller
// holds c.mu.
type ctrlPinnedDelRing struct {
	c  *ctrl
	id uint32
}

func (p ctrlPinnedDelRing) submitDel() error {
	s := p.c.ring.getSQE()
	if s == nil {
		return errRingFull
	}

	s.Opcode = ioringOpURingCmd
	s.Flags = iosqeAsync
	s.Fd = int32(p.c.fd)
	s.Off = uint64(uCmdDelDev)
	s.UserData = pinnedDelUserData

	cmd := (*ctrlCmd)(unsafe.Pointer(&cmdArea(s)[0]))
	cmd.DevID = p.id
	cmd.QueueID = 0xffff

	return p.c.ring.submitAndWait(0)
}

func (p ctrlPinnedDelRing) submitCancel() error {
	s := p.c.ring.getSQE()
	if s == nil {
		return errRingFull
	}

	s.Opcode = ioringOpAsyncCancel
	s.Fd = -1
	s.Addr = pinnedDelUserData
	s.UserData = pinnedCancelUserData

	return p.c.ring.submitAndWait(0)
}

func (p ctrlPinnedDelRing) poll() (cqe, bool, error) {
	c, ok := p.c.ring.peekCQE()
	if ok {
		p.c.ring.advanceCQ()
	}

	return c, ok, nil
}

func (p ctrlPinnedDelRing) wait() (cqe, error) { return p.c.ring.waitCQE() }

func (p ctrlPinnedDelRing) pause(round int) {
	if testHooks.pinnedDelPause != nil {
		testHooks.pinnedDelPause(round)
		return
	}

	pinnedDelSleep(round)
}

// pinnedDelSleep is the production pause before a pinned DEL_DEV cancel.
func pinnedDelSleep(round int) {
	time.Sleep(min(time.Millisecond<<min(round, 8), 200*time.Millisecond))
}

// delDevPinned deletes device id with DEL_DEV while the caller holds its
// char device open. See pinnedDel. If it returns an error wrapping
// errPinnedDelUnknown, the channel is poisoned and the char device must
// not be closed.
func (c *ctrl) delDevPinned(id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ring == nil {
		return fmt.Errorf("ublk: control channel closed")
	}

	if c.poisoned {
		return fmt.Errorf("%w: control channel unusable after a failed command", errPinnedDelUnknown)
	}

	err := pinnedDel(ctrlPinnedDelRing{c: c, id: uint32(id)})
	if errors.Is(err, errPinnedDelUnknown) {
		c.poisoned = true
	}

	return err
}
