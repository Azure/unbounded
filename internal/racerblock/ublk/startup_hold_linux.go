// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"fmt"
	"sync"
	"syscall"
	"time"
)

// ublkSDevLive is UBLK_S_DEV_LIVE.
const ublkSDevLive = 1

var (
	// brokenHoldTimeout bounds how long a queue whose ring can no longer
	// serve waits for startup to resolve, in case a kernel invariant
	// below does not hold.
	brokenHoldTimeout = 30 * time.Second
	// livePollInterval is how often a broken queue polls the device state.
	livePollInterval = 5 * time.Millisecond
)

// holdUntilSafe keeps a failed queue's ring open until closing it cannot
// race START_DEV.
//
// Closing the ring cancels the queue's commands. Before START_DEV has
// published the disk, Linux 7.0's cancel path returns early without
// marking the queue canceling (ublk_start_cancel), so I/O that START_DEV
// later queues, such as the async partition scan, can reach a completed
// command. Once the disk exists, cancel marks the queue canceling and
// aborts its I/O, so the ring can be closed. Linux 6.8 scans partitions
// synchronously inside START_DEV, so a held queue must keep answering
// I/O or START_DEV never returns.
//
// The queue therefore answers every request with EIO until startup is
// resolved. If the ring itself fails, it cannot answer, and it waits
// until startup is resolved or GET_DEV_INFO reports the device LIVE.
// Both 6.8 and 7.0 set the disk before LIVE, under the device mutex
// that GET_DEV_INFO does not take, so a LIVE device has a disk and the
// close aborts the I/O that START_DEV is waiting for.
func (q *queue) holdUntilSafe() {
	done := q.dev.startDone
	if isClosed(done) {
		return
	}

	stop := make(chan struct{})

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		select {
		case <-done:
			q.wake()
		case <-stop:
		}
	}()

	err := q.serveEIO(done)

	close(stop)
	wg.Wait()

	if err == nil {
		return
	}

	q.dev.cfg.Logger.Warn("ublk: failed queue cannot serve during startup", "queue", q.id, "err", err)

	live, closeLive := q.dev.liveFunc()
	defer closeLive()

	waitStartupOrLive(done, live, brokenHoldTimeout, q.dev.cfg.Logger.Warn)
}

// serveEIO answers every request with EIO until done is closed or the
// queue is stopped. It returns an error if the ring stops working.
func (q *queue) serveEIO(done <-chan struct{}) error {
	eio := -int32(syscall.EIO)

	for {
		for {
			c, ok := q.ring.peekCQE()
			if !ok {
				break
			}

			q.ring.advanceCQ()

			if c.UserData == pollUserData {
				q.pollP = false
				continue
			}

			switch c.Res {
			case 0:
				if err := q.prepIO(uIOCommitAndFetchReq, uint16(c.UserData), eio); err != nil {
					return err
				}
			case ublkIOResAbort:
			default:
				return fmt.Errorf("io command tag %d: %w", c.UserData, syscall.Errno(-c.Res))
			}
		}

		if isClosed(done) || q.stopping.Load() {
			return nil
		}

		// Handler results that were already in flight are committed as
		// they are; the queue is failing either way.
		if err := q.drain(); err != nil {
			return err
		}

		if !q.pollP {
			if err := q.armPoll(); err != nil {
				return err
			}
		}

		if err := q.ring.submitAndWait(1); err != nil {
			return err
		}
	}
}

// liveFunc returns a probe that reports whether the device is LIVE. It
// uses its own control channel because Start holds d.ctrl in START_DEV.
// The returned close function releases that channel.
func (d *Device) liveFunc() (func() (bool, error), func()) {
	if testHooks.live != nil {
		return testHooks.live, func() {}
	}

	var c *ctrl

	closeFn := func() {
		if c == nil {
			return
		}

		if err := c.close(); err != nil {
			d.cfg.Logger.Warn("ublk: closing state probe control channel", "err", err)
		}
	}

	return func() (bool, error) {
		if c == nil {
			var err error
			if c, err = openCtrl(); err != nil {
				return false, err
			}
		}

		var info ctrlDevInfo

		res, err := c.doPayload(uCmdGetDevInfo, uint32(d.id), 0, nil, structBytes(&info))
		if err == nil {
			err = resErr("GET_DEV_INFO", res)
		}

		if err != nil {
			return false, err
		}

		return info.State == ublkSDevLive, nil
	}, closeFn
}

// waitStartupOrLive waits until done is closed, live reports true or
// timeout passes. Probe errors are not fatal: done or the timeout still
// end the wait. It reports why it returned.
func waitStartupOrLive(done <-chan struct{}, live func() (bool, error), timeout time.Duration, warn func(string, ...any)) string {
	deadline := time.After(timeout)
	tick := time.NewTicker(livePollInterval)

	defer tick.Stop()

	var lastErr error

	for {
		if ok, err := live(); err != nil {
			lastErr = err
		} else if ok {
			return "live"
		}

		select {
		case <-done:
			return "resolved"
		case <-deadline:
			warn("ublk: gave up waiting for startup before closing failed queue", "err", lastErr)
			return "timeout"
		case <-tick.C:
		}
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}
