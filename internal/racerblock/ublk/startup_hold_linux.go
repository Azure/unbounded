// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"
)

// ublkSDevLive is UBLK_S_DEV_LIVE.
const ublkSDevLive = 1

// Reasons a failed queue stopped holding its ring.
const (
	holdStopped   = "stopped"   // served EIO until the device was stopped
	holdResolved  = "resolved"  // startup resolved
	holdPublished = "published" // disk LIVE and registered with the block layer
)

var (
	// brokenHoldWarnAfter is how long a broken queue holds before it
	// logs that startup is taking unusually long. It keeps holding.
	brokenHoldWarnAfter = 30 * time.Second
	// probeInterval is how often a broken queue probes the device.
	probeInterval = 5 * time.Millisecond
)

// errServable marks a queue failure that loses no request the kernel
// handed to the queue, so the ring can still answer I/O with EIO.
type errServable struct{ error }

func (e errServable) Unwrap() error { return e.error }

// holdUntilSafe keeps a failed queue answering I/O with EIO until
// teardown stops it. Only if it cannot, it holds the ring until closing
// it cannot race START_DEV.
//
// On Linux 7.0, closing the ring cancels the queue's pending commands
// but does not complete a request the kernel already handed to the
// queue. Such requests are only aborted when the char device is
// released (ublk_ch_release_work_fn, ublk_drv.c v7.0 2474-2476), and
// teardown keeps it open until after STOP_DEV, whose del_gendisk
// waits for them (genhd.c v7.0 710). Linux 6.8 aborted them from the
// cancel path (ublk_abort_requests, ublk_drv.c v6.8 1451). Linux 7.0
// also scans partitions from a work item that holds the disk's
// open_mutex and may still be reading after START_DEV returns
// (ublk_drv.c v7.0 2368-2370, 4452), so a failed queue can receive
// requests after startup resolves.
//
// When err lost no request (errServable), the queue answers every
// request with EIO on its own ring until teardown stops it after
// STOP_DEV, like a healthy queue. Otherwise, or if that fails, a rescue
// ring answers them instead, including any request already handed to
// the queue; see rescue. Only if the rescue fails too does the queue
// stop answering I/O. It then closes its ring once that cannot race
// START_DEV: before START_DEV has published the disk, Linux 7.0's
// cancel path returns early without marking the queue canceling
// (ublk_start_cancel), so I/O that START_DEV later queues can reach a
// completed command. Once the disk exists, cancel marks the queue
// canceling and aborts new I/O. See waitSafe. On Linux 7.0 a request
// already handed to such a queue still blocks STOP_DEV, so teardown
// hangs; that needs the rescue ring itself to fail, for example
// because io_uring_setup fails.
func (q *queue) holdUntilSafe(err error) string {
	if errors.As(err, new(errServable)) {
		err = q.serveEIO()
		if err == nil {
			return holdStopped
		}
	}

	rerr := q.rescue()
	if rerr == nil {
		return holdRescued
	}

	if l := q.dev.cfg.Logger; l != nil {
		l.Error("ublk: failed queue cannot answer I/O; a request handed to it may block teardown",
			"queue", q.id, "err", err, "rescueErr", rerr)
	}

	done := q.dev.startDone
	if isClosed(done) {
		return holdResolved
	}

	q.dev.cfg.Logger.Warn("ublk: failed queue cannot serve during startup", "queue", q.id, "err", err)

	probe, closeProbe := q.dev.publishedProbe()
	defer closeProbe()

	return waitSafe(done, probe, brokenHoldWarnAfter, q.dev.cfg.Logger.Warn)
}

// markServing tells teardown that the queue answers I/O until stopped.
func (q *queue) markServing() {
	q.servingOnce.Do(func() {
		if q.serving != nil {
			close(q.serving)
		}
	})
}

// serveEIO answers every request with EIO until the queue is stopped.
// It submits every prepared answer before it returns, so no request is
// left with the queue. It returns an error if the ring stops working or
// a request may have been lost.
func (q *queue) serveEIO() error {
	if testHooks.serveEIO != nil {
		if err := testHooks.serveEIO(q); err != nil {
			return err
		}
	}

	// Teardown may now run STOP_DEV, which relies on this loop to answer
	// I/O. If the loop fails later, STOP_DEV may already be waiting for
	// a request this loop was handed, so holdUntilSafe falls back to a
	// rescue ring that answers it.
	q.markServing()

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

			if c.UserData >= uint64(q.depth) {
				return fmt.Errorf("kernel returned out-of-range tag %d", c.UserData)
			}

			switch c.Res {
			case 0:
				if err := q.delivered(uint16(c.UserData), true); err != nil {
					return err
				}

				if err := q.prepIO(uIOCommitAndFetchReq, uint16(c.UserData), eio); err != nil {
					return err
				}

				if testHooks.eioAnswered != nil {
					testHooks.eioAnswered(q)
				}
			case ublkIOResAbort:
			default:
				return fmt.Errorf("io command tag %d: %w", c.UserData, syscall.Errno(-c.Res))
			}
		}

		if testHooks.eioBeforeDrain != nil {
			testHooks.eioBeforeDrain(q)
		}

		// Handler results that were already in flight are committed as
		// they are; the queue is failing either way.
		if err := q.drain(); err != nil {
			return err
		}

		// Check for stop only after drain has cleared the eventfd. A
		// stop that drain consumed is seen here, and a later one wakes
		// the poll armed below. Checking before drain could lose the
		// only wake and block in submitAndWait.
		if q.stopping.Load() {
			return q.ring.submitAndWait(0)
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

// publishedProbe returns a probe that reports whether the device's disk
// is positively published, and a function that releases the probe.
//
// Published means GET_DEV_INFO reports LIVE and the kernel has
// registered /sys/class/block/ublkb<ID>. In START_DEV, both 6.8 and 7.0
// set ub->ub_disk before LIVE and before add_disk (ublk_drv.c v6.8
// 2235/2246/2254, v7.0 4413/4430/4438). add_disk registers the disk
// device in sysfs before it scans partitions (genhd.c v6.8 device_add
// 458, disk_scan_partitions 512; v7.0 device_add 496, scan 416, which
// ublk also defers to a work item). GET_DEV_INFO copies the state
// without a lock, so LIVE alone is not a publication barrier; the sysfs
// entry is created by the kernel after ub_disk is set, through locked
// device registration. Our char device stays open, so the id cannot
// belong to another device and the sysfs entry cannot be stale.
//
// The probe uses its own control channel because Start holds d.ctrl in
// START_DEV.
func (d *Device) publishedProbe() (func() (bool, error), func()) {
	if testHooks.published != nil {
		return testHooks.published, func() {}
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

	sysPath := "/sys/class/block/ublkb" + strconv.Itoa(d.id)

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

		return diskPublished(info.State, sysPath)
	}, closeFn
}

// diskPublished reports whether a device in state with block device
// sysfs entry sysPath has a published disk. Both are required.
func diskPublished(state uint16, sysPath string) (bool, error) {
	if state != ublkSDevLive {
		return false, nil
	}

	return sysfsPresent(sysPath)
}

func sysfsPresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, err
}

// waitSafe waits until done is closed or probe positively reports that
// the disk is published. Probe errors and timing never end the wait:
// closing the ring before the disk is published can corrupt kernel
// memory on Linux 7.0, so when the ring is broken and the control device
// or sysfs keeps failing, the queue keeps its resources until startup
// resolves, which may be never if the kernel cannot complete START_DEV.
// It logs once after warnAfter.
func waitSafe(done <-chan struct{}, probe func() (bool, error), warnAfter time.Duration, warn func(string, ...any)) string {
	warnC := time.After(warnAfter)
	tick := time.NewTicker(probeInterval)

	defer tick.Stop()

	var lastErr error

	for {
		if ok, err := probe(); err != nil {
			lastErr = err
		} else if ok {
			return holdPublished
		}

		select {
		case <-done:
			return holdResolved
		case <-warnC:
			warn("ublk: failed queue still waiting for startup before closing its ring", "after", warnAfter, "err", lastErr)
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
