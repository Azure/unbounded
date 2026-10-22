// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"fmt"
	"syscall"
	"time"
)

// holdRescued is the hold reason of a queue that answered I/O with EIO
// through a rescue ring until it was stopped.
const holdRescued = "rescued"

// rescueTick is how often a rescue looks for requests the kernel handed
// to the replaced ring.
var rescueTick = 5 * time.Millisecond

// rescue answers every request the queue owns, or will be handed, with
// EIO through a fresh io_uring, until the queue is stopped. It is used
// when the queue's ring can no longer answer I/O and a request may
// already have been handed to this process. It returns nil once the
// queue was stopped, or an error if the rescue ring could not be set up
// or stopped working.
//
// Linux 7.0 does not complete a request that was handed to the server
// when the server's ring is closed (ublk_cancel_cmd skips owned ios,
// ublk_drv.c v7.0 2662-2700); only releasing the char device aborts it
// (ublk_ch_release_work_fn 2474-2476). STOP_DEV's del_gendisk waits for
// it, so without an answer teardown blocks while the char device is
// open. Committing it from another ring is allowed: COMMIT_AND_FETCH_REQ
// only requires the calling task to be the one that fetched the tag and
// the io to be owned by the server (ublk_ch_uring_cmd_local, v7.0
// 3312-3328, 3342-3357), and Linux 6.8 requires the same daemon task and
// an io that is not active and is owned (__ublk_ch_uring_cmd, v6.8
// 1722-1734, 1779-1803). Neither checks which ring the command came from. The queue thread
// is locked to its OS thread, so it is that task.
//
// The rescue therefore commits EIO for every tag that no handler is
// serving. The kernel rejects a commit for a tag it did not hand out
// (-EBUSY or -EINVAL on both versions, same lines) before touching the
// io, so the
// sweep needs no record of which tags are owned. Handler results are
// still committed through drain.
//
// The replaced ring stays open and is never entered again. Closing it
// would cancel its commands; on Linux 6.8 that aborts owned requests
// (ublk_abort_queue, v6.8 1403-1423) but leaves them marked owned, so a
// later commit would complete an already completed request. Keeping it
// open also keeps the startup guard in waitSafe intact: no ring is
// closed before teardown stops the queue. Requests the kernel hands to
// the old ring's fetch commands are read from its completion queue,
// which needs no system call, and answered through the rescue ring.
func (q *queue) rescue() error {
	if testHooks.rescue != nil {
		if err := testHooks.rescue(q); err != nil {
			return err
		}
	}

	r, err := newURing(uint32(2*q.depth + 2)) //nolint:gosec // depth is at most MaxQueueDepth
	if err != nil {
		return fmt.Errorf("rescue ring: %w", err)
	}

	q.oldRing, q.ring = q.ring, r
	q.pollP = false

	eio := -int32(syscall.EIO)

	if err := q.harvestOld(eio); err != nil {
		return err
	}

	if err := q.sweep(eio); err != nil {
		return err
	}

	if err := q.ring.submitAndWait(0); err != nil {
		return err
	}

	// Every request the queue owned and no handler serves has now been
	// answered, so teardown may issue STOP_DEV.
	q.markServing()

	if testHooks.rescued != nil {
		testHooks.rescued(q)
	}

	// The ticker wakes the queue through its eventfd, which release
	// closes, so rescue waits for the ticker to exit before returning.
	stopTick := make(chan struct{})
	tickDone := make(chan struct{})

	defer func() {
		close(stopTick)
		<-tickDone
	}()

	go func() {
		defer close(tickDone)

		tick := time.NewTicker(rescueTick)
		defer tick.Stop()

		for {
			select {
			case <-stopTick:
				return
			case <-tick.C:
				q.wake()
			}
		}
	}()

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

			// Any other result is an abort or a rejected sweep commit
			// for a tag the server did not own; nothing is pending.
			if c.Res == 0 {
				if err := q.prepIO(uIOCommitAndFetchReq, uint16(c.UserData), eio); err != nil {
					return err
				}
			}
		}

		if err := q.harvestOld(eio); err != nil {
			return err
		}

		if err := q.drain(); err != nil {
			return err
		}

		// As in serveEIO, check for stop only after drain cleared the
		// eventfd.
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

// harvestOld answers, through the rescue ring, every request the kernel
// handed to the replaced ring. It only reads the replaced ring's shared
// completion queue. That queue holds twice as many entries as the ring
// has commands (IORING_SETUP default), so it cannot overflow.
func (q *queue) harvestOld(res int32) error {
	if q.oldRing == nil {
		return nil
	}

	for {
		c, ok := q.oldRing.peekCQE()
		if !ok {
			return nil
		}

		q.oldRing.advanceCQ()

		if c.Res != 0 || c.UserData >= uint64(q.depth) {
			continue
		}

		if err := q.prepIO(uIOCommitAndFetchReq, uint16(c.UserData), res); err != nil {
			return err
		}
	}
}

// sweep prepares a commit of res for every tag no handler is serving.
func (q *queue) sweep(res int32) error {
	for t := range q.depth {
		if t < len(q.busy) && q.busy[t] {
			continue
		}

		if err := q.prepIO(uIOCommitAndFetchReq, uint16(t), res); err != nil {
			return err
		}
	}

	return nil
}
