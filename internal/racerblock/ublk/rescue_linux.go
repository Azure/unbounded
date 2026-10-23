// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ublk

import (
	"errors"
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

// errNoSpareRing reports that a queue has no rescue ring left, because
// it was never reserved or a rescue already used it.
var errNoSpareRing = errors.New("no spare rescue ring")

// rescueRingEntries is the submission queue size of a rescue ring. A
// rescue may prepare one sweep commit per tag, then one commit per tag
// from the replaced ring and from handlers, before it submits; the ring
// flushes when full, so the size only limits batching.
func rescueRingEntries(depth int) uint32 {
	return uint32(2*depth + 2) //nolint:gosec // depth is at most MaxQueueDepth
}

// newRescueRing reserves the rescue ring of q.
func newRescueRing(q *queue) (*uring, error) {
	if testHooks.spareRing != nil {
		if err := testHooks.spareRing(q); err != nil {
			return nil, err
		}
	}

	return newURing(rescueRingEntries(q.depth))
}

// rescue answers every request the queue owns, or is handed later,
// with EIO through the queue's spare ring until the queue is stopped.
// It is used when the queue's own ring can no longer answer I/O and
// a request may already have been handed to this process. It returns
// nil once the queue is stopped, or an error if there is no spare
// ring or the spare ring stops working.
//
// Why it is needed: on Linux 7.0, closing the server's ring does not
// complete a request already handed to the server. ublk_cancel_cmd
// skips owned ios (ublk_drv.c v7.0 2662-2700); only releasing the char
// device aborts them (ublk_ch_release_work_fn, 2474-2476). STOP_DEV's
// del_gendisk waits for such a request, so without an answer teardown
// blocks while the char device is open.
//
// Why another ring may answer: COMMIT_AND_FETCH_REQ checks only that
// the caller is the task that fetched the tag and that the server owns
// the io (ublk_ch_uring_cmd_local, v7.0 3312-3328 and 3342-3357).
// Linux 6.8 checks the same daemon task and an io that is owned and
// not active (__ublk_ch_uring_cmd, v6.8 1722-1734 and 1779-1803).
// Neither checks which ring the command came from. The queue goroutine
// is locked to its OS thread, so it is that task.
//
// The rescue commits EIO for every tag that no handler is serving. For
// a tag the kernel did not hand out, both versions reject the commit
// with -EBUSY or -EINVAL before touching the io, so the sweep needs no
// record of which tags are owned. Handler results are still committed
// through drain.
//
// The replaced ring stays open and is never entered again. Closing it
// would cancel its commands. On Linux 6.8 that aborts owned requests
// but leaves them marked owned (ublk_abort_queue, v6.8 1403-1423), so
// a later commit would complete a request twice. Keeping it open also
// keeps the startup guard in waitSafe intact: no ring is closed before
// teardown stops the queue. Requests the kernel hands to the old
// ring's fetch commands are read from its completion queue and
// answered through the rescue ring; see harvestOld.
func (q *queue) rescue() error {
	if testHooks.rescue != nil {
		if err := testHooks.rescue(q); err != nil {
			return err
		}
	}

	// Use the ring reserved at setup: a failure may be resource
	// exhaustion, so the rescue must not need a new file or mapping.
	r := q.spare
	if r == nil {
		return errNoSpareRing
	}

	q.spare = nil
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
// handed to the replaced ring. It only reads that ring's shared
// completion queue and never enters the ring.
//
// That is enough only if the completion queue never overflows: the
// kernel keeps overflowed completions aside and moves them back on a
// later io_uring_enter, which the rescue never makes on this ring. The
// queue has at most one io command per tag that it submitted and has
// not reaped: fetchAll submits one per tag, and a tag gets its next
// command only after the queue reaped the previous one's completion
// (handle and drain in serve, serveEIO). The eventfd poll adds one. So
// at most depth+1 completions can be waiting, and the ring is set up
// for queueRingEntries(depth) = depth+1 commands with a completion
// queue at least that large; TestQueueRingHoldsEveryCompletion checks
// that. Rescue commits go to the rescue ring, which it does enter.
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
