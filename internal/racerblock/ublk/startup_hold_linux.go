// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ublkSDevLive is UBLK_S_DEV_LIVE.
const ublkSDevLive = 1

// Reasons a failed queue stopped holding its ring.
const (
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

// holdUntilSafe keeps a failed queue's ring open until closing it cannot
// race START_DEV, and returns why it stopped holding.
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
// When err lost no request (errServable), the queue answers every
// request with EIO until startup is resolved. Otherwise, or if that
// fails, a request may be stuck on this queue and the queue waits until
// startup is resolved or the disk is positively published; see
// waitSafe.
func (q *queue) holdUntilSafe(err error) string {
	done := q.dev.startDone
	if isClosed(done) {
		return holdResolved
	}

	if errors.As(err, new(errServable)) {
		err = q.holdServing(done)
		if err == nil {
			return holdResolved
		}
	}

	q.dev.cfg.Logger.Warn("ublk: failed queue cannot serve during startup", "queue", q.id, "err", err)

	probe, closeProbe := q.dev.publishedProbe()
	defer closeProbe()

	return waitSafe(done, probe, brokenHoldWarnAfter, q.dev.cfg.Logger.Warn)
}

// holdServing runs serveEIO and wakes it when done is closed.
func (q *queue) holdServing(done <-chan struct{}) error {
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

	return err
}

// serveEIO answers every request with EIO until done is closed or the
// queue is stopped. It returns an error if the ring stops working or a
// request may have been lost.
func (q *queue) serveEIO(done <-chan struct{}) error {
	if testHooks.serveEIO != nil {
		if err := testHooks.serveEIO(q); err != nil {
			return err
		}
	}

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
